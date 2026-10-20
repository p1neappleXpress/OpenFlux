package avito

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

var baseURL = "https://code.avito.ru"

const (
	avitoUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:155.0) Gecko/20100101 Firefox/155.0"
	shareDBType = "http://sharejs.org/types/textv1"
)

type AvitoConfig struct {
	WSHandshakeTimeout  time.Duration
	WSReadTimeout       time.Duration
	WSWriteTimeout      time.Duration
	ReconnectMinDelay   time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectMultiplier float64
	HTTPTimeout         time.Duration

	// ChunkBytes is how many raw bytes ride in one ShareDB op (delete+insert).
	// 256 KB is the bench-calibrated optimum: RTT grows sub-linearly with size,
	// so bigger = more throughput per op, up to ~256 KB where RTT plateaus at
	// ~355 ms giving ~540 KB/s per room. 512 KB doubles RTT for no gain.
	ChunkBytes int

	SendQueueSize int

	ReadBufferSize  int
	WriteBufferSize int
	MaxMessageBytes int64

	NumRooms        int
	RoomCreatePause time.Duration

	StatsInterval time.Duration
}

func DefaultAvitoConfig() AvitoConfig {
	return AvitoConfig{
		WSHandshakeTimeout:  15 * time.Second,
		WSReadTimeout:       90 * time.Second,
		WSWriteTimeout:      15 * time.Second,
		ReconnectMinDelay:   100 * time.Millisecond,
		ReconnectMaxDelay:   10 * time.Second,
		ReconnectMultiplier: 1.3,
		HTTPTimeout:         30 * time.Second,

		ChunkBytes:    256 * 1024,
		SendQueueSize: 256,

		ReadBufferSize:  256 << 10,
		WriteBufferSize: 256 << 10,
		MaxMessageBytes: 8 << 20,

		NumRooms:        4,
		RoomCreatePause: 300 * time.Millisecond,

		StatsInterval: 5 * time.Second,
	}
}

var (
	errRoomGone = errors.New("комната не существует")
	errRefused  = errors.New("refused")
	errNoRoom   = errors.New("avito: нет подключённой комнаты")
	errStopped  = errors.New("avito: транспорт остановлен")
)

