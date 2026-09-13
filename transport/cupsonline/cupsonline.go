package cupsonline

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/utils"
)

const (
	baseRoomURL = "https://interview.cups.online/live-coding/"

	cupsUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"
)

type CupsonlineConfig struct {
	WSHandshakeTimeout  time.Duration
	WSReadTimeout       time.Duration
	KeepAliveInterval   time.Duration
	ReconnectMinDelay   time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectMultiplier float64
	HTTPTimeout         time.Duration

	// Батчинг внутри одного WS. 32 KB — потолок Centrifugo (64 KB push).
	BatchMaxPackets int
	BatchMaxBytes   int
	BatchTimeout    time.Duration

	SendQueueSize int

	ReadBufferSize  int
	WriteBufferSize int

	MaxPayloadBytes int

	NumRooms        int
	RoomCreatePause time.Duration

	StatsInterval time.Duration
}

func DefaultCupsonlineConfig() CupsonlineConfig {
	return CupsonlineConfig{
		WSHandshakeTimeout:  15 * time.Second,
		WSReadTimeout:       300 * time.Second,
		KeepAliveInterval:   20 * time.Second,
		ReconnectMinDelay:   100 * time.Millisecond,
		ReconnectMaxDelay:   10 * time.Second,
		ReconnectMultiplier: 1.3,
		HTTPTimeout:         30 * time.Second,

		// 32 KB — проверено probe'ом: column 49152 OK, 65536 CLOSED.
		// 32 KB батч → ~43 KB base64 → push ~43 KB < 64 KB лимита.
		BatchMaxPackets: 64,
		BatchMaxBytes:   32 * 1024,
		BatchTimeout:    2 * time.Millisecond,

		SendQueueSize: 65536,

		ReadBufferSize:  32 << 20,
		WriteBufferSize: 32 << 20,

		MaxPayloadBytes: 16_000_000,

		NumRooms:        4,
		RoomCreatePause: 500 * time.Millisecond,

		StatsInterval: 5 * time.Second,
	}
}

var (
	reMetaConnToken = regexp.MustCompile(`<meta[^>]+name="centrifuge-connection-token"[^>]+content="([^"]+)"`)
	reMetaConnURL   = regexp.MustCompile(`<meta[^>]+name="centrifuge-connection-url"[^>]+content="([^"]+)"`)
	reMetaSubURL    = regexp.MustCompile(`<meta[^>]+name="centrifuge-subscription-token-url"[^>]+content="([^"]+)"`)
	reDataRoomUUID  = regexp.MustCompile(`data-room="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;`)
	reDataUserUUID  = regexp.MustCompile(`data-user="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;`)
)

type cupsAuth struct {
	roomUUID   string
	userUUID   string
	connToken  string
	connURL    string
	subURL     string
	subToken   string
	channel    string
	httpClient *http.Client
	csrfToken  string
}

func authorize(roomURL string) (*cupsAuth, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, _ := http.NewRequest("GET", roomURL, nil)
	req.Header.Set("User-Agent", cupsUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET room: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET room status %d", resp.StatusCode)
	}

	html := string(body)
	a := &cupsAuth{
		roomUUID:   firstMatch(reDataRoomUUID, html),
		userUUID:   firstMatch(reDataUserUUID, html),
		connToken:  firstMatch(reMetaConnToken, html),
		connURL:    firstMatch(reMetaConnURL, html),
		subURL:     firstMatch(reMetaSubURL, html),
		httpClient: client,
	}
	for _, c := range jar.Cookies(mustParseURL(roomURL)) {
		if c.Name == "csrftoken" {
			a.csrfToken = c.Value
			break
		}
	}
	if a.roomUUID == "" || a.userUUID == "" {
		return nil, fmt.Errorf("room/user uuid missing")
	}
	if a.connToken == "" || a.connURL == "" || a.subURL == "" {
		return nil, fmt.Errorf("centrifuge meta missing")
	}
	if a.csrfToken == "" {
		return nil, fmt.Errorf("csrftoken missing")
	}
	a.channel = fmt.Sprintf("$shared_editor:room-%s", a.roomUUID)

	subBody, _ := json.Marshal(map[string]string{"channel": a.channel})
	req2, _ := http.NewRequest("POST", a.subURL, bytes.NewReader(subBody))
	req2.Header.Set("User-Agent", cupsUA)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-CSRFToken", a.csrfToken)
	req2.Header.Set("Origin", originOf(roomURL))
	req2.Header.Set("Referer", roomURL)

	resp2, err := client.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("POST sub token: %w", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return nil, fmt.Errorf("sub token status %d", resp2.StatusCode)
	}
	var subResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body2, &subResp); err != nil {
		return nil, err
	}
	a.subToken = subResp.Token
	if a.subToken == "" {
		return nil, fmt.Errorf("empty sub token")
	}
	utils.Debugf("[CUPS] auth OK: room=%s user=%s", a.roomUUID, a.userUUID)
	return a, nil
}

