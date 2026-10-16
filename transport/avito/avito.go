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
	"os"
	"strconv"
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
	avitoUA     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"
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

// envKB reads a positive integer knob (the name is historic: chunk sizes
// are in KB); def when unset or invalid.
func envKB(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
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

		ChunkBytes:    envKB("AVITO_CHUNK_KB", 256) * 1024,
		SendQueueSize: 256,

		ReadBufferSize:  256 << 10,
		WriteBufferSize: 256 << 10,
		MaxMessageBytes: 8 << 20,

		NumRooms:        envKB("AVITO_ROOMS", 8), // room PAIRS = stripe lanes
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
	stats     *channelStats

	// Striped wire (see stripe.go): an op carries a run of framed pieces
	// ([len u32][piece]...) instead of one packet; pull, when set, makes the
	// room a lane's send side, and wake says the stripe has something.
	framed bool
	pull   atomic.Pointer[func(max int) [][]byte]
	wake   chan struct{}
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
	utils.Debugf("[AVITO] WS ready: %s ver=%d docLen=%d", w.roomID[:8], w.ver, w.docLen)

	// Single reader dispatches acks to sendLoop and peer ops to onData. Both
	// loops belong to this socket: done ends them with it, and the send loop
	// is waited for, so a loop from a dead socket never takes the next one's
	// acks or writes into it with a stale version.
	c := &avitoConn{done: make(chan struct{}), ack: make(chan map[string]interface{}, 64)}
	readDone := make(chan error, 1)
	go func() {
		readDone <- w.readLoop(conn, c)
	}()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- w.sendLoop(c)
	}()

	readEnded, sendEnded := false, false
	select {
	case err = <-readDone:
		readEnded = true
	case err = <-sendDone:
		sendEnded = true
	case <-w.ctx.Done():
		err = errStopped
	}
	close(c.done)
	w.connected.Store(false)
	conn.Close()
	if !sendEnded {
		<-sendDone
	}
	// The reader too: the deferred w.conn = nil must not land under it, and
	// it must never go on to read the next socket. conn.Close above makes
	// its ReadMessage fail at once.
	if !readEnded {
		<-readDone
	}
	return err
}

// avitoConn is one socket's lifetime: done closes when it ends, ack carries
// the server's replies to the send loop.
type avitoConn struct {
	done chan struct{}
	ack  chan map[string]interface{}
}

// readLoop is the sole reader on the WebSocket. It routes ack replies to
// the sendLoop's ackCh and delivers peer ops to onData. It reads its own
// socket, not w.conn, which the next connection replaces (or nils).
func (w *avitoWS) readLoop(conn *websocket.Conn, c *avitoConn) error {
	for {
		conn.SetReadDeadline(time.Now().Add(w.config.WSReadTimeout))
		_, raw, err := conn.ReadMessage()
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
			case c.ack <- msg:
			case <-c.done:
				return errStopped
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
			if w.framed {
				splitFramed(decoded, w.onData)
			} else {
				w.onData(decoded)
			}
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

func (w *avitoWS) sendLoop(c *avitoConn) error {
	if src := w.pull.Load(); src != nil {
		return w.pullLoop(c, *src)
	}
	for {
		select {
		case <-w.ctx.Done():
			return errStopped
		case <-c.done:
			return errStopped
		case data := <-w.sendQueue:
			if err := w.sendOp(c, data); err != nil {
				return err
			}
		}
	}
}

func (w *avitoWS) sendOp(c *avitoConn, data []byte) error {
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
	case reply := <-c.ack:
		if errMsg := reply["error"]; errMsg != nil {
			w.stats.errors.Add(1)
			return fmt.Errorf("op error: %v", errMsg)
		}
		if v, ok := reply["v"].(float64); ok {
			w.ver = int(v) + 1
		} else {
			w.ver++
		}
	case <-c.done:
		return errStopped
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
	// striped: the room pairs are the lanes of a stripe (the list says so,
	// or an exit creates new rooms); otherwise packets go round-robin.
	striped bool
	stripe  atomic.Pointer[transport.StripedTransport]

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
	t.roomIDs, t.striped = parseRooms(rawURL)
	if len(t.roomIDs) == 0 && !isClient {
		t.striped = stripeNewRooms()
	}
	return t
}

// stripeNewRooms: rooms an exit creates now get the striped list, unless
// AVITO_STRIPE=0 asks for the old wire (peers from 0.7.0).
func stripeNewRooms() bool { return os.Getenv("AVITO_STRIPE") != "0" }

// parseRooms pulls the room ids out of --url (the packed list, bare or as
// ?rooms=) and whether the list is in the striped format.
func parseRooms(rawURL string) ([]string, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, false
	}
	if u, err := url.Parse(rawURL); err == nil {
		if packed := u.Query().Get("rooms"); packed != "" {
			if ids, striped, err := transport.UnpackRooms(packed); err == nil {
				return ids, striped
			}
		}
	}
	if ids, striped, err := transport.UnpackRooms(rawURL); err == nil {
		return ids, striped
	}
	return nil, false
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
				framed:    t.striped,
				wake:      make(chan struct{}, 1),
			}
			ws.onData = onData
			wss[i] = ws
		}
		return wss
	}

	t.txWss = mkWS(txIDs, nil)
	t.rxWss = mkWS(rxIDs, t.CallReceive)
	if t.striped {
		// Wires the pairs in as lanes before the rooms start, so their send
		// loops come up pulling.
		if err := t.startStripe(); err != nil {
			return err
		}
	}

	for _, ws := range t.txWss {
		go ws.run()
	}
	for _, ws := range t.rxWss {
		go ws.run()
	}

	utils.Debugf("[AVITO] transport started: %d tx + %d rx rooms, striped=%v", len(t.txWss), len(t.rxWss), t.striped)
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
	fmt.Printf("%s\n", transport.PackRooms(ids, t.striped))
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
	packed := transport.PackRooms(ids, t.striped)
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
	if st := t.stripe.Load(); st != nil {
		_ = st.Stop()
	}
	return t.BaseTransport.Stop()
}

func (t *AvitoTransport) Send(data []byte) error {
	if st := t.stripe.Load(); st != nil {
		return st.Send(data)
	}
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
	return transport.PackRooms(ids, stripeNewRooms()), nil
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

// SetNumRooms sets how many rooms (avito: room pairs) an exit with no --url
// creates, before Start. A group of carriers gives each lane one.
func (t *AvitoTransport) SetNumRooms(n int) {
	if n > 0 {
		t.config.NumRooms = n
	}
}

// StripeOptions are the settings for a stripe over this carrier's rooms.
func StripeOptions() transport.StripeOptions { return stripeOptions }