func createRoom(ctx context.Context, cookies string) (string, error) {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(baseURL)
	parseCookiesInto(jar, u, cookies)

	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL+"/web/1/code/room", bytes.NewReader(nil))
	req.Header.Set("User-Agent", avitoUA)
	req.Header.Set("Origin", baseURL)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST room: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("POST room status %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var result struct {
		Room struct {
			ID string `json:"id"`
		} `json:"room"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse room: %w", err)
	}
	if result.Room.ID == "" {
		return "", fmt.Errorf("empty room id")
	}
	return result.Room.ID, nil
}

func parseCookiesInto(jar http.CookieJar, u *url.URL, raw string) {
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			jar.SetCookies(u, []*http.Cookie{{
				Name: part[:eq], Value: part[eq+1:], Path: "/",
			}})
		}
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// ---- per-room state ----

type avitoWS struct {
	idx     int
	roomID  string
	cookies string
	config  AvitoConfig
	ctx     context.Context

	conn    *websocket.Conn
	writeMu sync.Mutex
	connID  string
	ver     int
	docLen  int
	seq     int

	connected atomic.Bool
	onData    func([]byte)
	sendQueue chan []byte
	ackCh     chan map[string]interface{}
	stats     *channelStats
}

type channelStats struct {
	idx        int
	roomID     string
	opsSent    atomic.Uint64
	opsRecv    atomic.Uint64
	bytesSent  atomic.Uint64
	bytesRecv  atomic.Uint64
	reconnects atomic.Uint64
	errors     atomic.Uint64
}

func (w *avitoWS) dial() (*websocket.Conn, error) {
	wsURL := strings.Replace(baseURL, "https://", "wss://", 1) + "/sharedb"
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	header := http.Header{}
	header.Set("User-Agent", avitoUA)
	header.Set("Origin", baseURL)
	header.Set("Cookie", w.cookies)

	dialer := websocket.Dialer{
		NetDialContext:   netbind.DialContext,
		HandshakeTimeout: w.config.WSHandshakeTimeout,
		ReadBufferSize:   w.config.ReadBufferSize,
		WriteBufferSize:  w.config.WriteBufferSize,
	}
	conn, _, err := dialer.DialContext(w.ctx, wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	return conn, nil
}

func (w *avitoWS) writeJSON(v interface{}) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("ws not connected")
	}
	w.conn.SetWriteDeadline(time.Now().Add(w.config.WSWriteTimeout))
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := w.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		w.conn.Close()
		return err
	}
	return nil
}

func (w *avitoWS) readJSON(v interface{}) error {
	w.conn.SetReadDeadline(time.Now().Add(w.config.WSReadTimeout))
	_, raw, err := w.conn.ReadMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (w *avitoWS) subscribe() error {
	var init struct {
		ID string `json:"id"`
	}
	if err := w.readJSON(&init); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	w.connID = init.ID

	if err := w.writeJSON(map[string]interface{}{
		"a": "s", "c": "docs", "d": w.roomID, "v": nil,
	}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	var sub map[string]interface{}
	if err := w.readJSON(&sub); err != nil {
		return fmt.Errorf("subscribe reply: %w", err)
	}

	if errMsg, ok := sub["error"].(map[string]interface{}); ok {
		return fmt.Errorf("%w: subscribe: %v", errRefused, errMsg)
	}

	doc, _ := sub["data"].(map[string]interface{})
	if doc == nil {
		doc = sub
	}
	if v, ok := doc["v"].(float64); ok {
		w.ver = int(v)
	}
	data, _ := doc["data"].(string)
	w.docLen = len(data)
	w.seq = 1

	if doc["type"] == nil {
		if err := w.writeJSON(map[string]interface{}{
			"a": "op", "c": "docs", "d": w.roomID, "v": 0,
			"src": w.connID, "seq": w.seq,
			"create": map[string]interface{}{"type": shareDBType, "data": ""},
		}); err != nil {
			return fmt.Errorf("create doc: %w", err)
		}
		var reply map[string]interface{}
		if err := w.readJSON(&reply); err != nil {
			return fmt.Errorf("create reply: %w", err)
		}
		if errMsg := reply["error"]; errMsg != nil {
			return fmt.Errorf("create error: %v", errMsg)
		}
		w.ver = 1
		w.docLen = 0
		w.seq = 2
	}

	return nil
}

func (w *avitoWS) run() {
	delay := w.config.ReconnectMinDelay
	for w.ctx.Err() == nil {
		err := w.connectAndServe()
		w.connected.Store(false)
		if w.ctx.Err() != nil {
			return
		}
		utils.Debugf("[AVITO] ws error (%s): %v", w.roomID[:8], err)
		w.stats.reconnects.Add(1)
		if !sleepCtx(w.ctx, delay) {
			return
		}
		delay = min(time.Duration(float64(delay)*w.config.ReconnectMultiplier), w.config.ReconnectMaxDelay)
	}
}

func (w *avitoWS) connectAndServe() error {
	conn, err := w.dial()
	if err != nil {
		return err
	}
	unhook := context.AfterFunc(w.ctx, func() { conn.Close() })
	defer unhook()
	w.writeMu.Lock()
	w.conn = conn
	w.writeMu.Unlock()
	defer func() {
		w.writeMu.Lock()
		w.conn = nil
		w.writeMu.Unlock()
		conn.Close()
	}()

	conn.SetReadLimit(w.config.MaxMessageBytes)

	if err := w.subscribe(); err != nil {
		return err
	}
	w.connected.Store(true)
	w.ackCh = make(chan map[string]interface{}, 1)
	utils.Debugf("[AVITO] WS ready: %s ver=%d docLen=%d", w.roomID[:8], w.ver, w.docLen)

	// Single reader dispatches acks to sendLoop and peer ops to onData.
	readDone := make(chan error, 1)
	go func() {
		readDone <- w.readLoop()
	}()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- w.sendLoop()
	}()

	select {
	case err := <-readDone:
		return err
	case err := <-sendDone:
		return err
	case <-w.ctx.Done():
		return errStopped
	}
}

// readLoop is the sole reader on the WebSocket. It routes ack replies to
// the sendLoop's ackCh and delivers peer ops to onData.
func (w *avitoWS) readLoop() error {
	for {
		w.conn.SetReadDeadline(time.Now().Add(w.config.WSReadTimeout))
		_, raw, err := w.conn.ReadMessage()
		if err != nil {
			return err
		}
		var msg map[string]interface{}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		a, _ := msg["a"].(string)
		src, _ := msg["src"].(string)

		switch {
		case a == "op" && src != "" && src != w.connID:
			w.handlePeerOp(msg)
		default:
			// Ack for our op, or any other server reply.
			select {
			case w.ackCh <- msg:
			case <-w.ctx.Done():
				return errStopped
			}
		}
	}
}

func (w *avitoWS) handlePeerOp(msg map[string]interface{}) {
	ops, ok := msg["op"].([]interface{})
	if !ok || len(ops) == 0 {
		return
	}
	for _, op := range ops {
		s, ok := op.(string)
		if !ok || len(s) == 0 {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			continue
		}
		if w.onData != nil {
			w.onData(decoded)
		}
		w.stats.opsRecv.Add(1)
		w.stats.bytesRecv.Add(uint64(len(decoded)))
	}
	// Track the version from peer ops so our next send uses the right one.
	if v, ok := msg["v"].(float64); ok {
		newVer := int(v) + 1
		if newVer > w.ver {
			w.ver = newVer
		}
	}
}

func (w *avitoWS) sendLoop() error {
	for {
		select {
		case <-w.ctx.Done():
			return errStopped
		case data := <-w.sendQueue:
			if err := w.sendOp(data); err != nil {
				return err
			}
		}
	}
}

func (w *avitoWS) sendOp(data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)

	var op []interface{}
	if w.docLen > 0 {
		op = []interface{}{map[string]int{"d": w.docLen}, encoded}
	} else {
		op = []interface{}{encoded}
	}

	if err := w.writeJSON(map[string]interface{}{
		"a": "op", "c": "docs", "d": w.roomID,
		"v": w.ver, "src": w.connID, "seq": w.seq, "op": op,
	}); err != nil {
		return err
	}

	// Wait for the ack from readLoop.
	select {
	case reply := <-w.ackCh:
		if errMsg := reply["error"]; errMsg != nil {
			w.stats.errors.Add(1)
			return fmt.Errorf("op error: %v", errMsg)
		}
		if v, ok := reply["v"].(float64); ok {
			w.ver = int(v) + 1
		} else {
			w.ver++
		}
	case <-w.ctx.Done():
		return errStopped
	}

	w.seq++
	w.docLen = len(encoded)
	w.stats.opsSent.Add(1)
	w.stats.bytesSent.Add(uint64(len(data)))
	return nil
}

func (w *avitoWS) Send(data []byte) error {
	select {
	case w.sendQueue <- data:
		return nil
	case <-w.ctx.Done():
		return errStopped
	}
}

// ---- Transport ----

type AvitoTransport struct {
	*transport.BaseTransport

	cookies  string
	roomIDs  []string
	config   AvitoConfig
	isClient bool

	roomsMu    sync.Mutex
	roomList   string
	onRoomList func(packed string)

	ctx    context.Context
	cancel context.CancelFunc

	// txWss are rooms this side writes to; rxWss are rooms it reads from.
	// The room list packs exit-tx rooms first, then exit-rx rooms.
	// The exit writes to the first half, reads from the second;
	// the client writes to the second half, reads from the first.
	txWss []*avitoWS
	rxWss []*avitoWS
	rr    atomic.Uint32

	statsStart time.Time
}

func NewAvitoTransport(rawURL string, cfg transport.TransportConfig, cookies string, isClient bool) *AvitoTransport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &AvitoTransport{
		BaseTransport: transport.NewBaseTransport(cfg),
		config:        DefaultAvitoConfig(),
		cookies:       cookies,
		isClient:      isClient,
		ctx:           ctx,
		cancel:        cancel,
		statsStart:    time.Now(),
	}
	t.roomIDs = parseRoomList(rawURL)
	return t
}

func parseRoomList(rawURL string) []string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if u, err := url.Parse(rawURL); err == nil {
		q := u.Query()
		if packed := q.Get("rooms"); packed != "" {
			if ids, err := unpackRooms(packed); err == nil {
				return ids
			}
		}
	}
	if ids, err := unpackRooms(rawURL); err == nil {
		return ids
	}
	return nil
}

func packRooms(ids []string) string {
	raw, _ := json.Marshal(ids)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func unpackRooms(s string) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (t *AvitoTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	allIDs, err := t.enterRooms()
	if err != nil {
		return err
	}
	if t.ctx.Err() != nil {
		return errStopped
	}

	// Split rooms: first half = exit-tx (exit writes, client reads),
	// second half = exit-rx (client writes, exit reads).
	half := len(allIDs) / 2
	if half == 0 {
		half = 1
	}
	var txIDs, rxIDs []string
	if t.isClient {
		// Client writes to exit-rx rooms (second half).
		txIDs = allIDs[half:]
		rxIDs = allIDs[:half]
	} else {
		// Exit writes to exit-tx rooms (first half).
		txIDs = allIDs[:half]
		rxIDs = allIDs[half:]
	}

	mkWS := func(ids []string, onData func([]byte)) []*avitoWS {
		wss := make([]*avitoWS, len(ids))
		for i, id := range ids {
			ws := &avitoWS{
				idx:       i,
				roomID:    id,
				cookies:   t.cookies,
				config:    t.config,
				ctx:       t.ctx,
				sendQueue: make(chan []byte, t.config.SendQueueSize),
				stats:     &channelStats{idx: i, roomID: id},
			}
			ws.onData = onData
			wss[i] = ws
		}
		return wss
	}

	t.txWss = mkWS(txIDs, nil)
	t.rxWss = mkWS(rxIDs, t.CallReceive)

	for _, ws := range t.txWss {
		go ws.run()
	}
	for _, ws := range t.rxWss {
		go ws.run()
	}

	utils.Debugf("[AVITO] transport started: %d tx + %d rx rooms", len(t.txWss), len(t.rxWss))
	t.SetConnected(true)

	go t.statsLoop()
	return nil
}

func (t *AvitoTransport) enterRooms() ([]string, error) {
	if len(t.roomIDs) > 0 {
		utils.Infof("[AVITO] Avito: подключаемся к %d комнатам", len(t.roomIDs))
		t.setRoomList(t.roomIDs)
		return t.roomIDs, nil
	}
	if t.isClient {
		return nil, fmt.Errorf("avito client: --url must contain room list")
	}

	// Exit mode: create 2*NumRooms (half tx, half rx).
	total := t.config.NumRooms * 2
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		if t.ctx.Err() != nil {
			return nil, errStopped
		}
		var roomID string
		var lastErr error
		for attempt := 0; attempt < 5; attempt++ {
			roomID, lastErr = createRoom(t.ctx, t.cookies)
			if lastErr == nil {
				break
			}
			wait := t.config.RoomCreatePause * time.Duration(1<<uint(attempt))
			if wait > 15*time.Second {
				wait = 15 * time.Second
			}
			utils.Debugf("[AVITO] room %d attempt %d failed: %v (wait %v)", i+1, attempt+1, lastErr, wait)
			if !sleepCtx(t.ctx, wait) {
				return nil, errStopped
			}
		}
		if roomID == "" {
			utils.Debugf("[AVITO] room %d skipped: %v", i+1, lastErr)
			continue
		}
		ids = append(ids, roomID)
		utils.Debugf("[AVITO] created room %d/%d: %s", i+1, total, roomID[:8])
		if i < total-1 {
			if !sleepCtx(t.ctx, t.config.RoomCreatePause) {
				return nil, errStopped
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("could not create any room")
	}

	fmt.Printf("\n=== COPY THIS TO CLIENT ===\n")
	fmt.Printf("%s\n", packRooms(ids))
	fmt.Printf("===========================\n")
	fmt.Printf("Save it and pass it back as --url to reuse these rooms after a restart.\n\n")
	t.setRoomList(ids)
	return ids, nil
}

func (t *AvitoTransport) RoomList() string {
	t.roomsMu.Lock()
	defer t.roomsMu.Unlock()
	return t.roomList
}

func (t *AvitoTransport) OnRoomList(f func(packed string)) {
	t.roomsMu.Lock()
	t.onRoomList = f
	t.roomsMu.Unlock()
}

func (t *AvitoTransport) setRoomList(ids []string) {
	packed := packRooms(ids)
	t.roomsMu.Lock()
	changed := packed != t.roomList
	t.roomList = packed
	f := t.onRoomList
	t.roomsMu.Unlock()
	if changed && f != nil {
		f(packed)
	}
}

func (t *AvitoTransport) Stop() error {
	t.cancel()
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

func (t *AvitoTransport) Send(data []byte) error {
	ws := t.pickTxRoom()
	if ws == nil {
		return errNoRoom
	}
	return ws.Send(data)
}

func (t *AvitoTransport) pickTxRoom() *avitoWS {
	n := len(t.txWss)
	if n == 0 {
		return nil
	}
	start := int(t.rr.Add(1))
	for i := 0; i < n; i++ {
		ws := t.txWss[(start+i)%n]
		if ws.connected.Load() {
			return ws
		}
	}
	return nil
}

func (t *AvitoTransport) Receive(callback func([]byte)) {
	t.BaseTransport.Receive(callback)
}

func (t *AvitoTransport) IsConnected() bool {
	if !t.BaseTransport.IsConnected() {
		return false
	}
	for _, ws := range t.txWss {
		if ws.connected.Load() {
			return true
		}
	}
	for _, ws := range t.rxWss {
		if ws.connected.Load() {
			return true
		}
	}
	return false
}

func (t *AvitoTransport) allWS() []*avitoWS {
	out := make([]*avitoWS, 0, len(t.txWss)+len(t.rxWss))
	out = append(out, t.txWss...)
	out = append(out, t.rxWss...)
	return out
}

func (t *AvitoTransport) Stats() transport.TransportStats {
	var sent, recv, bytesSent, bytesRecv, reconnects uint64
	for _, ws := range t.allWS() {
		sent += ws.stats.opsSent.Load()
		recv += ws.stats.opsRecv.Load()
		bytesSent += ws.stats.bytesSent.Load()
		bytesRecv += ws.stats.bytesRecv.Load()
		reconnects += ws.stats.reconnects.Load()
	}
	base := t.BaseTransport.Stats()
	return transport.TransportStats{
		BytesSent:     bytesSent,
		BytesReceived: bytesRecv,
		PacketsSent:   sent,
		PacketsRecv:   recv,
		Reconnects:    reconnects,
		Connected:     t.IsConnected(),
		Uptime:        base.Uptime,
	}
}

func (t *AvitoTransport) statsLoop() {
	tick := time.NewTicker(t.config.StatsInterval)
	defer tick.Stop()

	all := t.allWS()
	lastBytesSent := make([]uint64, len(all))
	lastBytesRecv := make([]uint64, len(all))
	var lastTotalBS, lastTotalBR uint64

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tick.C:
			var totalBS, totalBR uint64
			for i, ws := range all {
				bs := ws.stats.bytesSent.Load()
				br := ws.stats.bytesRecv.Load()
				dBS := bs - lastBytesSent[i]
				dBR := br - lastBytesRecv[i]
				lastBytesSent[i] = bs
				lastBytesRecv[i] = br
				totalBS += bs
				totalBR += br

				dir := "tx"
				if i >= len(t.txWss) {
					dir = "rx"
				}
				utils.Debugf("[AVITO-%s-%02d %s] tx=%.1f KB/s  rx=%.1f KB/s  err=%d  reconn=%d  conn=%v",
					dir, i, ws.roomID[:8],
					float64(dBS)/t.config.StatsInterval.Seconds()/1024,
					float64(dBR)/t.config.StatsInterval.Seconds()/1024,
					ws.stats.errors.Load(),
					ws.stats.reconnects.Load(),
					ws.connected.Load(),
				)
			}
			dtBS := totalBS - lastTotalBS
			dtBR := totalBR - lastTotalBR
			lastTotalBS = totalBS
			lastTotalBR = totalBR

			utils.Debugf("[AVITO-TOTAL] tx=%.1f KB/s  rx=%.1f KB/s  uptime=%v",
				float64(dtBS)/t.config.StatsInterval.Seconds()/1024,
				float64(dtBR)/t.config.StatsInterval.Seconds()/1024,
				time.Since(t.statsStart).Round(time.Second),
			)
		}
	}
}

// CreateRoomList creates new rooms for the wizard (2*NumRooms: half tx, half rx).
func CreateRoomList(ctx context.Context, cookies string) (string, error) {
	cfg := DefaultAvitoConfig()
	total := cfg.NumRooms * 2
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id, err := createRoom(ctx, cookies)
		if err != nil {
			if len(ids) > 0 {
				break
			}
			return "", err
		}
		ids = append(ids, id)
		if i < total-1 {
			if !sleepCtx(ctx, cfg.RoomCreatePause) {
				return "", errStopped
			}
		}
	}
	return packRooms(ids), nil
}

// FetchCookies returns the cookies this transport was started with.
func (t *AvitoTransport) FetchCookies() (map[string]string, error) {
	out := make(map[string]string)
	for _, part := range strings.Split(t.cookies, ";") {
		part = strings.TrimSpace(part)
		if eq := strings.IndexByte(part, '='); eq > 0 {
			out[part[:eq]] = part[eq+1:]
		}
	}
	return out, nil
}

func (t *AvitoTransport) ApplyCookies(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	parts := make([]string, 0, len(values))
	for k, v := range values {
		parts = append(parts, k+"="+v)
	}
	t.cookies = strings.Join(parts, "; ")
	for _, ws := range t.allWS() {
		ws.cookies = t.cookies
		ws.writeMu.Lock()
		if ws.conn != nil {
			ws.conn.Close()
		}
		ws.writeMu.Unlock()
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