func createRooms(baseURL string, n int, pause time.Duration) ([]*cupsAuth, error) {
	if baseURL == "" {
		baseURL = baseRoomURL
	}
	out := make([]*cupsAuth, 0, n)
	delay := pause
	if delay <= 0 {
		delay = 150 * time.Millisecond
	}
	for i := 0; i < n; i++ {
		var a *cupsAuth
		var lastErr error
		for attempt := 0; attempt < 6; attempt++ {
			a, lastErr = authorize(baseURL)
			if lastErr == nil {
				break
			}
			msg := lastErr.Error()
			var wait time.Duration
			if strings.Contains(msg, "403") || strings.Contains(msg, "429") {
				wait = delay * time.Duration(1<<uint(attempt))
				if wait > 30*time.Second {
					wait = 30 * time.Second
				}
			} else {
				wait = delay * time.Duration(attempt+1)
			}
			utils.Debugf("[CUPS] room %d attempt %d failed: %v (wait %v)", i+1, attempt+1, lastErr, wait)
			<-time.After(wait)
		}
		if a == nil {
			utils.Debugf("[CUPS] room %d skipped: %v", i+1, lastErr)
			continue
		}
		out = append(out, a)
		utils.Debugf("[CUPS] created room %d/%d: %s", i+1, n, a.roomUUID)
		if i < n-1 {
			<-time.After(delay)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("could not create any room")
	}
	return out, nil
}

func packRooms(auths []*cupsAuth) string {
	ids := make([]string, 0, len(auths))
	for _, a := range auths {
		ids = append(ids, a.roomUUID)
	}
	raw, _ := json.Marshal(ids)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func unpackRooms(s string) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
	}
	return ids, nil
}

// ---- per-channel stats ----

type channelStats struct {
	idx         int
	roomUUID    string
	packetsSent atomic.Uint64
	packetsRecv atomic.Uint64
	bytesSent   atomic.Uint64
	bytesRecv   atomic.Uint64
	batchesSent atomic.Uint64
	batchesRecv atomic.Uint64
	reconnects  atomic.Uint64
	flows       atomic.Uint64
}

// ---- WS ----

type cupsWS struct {
	auth   *cupsAuth
	config CupsonlineConfig
	conn   *websocket.Conn

	writeMu sync.Mutex
	rpcID   atomic.Int64
	onData  func([]byte)

	ctx       chan struct{}
	closed    atomic.Bool
	connected atomic.Bool

	sendQueue chan []byte

	stats *channelStats
}

func (w *cupsWS) nextID() int64 { return w.rpcID.Add(1) }

func (w *cupsWS) writeRaw(data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("ws not connected")
	}
	return w.conn.WriteMessage(websocket.TextMessage, data)
}

func (w *cupsWS) writeJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeRaw(data)
}

func (w *cupsWS) run() {
	delay := w.config.ReconnectMinDelay
	for {
		select {
		case <-w.ctx:
			return
		default:
		}
		if err := w.connectAndServe(); err != nil {
			utils.Debugf("[CUPS] ws error (%s): %v", w.auth.roomUUID, err)
		}
		w.connected.Store(false)
		if w.closed.Load() {
			return
		}
		w.stats.reconnects.Add(1)
		select {
		case <-time.After(delay):
		case <-w.ctx:
			return
		}
		delay = time.Duration(float64(delay) * w.config.ReconnectMultiplier)
		if delay > w.config.ReconnectMaxDelay {
			delay = w.config.ReconnectMaxDelay
		}
	}
}

func (w *cupsWS) connectAndServe() error {
	wsURL := strings.Replace(w.auth.connURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.TrimRight(wsURL, "/") + "/websocket"

	header := http.Header{}
	header.Set("Origin", originOf(w.auth.connURL))
	header.Set("User-Agent", cupsUA)
	for _, c := range w.auth.httpClient.Jar.Cookies(mustParseURL(w.auth.connURL)) {
		header.Add("Cookie", c.Name+"="+c.Value)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: w.config.WSHandshakeTimeout,
		ReadBufferSize:   w.config.ReadBufferSize,
		WriteBufferSize:  w.config.WriteBufferSize,
	}
	conn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	w.writeMu.Lock()
	w.conn = conn
	w.writeMu.Unlock()
	defer func() {
		w.writeMu.Lock()
		w.conn = nil
		w.writeMu.Unlock()
		conn.Close()
	}()

	conn.SetReadLimit(int64(w.config.ReadBufferSize))

	if err := w.writeJSON(map[string]interface{}{
		"id": 1, "connect": map[string]interface{}{"token": w.auth.connToken, "name": "js"},
	}); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(w.config.WSReadTimeout))
	if _, _, err := conn.ReadMessage(); err != nil {
		return err
	}
	if err := w.writeJSON(map[string]interface{}{
		"id": 2, "subscribe": map[string]interface{}{"channel": w.auth.channel, "token": w.auth.subToken},
	}); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(w.config.WSReadTimeout))
	if _, _, err := conn.ReadMessage(); err != nil {
		return err
	}
	w.connected.Store(true)
	utils.Debugf("[CUPS] WS ready: %s", w.auth.roomUUID)

	kaStop := make(chan struct{})
	go w.keepAliveLoop(kaStop)
	defer close(kaStop)

	for {
		select {
		case <-w.ctx:
			return nil
		default:
		}
		conn.SetReadDeadline(time.Now().Add(w.config.WSReadTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		w.handleMessage(raw)
	}
}

func (w *cupsWS) keepAliveLoop(stop chan struct{}) {
	t := time.NewTicker(w.config.KeepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-w.ctx:
			return
		case <-t.C:
			_ = w.writeJSON(map[string]interface{}{
				"rpc": map[string]interface{}{
					"method": "shared_editor_ping",
					"data":   map[string]interface{}{"room": w.auth.roomUUID, "user": w.auth.userUUID},
				},
				"id": w.nextID(),
			})
		}
	}
}

func (w *cupsWS) sendLoop() {
	batch := make([][]byte, 0, w.config.BatchMaxPackets)
	totalBytes := 0
	timer := time.NewTimer(w.config.BatchTimeout)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		for !w.connected.Load() {
			select {
			case <-w.ctx:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		if err := w.sendBatch(batch); err != nil {
			utils.Debugf("[CUPS] batch send (%s): %v", w.auth.roomUUID, err)
		}
		batch = batch[:0]
		totalBytes = 0
	}

	for {
		select {
		case <-w.ctx:
			flush()
			return
		case pkt, ok := <-w.sendQueue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, pkt)
			totalBytes += len(pkt)
			if len(batch) >= w.config.BatchMaxPackets || totalBytes >= w.config.BatchMaxBytes {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				flush()
			} else if len(batch) == 1 {
				timer.Reset(w.config.BatchTimeout)
			}
		case <-timer.C:
			flush()
		}
	}
}

func (w *cupsWS) sendBatch(batch [][]byte) error {
	var buf bytes.Buffer
	var hdr [2]byte
	totalRaw := 0
	for _, p := range batch {
		binary.BigEndian.PutUint16(hdr[:], uint16(len(p)))
		buf.Write(hdr[:])
		buf.Write(p)
		totalRaw += len(p)
	}
	col := base64.RawStdEncoding.EncodeToString(buf.Bytes())

	msg := map[string]interface{}{
		"rpc": map[string]interface{}{
			"method": "shared_editor_change_cursors",
			"data": map[string]interface{}{
				"cursors": []map[string]interface{}{{"row": 0, "column": col}},
				"ranges":  []interface{}{},
				"room":    w.auth.roomUUID,
				"user":    w.auth.userUUID,
			},
		},
		"id": w.nextID(),
	}
	if err := w.writeJSON(msg); err != nil {
		return err
	}
	w.stats.packetsSent.Add(uint64(len(batch)))
	w.stats.bytesSent.Add(uint64(totalRaw))
	w.stats.batchesSent.Add(1)
	return nil
}

func (w *cupsWS) handleMessage(raw []byte) {
	if len(raw) == 2 && raw[0] == '{' && raw[1] == '}' {
		_ = w.writeRaw([]byte("{}"))
		return
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return
	}
	push, ok := obj["push"].(map[string]interface{})
	if !ok {
		return
	}
	pub, _ := push["pub"].(map[string]interface{})
	data, _ := pub["data"].(map[string]interface{})
	if data == nil {
		return
	}
	if t, _ := data["type"].(string); t != "cursors_update" {
		return
	}
	payload, _ := data["payload"].(map[string]interface{})
	if payload == nil {
		return
	}
	if uuid, _ := payload["user_uuid"].(string); uuid == w.auth.userUUID {
		return
	}
	cursors, _ := payload["cursors"].([]interface{})
	if len(cursors) == 0 {
		return
	}
	c0, _ := cursors[0].(map[string]interface{})
	if c0 == nil {
		return
	}
	colStr, ok := c0["column"].(string)
	if !ok || colStr == "" {
		return
	}
	decoded, err := base64.RawStdEncoding.DecodeString(colStr)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(colStr)
		if err != nil {
			return
		}
	}

	count := 0
	total := 0
	for len(decoded) >= 2 {
		ln := int(binary.BigEndian.Uint16(decoded[:2]))
		decoded = decoded[2:]
		if ln == 0 || len(decoded) < ln {
			break
		}
		pkt := decoded[:ln]
		decoded = decoded[ln:]
		if w.onData != nil {
			w.onData(pkt)
		}
		count++
		total += ln
	}
	if count > 0 {
		w.stats.packetsRecv.Add(uint64(count))
		w.stats.bytesRecv.Add(uint64(total))
		w.stats.batchesRecv.Add(1)
	}
}

func (w *cupsWS) Send(data []byte) error {
	if len(data) > w.config.MaxPayloadBytes {
		return fmt.Errorf("too large: %d", len(data))
	}
	select {
	case w.sendQueue <- data:
		return nil
	case <-w.ctx:
		return fmt.Errorf("closed")
	}
}

// ---- flow key ----

type flowKey struct {
	srcIP, dstIP     uint32
	srcPort, dstPort uint16
	proto            uint8
}

type flowSender struct {
	seq   atomic.Uint64
	chIdx int
}

// ---- Transport ----

type CupsonlineTransport struct {
	*transport.BaseTransport

	baseURL   string
	urls      []string
	config    CupsonlineConfig
	isClient  bool
	clientErr error

	auths []*cupsAuth
	wss   []*cupsWS

	flowMu sync.RWMutex
	flows  map[flowKey]*flowSender

	stopCh     chan struct{}
	statsStart time.Time
}

func NewCupsonlineTransport(rawURL string, cfg transport.TransportConfig, isClient bool) *CupsonlineTransport {
	t := &CupsonlineTransport{
		BaseTransport: transport.NewBaseTransport(cfg),
		config:        DefaultCupsonlineConfig(),
		isClient:      isClient,
		flows:         make(map[flowKey]*flowSender),
		stopCh:        make(chan struct{}),
		statsStart:    time.Now(),
	}

	if !isClient {
		t.baseURL = baseRoomURL
		return t
	}

	var ids []string
	if rawURL != "" {
		if u, err := url.Parse(rawURL); err == nil {
			q := u.Query()
			if packed := q.Get("rooms"); packed != "" {
				if got, err := unpackRooms(packed); err == nil {
					ids = got
				}
			} else if single := q.Get("room"); single != "" {
				ids = []string{single}
			}
		}
	}
	if len(ids) == 0 && rawURL != "" {
		if got, err := unpackRooms(rawURL); err == nil && len(got) > 0 {
			ids = got
		}
	}
	if len(ids) == 0 {
		t.clientErr = fmt.Errorf("client mode: --url must contain base64 room list")
		return t
	}
	t.urls = make([]string, 0, len(ids))
	for _, id := range ids {
		t.urls = append(t.urls, baseRoomURL+"?room="+id)
	}
	utils.Debugf("[CUPS] client mode: %d rooms from base64", len(t.urls))
	return t
}

func (t *CupsonlineTransport) Start() error {
	if t.clientErr != nil {
		return t.clientErr
	}
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	var auths []*cupsAuth
	var err error

	if t.isClient {
		for i, u := range t.urls {
			a, e := authorize(u)
			if e != nil {
				utils.Debugf("[CUPS] join %d failed: %v", i, e)
				continue
			}
			auths = append(auths, a)
		}
		if len(auths) == 0 {
			return fmt.Errorf("no rooms joined")
		}
	} else {
		auths, err = createRooms(t.baseURL, t.config.NumRooms, t.config.RoomCreatePause)
		if err != nil {
			return fmt.Errorf("create rooms: %w", err)
		}
		packed := packRooms(auths)
		fmt.Printf("\n=== COPY THIS TO CLIENT ===\n")
		fmt.Printf("%s\n", packed)
		fmt.Printf("===========================\n\n")
	}

	t.auths = auths

	for i, a := range auths {
		cs := &channelStats{idx: i, roomUUID: a.roomUUID}
		ws := &cupsWS{
			auth:      a,
			config:    t.config,
			ctx:       make(chan struct{}),
			sendQueue: make(chan []byte, t.config.SendQueueSize),
			stats:     cs,
		}
		ws.onData = t.handleIncoming
		t.wss = append(t.wss, ws)
		go ws.run()
		go ws.sendLoop()
	}

	utils.Debugf("[CUPS] transport started: %d channels", len(t.wss))
	t.SetConnected(true)

	go t.statsLoop()
	return nil
}

func (t *CupsonlineTransport) Stop() error {
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
	for _, ws := range t.wss {
		ws.closed.Store(true)
		close(ws.ctx)
	}
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

// Send — flow-hash: один TCP-flow всегда в один WS-канал.
// Это гарантирует порядок внутри потока, Centrifugo сохраняет порядок push'ей.
func (t *CupsonlineTransport) Send(data []byte) error {
	if len(t.wss) == 0 {
		return fmt.Errorf("no ws")
	}
	key := extractFlowKey(data)

	t.flowMu.RLock()
	fs, ok := t.flows[key]
	t.flowMu.RUnlock()

	if !ok {
		t.flowMu.Lock()
		fs, ok = t.flows[key]
		if !ok {
			idx := int(flowHash(key) % uint64(len(t.wss)))
			fs = &flowSender{chIdx: idx}
			t.flows[key] = fs
			t.wss[idx].stats.flows.Add(1)
			utils.Debugf("[CUPS] new flow %s:%d -> %s:%d proto=%d -> ws[%d] %s",
				ipStr(key.srcIP), key.srcPort, ipStr(key.dstIP), key.dstPort, key.proto,
				idx, t.wss[idx].auth.roomUUID[:8])
		}
		t.flowMu.Unlock()
	}

	fs.seq.Add(1)
	return t.wss[fs.chIdx].Send(data)
}

func (t *CupsonlineTransport) handleIncoming(pkt []byte) {
	t.CallReceive(pkt)
}

func (t *CupsonlineTransport) Receive(callback func([]byte)) {
	t.BaseTransport.Receive(callback)
}

func (t *CupsonlineTransport) IsConnected() bool {
	return t.BaseTransport.IsConnected()
}

func (t *CupsonlineTransport) Stats() transport.TransportStats {
	var sent, recv, bytesSent, bytesRecv, reconnects uint64
	for _, ws := range t.wss {
		sent += ws.stats.packetsSent.Load()
		recv += ws.stats.packetsRecv.Load()
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

// statsLoop — печатает per-channel статистику каждые StatsInterval секунд.
func (t *CupsonlineTransport) statsLoop() {
	tick := time.NewTicker(t.config.StatsInterval)
	defer tick.Stop()

	lastSent := make([]uint64, len(t.wss))
	lastRecv := make([]uint64, len(t.wss))
	lastBytesSent := make([]uint64, len(t.wss))
	lastBytesRecv := make([]uint64, len(t.wss))
	var lastTotalSent, lastTotalRecv, lastTotalBytesSent, lastTotalBytesRecv uint64

	for {
		select {
		case <-t.stopCh:
			return
		case <-tick.C:
			var totalSent, totalRecv, totalBytesSent, totalBytesRecv uint64
			for i, ws := range t.wss {
				s := ws.stats.packetsSent.Load()
				r := ws.stats.packetsRecv.Load()
				bs := ws.stats.bytesSent.Load()
				br := ws.stats.bytesRecv.Load()

				dS := s - lastSent[i]
				dR := r - lastRecv[i]
				dBS := bs - lastBytesSent[i]
				dBR := br - lastBytesRecv[i]

				lastSent[i] = s
				lastRecv[i] = r
				lastBytesSent[i] = bs
				lastBytesRecv[i] = br

				totalSent += s
				totalRecv += r
				totalBytesSent += bs
				totalBytesRecv += br

				utils.Debugf("[CH-%02d %s] tx=%d pkt/s (%.1f KB/s)  rx=%d pkt/s (%.1f KB/s)  flows=%d  reconn=%d  conn=%v",
					i, ws.auth.roomUUID[:8],
					dS/uint64(t.config.StatsInterval.Seconds()),
					float64(dBS)/t.config.StatsInterval.Seconds()/1024,
					dR/uint64(t.config.StatsInterval.Seconds()),
					float64(dBR)/t.config.StatsInterval.Seconds()/1024,
					ws.stats.flows.Load(),
					ws.stats.reconnects.Load(),
					ws.connected.Load(),
				)
			}
			dtS := totalSent - lastTotalSent
			dtR := totalRecv - lastTotalRecv
			dtBS := totalBytesSent - lastTotalBytesSent
			dtBR := totalBytesRecv - lastTotalBytesRecv
			lastTotalSent = totalSent
			lastTotalRecv = totalRecv
			lastTotalBytesSent = totalBytesSent
			lastTotalBytesRecv = totalBytesRecv

			utils.Debugf("[CUPS-TOTAL] tx=%d pkt/s (%.1f KB/s)  rx=%d pkt/s (%.1f KB/s)  flows=%d  uptime=%v",
				dtS/uint64(t.config.StatsInterval.Seconds()),
				float64(dtBS)/t.config.StatsInterval.Seconds()/1024,
				dtR/uint64(t.config.StatsInterval.Seconds()),
				float64(dtBR)/t.config.StatsInterval.Seconds()/1024,
				len(t.flows),
				time.Since(t.statsStart).Round(time.Second),
			)
		}
	}
}

func (t *CupsonlineTransport) RoomUUIDs() []string {
	out := make([]string, 0, len(t.auths))
	for _, a := range t.auths {
		out = append(out, a.roomUUID)
	}
	return out
}

// ---- helpers ----

func extractFlowKey(pkt []byte) flowKey {
	if len(pkt) < 20 {
		return flowKey{}
	}
	if pkt[0]>>4 != 4 {
		return flowKey{}
	}
	proto := pkt[9]
	srcIP := binary.BigEndian.Uint32(pkt[12:16])
	dstIP := binary.BigEndian.Uint32(pkt[16:20])
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+4 {
		return flowKey{srcIP: srcIP, dstIP: dstIP, proto: proto}
	}
	if proto == 6 || proto == 17 {
		sp := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
		dp := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
		return flowKey{srcIP: srcIP, dstIP: dstIP, srcPort: sp, dstPort: dp, proto: proto}
	}
	return flowKey{srcIP: srcIP, dstIP: dstIP, proto: proto}
}

// flowHash — детерминированный хеш от flow. Один поток всегда на один WS.
func flowHash(k flowKey) uint64 {
	h := fnv.New64a()
	var b [14]byte
	binary.BigEndian.PutUint32(b[0:4], k.srcIP)
	binary.BigEndian.PutUint32(b[4:8], k.dstIP)
	binary.BigEndian.PutUint16(b[8:10], k.srcPort)
	binary.BigEndian.PutUint16(b[10:12], k.dstPort)
	b[12] = k.proto
	b[13] = 0
	h.Write(b[:])
	return h.Sum64()
}

func ipStr(ip uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
}

func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}
