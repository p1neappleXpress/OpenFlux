package cupsonline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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

// baseRoomURL is where the rooms live. It's a variable only so tests can aim
// the transport at a local stand-in for cups.online.
var baseRoomURL = "https://interview.cups.online/live-coding/"

const cupsUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

type CupsonlineConfig struct {
	WSHandshakeTimeout  time.Duration
	WSReadTimeout       time.Duration
	WSWriteTimeout      time.Duration
	KeepAliveInterval   time.Duration
	ReconnectMinDelay   time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectMultiplier float64
	HTTPTimeout         time.Duration

	// Комнату, похожую на закрытую, проверяем не чаще этого: от Min с
	// удвоением до Max. Каждая такая проверка может заставить cups выдать
	// новую комнату, так что долбить её нельзя.
	RoomGoneRetryMin time.Duration
	RoomGoneRetryMax time.Duration

	// Батчинг внутри одного WS. Данные едут в целых row/column курсоров
	// (bytesPerNumber байт на число, два числа на курсор): cups.online больше
	// не принимает строку в column, но массив целых курсоров ретранслирует
	// как есть, дословно и по порядку.
	//
	// BatchMaxBytes — сколько байт пакетов копится за одну отправку.
	// MaxMessageData — сколько байт едет в одном сообщении: на проводе число
	// стоит ~4× своих байт, поэтому пачка режется на несколько сообщений,
	// каждое заведомо под потолком сервера (проверено probe'ом: ~21 KB
	// проходит, ~44 KB уже нет), а приёмная сторона склеивает их по порядку.
	// MaxCursors — страховочный потолок массива в одном сообщении.
	BatchMaxPackets int
	BatchMaxBytes   int
	MaxMessageData  int
	MaxCursors      int
	BatchTimeout    time.Duration

	// SendInterval is the least time between two messages on one channel.
	// cups.online throttles inbound, and firing messages back to back stalls
	// the write and drops the connection, so the transport paces itself below
	// that. Four channels each send at this rate.
	SendInterval time.Duration

	SendQueueSize int

	// Буферы сокета, а не лимит на сообщение: gorilla выделяет их целиком на
	// каждое соединение. MaxMessageBytes — сколько может прийти одним фреймом.
	ReadBufferSize  int
	WriteBufferSize int
	MaxMessageBytes int

	MaxPayloadBytes int

	NumRooms        int
	RoomCreatePause time.Duration

	StatsInterval time.Duration
}

func DefaultCupsonlineConfig() CupsonlineConfig {
	return CupsonlineConfig{
		WSHandshakeTimeout: 15 * time.Second,
		// Сервер пингует каждые ~25 с и отвечает на наш keepalive раз в 20 с,
		// так что полторы минуты тишины — это мёртвое соединение.
		WSReadTimeout:       90 * time.Second,
		WSWriteTimeout:      15 * time.Second,
		KeepAliveInterval:   20 * time.Second,
		ReconnectMinDelay:   100 * time.Millisecond,
		ReconnectMaxDelay:   10 * time.Second,
		ReconnectMultiplier: 1.3,
		HTTPTimeout:         30 * time.Second,

		RoomGoneRetryMin: time.Minute,
		RoomGoneRetryMax: 30 * time.Minute,

		BatchMaxPackets: 256,
		BatchMaxBytes:   11000,
		// 4096 байт данных → 2 байта заголовка → ~342 курсора → провод ~17 KB,
		// с запасом под проверенными ~21 KB и вчетверо меньше потолка.
		MaxMessageData: 4096,
		MaxCursors:     1000,
		BatchTimeout:   2 * time.Millisecond,
		// 18ms (~55 msg/s per channel) — calibrated against the live server
		// (TestLiveThroughputCalibrate): the knee of throughput (~80 KB/s per
		// channel; faster doesn't help, the server itself is the ceiling) with
		// no reconnects and margin under the burst that stalled the old,
		// unpaced code.
		SendInterval: 18 * time.Millisecond,

		// В очереди кадры кодека (до ~8 KB), а не IP-пакеты: очередь длиннее
		// — это уже не буфер, а секунды задержки.
		SendQueueSize: 1024,

		ReadBufferSize:  64 << 10,
		WriteBufferSize: 64 << 10,
		MaxMessageBytes: 8 << 20,

		// Длина пакета во внутреннем кадре — uint16, так что один пакет не
		// может быть больше 65535 байт. Кадр кодека (до ~8 KB) с запасом
		// помещается и режется на сообщения при отправке.
		MaxPayloadBytes: 65535,

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

// authorize loads a room page and collects what it takes to join the room's
// channel. session carries the cookies of an earlier join over (nil starts a
// fresh one), so re-joining doesn't add a new participant every time.
func authorize(ctx context.Context, roomURL string, session *http.Client) (*cupsAuth, error) {
	client := session
	if client == nil {
		jar, _ := cookiejar.New(nil)
		client = &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		}
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", roomURL, nil)
	req.Header.Set("User-Agent", cupsUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET room: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, fmt.Errorf("%w: GET room status %d", errRoomGone, resp.StatusCode)
	}
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
	for _, c := range client.Jar.Cookies(mustParseURL(roomURL)) {
		if c.Name == "csrftoken" {
			a.csrfToken = c.Value
			break
		}
	}
	if a.roomUUID == "" || a.userUUID == "" {
		// A closed room's page renders without the room/user data.
		return nil, fmt.Errorf("%w: room/user uuid missing", errRoomGone)
	}
	if a.connToken == "" || a.connURL == "" || a.subURL == "" {
		return nil, fmt.Errorf("centrifuge meta missing")
	}
	if a.csrfToken == "" {
		return nil, fmt.Errorf("csrftoken missing")
	}
	a.channel = fmt.Sprintf("$shared_editor:room-%s", a.roomUUID)

	subBody, _ := json.Marshal(map[string]string{"channel": a.channel})
	req2, _ := http.NewRequestWithContext(ctx, "POST", a.subURL, bytes.NewReader(subBody))
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

var (
	// errRoomGone marks a room that no longer exists, as opposed to a network
	// hiccup on the way to it.
	errRoomGone = errors.New("комната не существует")
	// errRefused marks Centrifugo turning our tokens down, with an error
	// reply or by hanging up mid-handshake. Only a fresh join fixes that.
	errRefused = errors.New("refused")
	errNoRoom  = errors.New("cups: нет подключённой комнаты")
	errStopped = errors.New("cups: транспорт остановлен")
)

func joinURL(roomUUID string) string { return baseRoomURL + "?room=" + roomUUID }

// joinRoom enters an existing room. A room that's gone can come back as a
// fresh one (a different uuid) instead of an error, so that counts as gone.
func joinRoom(ctx context.Context, roomUUID string, session *http.Client) (*cupsAuth, error) {
	a, err := authorize(ctx, joinURL(roomUUID), session)
	if err != nil {
		return nil, err
	}
	if a.roomUUID != roomUUID {
		return nil, fmt.Errorf("%w: вместо неё выдана новая %s", errRoomGone, shortRoom(a.roomUUID))
	}
	return a, nil
}

// readReply waits for the reply to command id. Centrifugo reports refusals
// (expired token, unknown channel...) as {"error": ...} in the reply, which
// used to be ignored - the channel then just sat there never receiving
// anything. Centrifugo may also pack several messages into one frame, one per
// line; whatever else comes along (a ping, a push) goes to other.
func readReply(conn *websocket.Conn, timeout time.Duration, id int, what string, other func([]byte)) error {
	deadline := time.Now().Add(timeout)
	for {
		conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return fmt.Errorf("%w: %s: %v", errRefused, what, err)
			}
			return err
		}
		found := false
		var refusal error
		for _, line := range splitFrame(raw) {
			if !found {
				if ok, err := matchReply(line, id); ok {
					found, refusal = true, err
					continue
				}
			}
			if other != nil {
				other(line)
			}
		}
		if refusal != nil {
			return fmt.Errorf("%w: %s: %v", errRefused, what, refusal)
		}
		if found {
			return nil
		}
	}
}

// matchReply reports whether line is the reply to command id, and the error
// it carries if it is one.
func matchReply(line []byte, id int) (bool, error) {
	if isPing(line) {
		return false, nil
	}
	var reply struct {
		ID    int `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &reply) != nil || reply.ID != id {
		return false, nil
	}
	if reply.Error != nil {
		return true, fmt.Errorf("%d %s", reply.Error.Code, reply.Error.Message)
	}
	return true, nil
}

// splitFrame takes apart a frame Centrifugo may have packed several messages
// into, one per line. Parsing such a frame whole fails, which silently lost
// every packet in it.
func splitFrame(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// isPing reports a Centrifugo server ping, which must be answered in kind.
func isPing(line []byte) bool { return string(line) == "{}" }

func shortRoom(roomUUID string) string {
	if len(roomUUID) > 8 {
		return roomUUID[:8]
	}
	return roomUUID
}

// fmtUptime renders a duration the way the log reads best: "47ч12м", "3м05с".
func fmtUptime(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%dч%02dм", h, m)
	}
	return fmt.Sprintf("%dм%02dс", m, s)
}

// parseRoomList pulls room uuids out of --url: the packed base64 list the
// exit node prints, bare or as ?rooms=, or a single ?room=.
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
		} else if single := q.Get("room"); single != "" {
			return []string{single}
		}
	}
	if ids, err := unpackRooms(rawURL); err == nil {
		return ids
	}
	return nil
}

func createRooms(ctx context.Context, baseURL string, n int, pause time.Duration) ([]*cupsAuth, error) {
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
			a, lastErr = authorize(ctx, baseURL, nil)
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
			if !sleepCtx(ctx, wait) {
				return nil, errStopped
			}
		}
		if a == nil {
			utils.Debugf("[CUPS] room %d skipped: %v", i+1, lastErr)
			continue
		}
		out = append(out, a)
		utils.Debugf("[CUPS] created room %d/%d: %s", i+1, n, a.roomUUID)
		if i < n-1 && !sleepCtx(ctx, delay) {
			return nil, errStopped
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("could not create any room")
	}
	return out, nil
}

func packRooms(ids []string) string {
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

// sleepCtx waits for d, or less if ctx ends first; it reports whether the
// whole wait went by.
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
	dropped     atomic.Uint64
}

// ---- WS ----

type cupsWS struct {
	idx      int
	roomUUID string
	joinedAt time.Time

	// authPtr is swapped on every re-join (fresh tokens), so it's atomic. It
	// stays nil until the room has been entered at least once.
	authPtr atomic.Pointer[cupsAuth]
	// session keeps the cookies between joins (run's goroutine only).
	session *http.Client

	// dead is set while the room looks closed; onRoomState reports the
	// change so the operator can see when (and after how long) it happened.
	dead        atomic.Bool
	goneDelay   time.Duration // run's goroutine only
	lastSend    time.Time     // sendLoop goroutine only; paces messages
	onRoomState func()

	config CupsonlineConfig
	conn   *websocket.Conn

	writeMu sync.Mutex
	rpcID   atomic.Int64
	onData  func([]byte)

	ctx       context.Context
	connected atomic.Bool

	// recvBuf reassembles the peer's byte stream across messages (one packet
	// may be split over several). Touched only by the read goroutine, and
	// reset on every reconnect - a packet split across a drop is lost, like
	// on any link.
	recvBuf []byte

	sendQueue chan []byte

	stats *channelStats
}

func (w *cupsWS) nextID() int64 { return w.rpcID.Add(1) }

func (w *cupsWS) auth() *cupsAuth { return w.authPtr.Load() }

func (w *cupsWS) writeRaw(data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("ws not connected")
	}
	w.conn.SetWriteDeadline(time.Now().Add(w.config.WSWriteTimeout))
	if err := w.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		// A failed or timed-out write leaves the socket unusable. Closing it
		// makes the reader notice and reconnect instead of sitting on a dead
		// channel until the read timeout.
		w.conn.Close()
		return err
	}
	return nil
}

func (w *cupsWS) writeJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeRaw(data)
}

// roomDeadAfterFails is how many joins in a row may fail to reach a
// subscribed channel (with fresh tokens each time) before the room is
// reported closed even though its page still loads.
const roomDeadAfterFails = 5

func (w *cupsWS) run() {
	delay := w.config.ReconnectMinDelay
	fails := 0
	// Start hands over freshly joined rooms; one it couldn't enter has no
	// tokens yet and has to be joined first.
	needJoin := w.auth() == nil
	for w.ctx.Err() == nil {
		if needJoin || w.dead.Load() {
			if err := w.join(); err != nil {
				if !sleepCtx(w.ctx, w.retryDelay(delay)) {
					return
				}
				delay = w.backoff(delay)
				continue
			}
			needJoin = false
		}

		err := w.connectAndServe()
		wasReady := w.connected.Swap(false)
		if w.ctx.Err() != nil {
			return
		}
		utils.Debugf("[CUPS] ws error (%s): %v", w.roomUUID, err)
		if wasReady {
			delay = w.config.ReconnectMinDelay
			fails = 0
		} else if fails++; fails == roomDeadAfterFails {
			w.markDead(fmt.Errorf("канал не поднимается %d раз подряд: %v", fails, err))
		}
		w.stats.reconnects.Add(1)
		// Tokens come from the room page once per join and can expire, so
		// reconnecting with the old ones may never work again. Re-join when
		// Centrifugo turned them down, and every other failed attempt in case
		// it said so just by hanging up. A connection that was working and
		// dropped just reconnects: a join is two page loads, and a reconnect
		// storm of those is how an IP gets rate-limited.
		needJoin = errors.Is(err, errRefused) || (!wasReady && fails%2 == 0)
		if !sleepCtx(w.ctx, w.retryDelay(delay)) {
			return
		}
		delay = w.backoff(delay)
	}
}

// retryDelay is the wait before the next attempt: the usual backoff, or the
// much slower probe interval while the room looks closed.
func (w *cupsWS) retryDelay(delay time.Duration) time.Duration {
	if !w.dead.Load() {
		return delay
	}
	d := w.goneDelay
	w.goneDelay = min(2*d, w.config.RoomGoneRetryMax)
	return d
}

func (w *cupsWS) backoff(delay time.Duration) time.Duration {
	return min(time.Duration(float64(delay)*w.config.ReconnectMultiplier), w.config.ReconnectMaxDelay)
}

// join enters the room for fresh tokens, in the same session as before. This
// is also where a closed room shows itself.
func (w *cupsWS) join() error {
	a, err := joinRoom(w.ctx, w.roomUUID, w.session)
	if err != nil {
		// Next time from a clean session, in case this one is what's broken.
		w.session = nil
		if errors.Is(err, errRoomGone) {
			w.markDead(err)
		} else if w.ctx.Err() == nil {
			utils.Debugf("[CUPS] join %s failed: %v", w.roomUUID, err)
		}
		return err
	}
	w.session = a.httpClient
	w.authPtr.Store(a)
	return nil
}

func (w *cupsWS) markDead(err error) {
	if w.dead.Swap(true) {
		return
	}
	utils.Infof("[ERROR] Cups: комната %s закрылась (в работе %s): %v",
		shortRoom(w.roomUUID), fmtUptime(time.Since(w.joinedAt)), err)
	if w.onRoomState != nil {
		w.onRoomState()
	}
}

func (w *cupsWS) markAlive() {
	w.goneDelay = w.config.RoomGoneRetryMin
	if !w.dead.Swap(false) {
		return
	}
	utils.Infof("[CUPS] Cups: комната %s снова доступна", shortRoom(w.roomUUID))
	if w.onRoomState != nil {
		w.onRoomState()
	}
}

func (w *cupsWS) connectAndServe() error {
	a := w.auth()
	wsURL := strings.Replace(a.connURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.TrimRight(wsURL, "/") + "/websocket"

	header := http.Header{}
	header.Set("Origin", originOf(a.connURL))
	header.Set("User-Agent", cupsUA)
	for _, c := range a.httpClient.Jar.Cookies(mustParseURL(a.connURL)) {
		header.Add("Cookie", c.Name+"="+c.Value)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: w.config.WSHandshakeTimeout,
		ReadBufferSize:   w.config.ReadBufferSize,
		WriteBufferSize:  w.config.WriteBufferSize,
	}
	conn, _, err := dialer.DialContext(w.ctx, wsURL, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	// Stop mustn't have to wait for the server's next message to take the
	// reader down.
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

	conn.SetReadLimit(int64(w.config.MaxMessageBytes))
	// Fresh socket, fresh stream: drop any half-assembled packet from before.
	w.recvBuf = w.recvBuf[:0]

	if err := w.writeJSON(map[string]interface{}{
		"id": 1, "connect": map[string]interface{}{"token": a.connToken, "name": "js"},
	}); err != nil {
		return err
	}
	if err := readReply(conn, w.config.WSHandshakeTimeout, 1, "connect", w.handleReply); err != nil {
		return err
	}
	if err := w.writeJSON(map[string]interface{}{
		"id": 2, "subscribe": map[string]interface{}{"channel": a.channel, "token": a.subToken},
	}); err != nil {
		return err
	}
	if err := readReply(conn, w.config.WSHandshakeTimeout, 2, "subscribe", w.handleReply); err != nil {
		return err
	}
	w.connected.Store(true)
	w.markAlive()
	utils.Debugf("[CUPS] WS ready: %s", a.roomUUID)

	kaStop := make(chan struct{})
	go w.keepAliveLoop(kaStop)
	defer close(kaStop)

	for {
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
		case <-w.ctx.Done():
			return
		case <-t.C:
			_ = w.writeJSON(map[string]interface{}{
				"rpc": map[string]interface{}{
					"method": "shared_editor_ping",
					"data":   map[string]interface{}{"room": w.auth().roomUUID, "user": w.auth().userUUID},
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
	timer.Stop()
	defer timer.Stop()

	flush := func() {
		timer.Stop()
		if len(batch) == 0 {
			return
		}
		if !w.connected.Load() {
			// The channel went down with this batch still here. New traffic
			// has already moved to a room that's up, and by the time this one
			// is back the batch is stale, so it's dropped rather than
			// delivered late and out of order.
			w.stats.dropped.Add(uint64(len(batch)))
		} else if err := w.sendBatch(batch); err != nil {
			w.stats.dropped.Add(uint64(len(batch)))
			utils.Debugf("[CUPS] batch send (%s): %v", w.roomUUID, err)
		}
		clear(batch)
		batch = batch[:0]
		totalBytes = 0
	}

	for {
		select {
		case <-w.ctx.Done():
			return
		case pkt := <-w.sendQueue:
			// Flush before a packet that would overflow the batch, not after
			// it: otherwise a batch ends up a whole packet over the limit.
			if len(batch) > 0 && totalBytes+2+len(pkt) > w.config.BatchMaxBytes {
				flush()
			}
			batch = append(batch, pkt)
			totalBytes += 2 + len(pkt)
			if len(batch) >= w.config.BatchMaxPackets || totalBytes >= w.config.BatchMaxBytes {
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
	blob := buf.Bytes()

	// One message can only carry so much before the server drops it, so a
	// batch is cut into chunks and each goes as its own message. The far side
	// stitches them back together in order.
	for off := 0; off < len(blob); off += w.config.MaxMessageData {
		end := min(off+w.config.MaxMessageData, len(blob))
		if err := w.pace(); err != nil {
			return err
		}
		if err := w.sendChunk(blob[off:end]); err != nil {
			return err
		}
	}
	w.stats.packetsSent.Add(uint64(len(batch)))
	w.stats.bytesSent.Add(uint64(totalRaw))
	w.stats.batchesSent.Add(1)
	return nil
}

// pace waits out the rest of SendInterval since the last message, so the
// channel never floods the server. Returns errStopped if the transport is
// stopped while waiting.
func (w *cupsWS) pace() error {
	if w.config.SendInterval <= 0 {
		return nil
	}
	if d := w.config.SendInterval - time.Since(w.lastSend); d > 0 {
		if !sleepCtx(w.ctx, d) {
			return errStopped
		}
	}
	w.lastSend = time.Now()
	return nil
}

// sendChunk sends one message: a 2-byte length of the chunk, then the chunk,
// packed into cursor integers. The length lets the peer strip the zero
// padding exactly before stitching the byte stream back.
func (w *cupsWS) sendChunk(chunk []byte) error {
	payload := make([]byte, 2+len(chunk))
	binary.BigEndian.PutUint16(payload[:2], uint16(len(chunk)))
	copy(payload[2:], chunk)

	msg := map[string]interface{}{
		"rpc": map[string]interface{}{
			"method": "shared_editor_change_cursors",
			"data": map[string]interface{}{
				"cursors": cursorsFromBytes(payload),
				"ranges":  []interface{}{},
				"room":    w.auth().roomUUID,
				"user":    w.auth().userUUID,
			},
		},
		"id": w.nextID(),
	}
	return w.writeJSON(msg)
}

// bytesPerNumber is how many bytes ride in one row/column integer. cups.online
// keeps a cursor integer exact up to 2^53-1; 6 bytes (48 bits) stays well
// inside that and is byte-aligned.
const bytesPerNumber = 6

// cursorsFromBytes packs a blob into {row, column} integer pairs. The data
// rides in the numbers because cups.online rejects a non-numeric column but
// relays a cursor array of integers verbatim and in order. Trailing padding is
// zero, which the length-prefixed framing on the far side reads as "no more
// packets" and stops.
func cursorsFromBytes(blob []byte) []map[string]interface{} {
	nums := packNumbers(blob)
	cursors := make([]map[string]interface{}, 0, (len(nums)+1)/2)
	for i := 0; i < len(nums); i += 2 {
		c := map[string]interface{}{"row": nums[i], "column": uint64(0)}
		if i+1 < len(nums) {
			c["column"] = nums[i+1]
		}
		cursors = append(cursors, c)
	}
	return cursors
}

// packNumbers cuts a blob into big-endian bytesPerNumber-byte integers, the
// last one zero-padded.
func packNumbers(blob []byte) []uint64 {
	n := (len(blob) + bytesPerNumber - 1) / bytesPerNumber
	out := make([]uint64, n)
	for i := range out {
		var v uint64
		for j := 0; j < bytesPerNumber; j++ {
			v <<= 8
			if idx := i*bytesPerNumber + j; idx < len(blob) {
				v |= uint64(blob[idx])
			}
		}
		out[i] = v
	}
	return out
}

// bytesFromNumbers reverses packNumbers.
func bytesFromNumbers(nums []uint64) []byte {
	out := make([]byte, 0, len(nums)*bytesPerNumber)
	for _, v := range nums {
		var b [bytesPerNumber]byte
		for j := bytesPerNumber - 1; j >= 0; j-- {
			b[j] = byte(v)
			v >>= 8
		}
		out = append(out, b[:]...)
	}
	return out
}

// asCursorInt reads a JSON number back as an integer. Values stay under 2^48,
// which float64 (how encoding/json hands back a number) holds exactly.
func asCursorInt(v interface{}) (uint64, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 {
		return 0, false
	}
	return uint64(f), true
}

func (w *cupsWS) handleMessage(raw []byte) {
	for _, line := range splitFrame(raw) {
		w.handleReply(line)
	}
}

// handleReply handles one message: a server ping, or a push that may carry
// a batch from the peer.
func (w *cupsWS) handleReply(raw []byte) {
	if isPing(raw) {
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
	if uuid, _ := payload["user_uuid"].(string); uuid == w.auth().userUUID {
		return
	}
	cursors, _ := payload["cursors"].([]interface{})
	if len(cursors) == 0 {
		return
	}
	// Rebuild the blob from the cursor integers, two per cursor in order, the
	// reverse of cursorsFromBytes.
	nums := make([]uint64, 0, len(cursors)*2)
	for _, c := range cursors {
		m, _ := c.(map[string]interface{})
		if m == nil {
			return
		}
		row, ok := asCursorInt(m["row"])
		if !ok {
			return
		}
		col, _ := asCursorInt(m["column"])
		nums = append(nums, row, col)
	}
	decoded := bytesFromNumbers(nums)
	// Strip the padding: the first two bytes are how many real bytes follow.
	if len(decoded) < 2 {
		return
	}
	dataLen := int(binary.BigEndian.Uint16(decoded[:2]))
	if 2+dataLen > len(decoded) {
		return
	}
	chunk := decoded[2 : 2+dataLen]

	// Append to the running stream and pull out whole packets; a packet split
	// across messages completes once the rest of it arrives.
	w.recvBuf = append(w.recvBuf, chunk...)
	buf := w.recvBuf
	adv, count, total := 0, 0, 0
	for len(buf)-adv >= 2 {
		ln := int(binary.BigEndian.Uint16(buf[adv : adv+2]))
		if ln == 0 {
			// Not a real length - the stream is out of sync; drop it.
			adv = len(buf)
			break
		}
		if len(buf)-adv < 2+ln {
			break // rest of this packet hasn't arrived yet
		}
		pkt := buf[adv+2 : adv+2+ln]
		if w.onData != nil {
			w.onData(pkt)
		}
		adv += 2 + ln
		count++
		total += ln
	}
	// Keep only the unconsumed tail. onData ran already, so moving the bytes
	// now can't disturb a packet still in flight.
	w.recvBuf = append(w.recvBuf[:0], buf[adv:]...)
	// A stream that never yields a packet must not grow without bound.
	if len(w.recvBuf) > w.config.MaxPayloadBytes+w.config.MaxMessageData {
		utils.Debugf("[CUPS] recv stream out of sync (%s), resetting", w.roomUUID)
		w.recvBuf = w.recvBuf[:0]
	}
	if count > 0 {
		w.stats.packetsRecv.Add(uint64(count))
		w.stats.bytesRecv.Add(uint64(total))
		w.stats.batchesRecv.Add(1)
	}
}

func (w *cupsWS) Send(data []byte) error {
	select {
	case w.sendQueue <- data:
		return nil
	case <-w.ctx.Done():
		return errStopped
	}
}

// ---- Transport ----

type CupsonlineTransport struct {
	*transport.BaseTransport

	baseURL string
	// roomIDs are the rooms to join, from --url. Empty on an exit node
	// means "create new ones"; a client can't start without them.
	roomIDs   []string
	config    CupsonlineConfig
	isClient  bool
	clientErr error

	// ctx ends with Stop and takes every room's goroutines down with it.
	ctx    context.Context
	cancel context.CancelFunc

	wss []*cupsWS
	// rr round-robins outbound frames across the connected rooms; see Send.
	rr atomic.Uint32

	statsStart time.Time
}

func NewCupsonlineTransport(rawURL string, cfg transport.TransportConfig, isClient bool) *CupsonlineTransport {
	ctx, cancel := context.WithCancel(context.Background())
	t := &CupsonlineTransport{
		BaseTransport: transport.NewBaseTransport(cfg),
		config:        DefaultCupsonlineConfig(),
		isClient:      isClient,
		ctx:           ctx,
		cancel:        cancel,
		statsStart:    time.Now(),
	}

	t.baseURL = baseRoomURL
	// Both roles accept a room list: the client always needs one, and an
	// exit node given one re-joins those rooms instead of creating new
	// ones, so a restart doesn't hand the phone a new string every time.
	t.roomIDs = parseRoomList(rawURL)
	if isClient && len(t.roomIDs) == 0 {
		t.clientErr = fmt.Errorf("client mode: --url must contain base64 room list")
	}
	return t
}

// roomSlot is one room the transport will keep a channel to.
type roomSlot struct {
	id   string
	auth *cupsAuth // nil if it couldn't be entered yet
	err  error     // why it couldn't
}

func (t *CupsonlineTransport) Start() error {
	if t.clientErr != nil {
		return t.clientErr
	}
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	slots, err := t.enterRooms()
	if err != nil {
		return err
	}
	if t.ctx.Err() != nil {
		return errStopped
	}

	joinedAt := time.Now()
	wss := make([]*cupsWS, len(slots))
	for i, s := range slots {
		ws := &cupsWS{
			idx:         i,
			roomUUID:    s.id,
			joinedAt:    joinedAt,
			goneDelay:   t.config.RoomGoneRetryMin,
			onRoomState: t.reportRooms,
			config:      t.config,
			ctx:         t.ctx,
			sendQueue:   make(chan []byte, t.config.SendQueueSize),
			stats:       &channelStats{idx: i, roomUUID: s.id},
		}
		// Command ids 1 and 2 are connect and subscribe.
		ws.rpcID.Store(2)
		if s.auth != nil {
			ws.authPtr.Store(s.auth)
			ws.session = s.auth.httpClient
		}
		// Already reported by enterRooms; run() only probes it now and then.
		ws.dead.Store(errors.Is(s.err, errRoomGone))
		ws.onData = t.CallReceive
		wss[i] = ws
	}
	t.wss = wss
	for _, ws := range wss {
		go ws.run()
		go ws.sendLoop()
	}

	utils.Debugf("[CUPS] transport started: %d channels", len(t.wss))
	t.SetConnected(true)

	go t.statsLoop()
	return nil
}

// enterRooms joins the rooms from --url, or, on an exit node without them,
// creates new ones and prints the string for the client.
func (t *CupsonlineTransport) enterRooms() ([]roomSlot, error) {
	if len(t.roomIDs) > 0 {
		slots := t.joinListed()
		joined, allGone := 0, true
		for _, s := range slots {
			if s.auth != nil {
				joined++
			} else if !errors.Is(s.err, errRoomGone) {
				allGone = false
			}
		}
		if t.ctx.Err() != nil {
			return nil, errStopped
		}
		utils.Infof("[CUPS] Cups: зашли в %d из %d комнат", joined, len(slots))
		// The rooms that failed get a channel all the same and keep being
		// retried. Dropping them left the peer's traffic in those rooms with
		// nobody listening.
		if joined > 0 {
			return slots, nil
		}
		switch {
		case t.isClient && allGone:
			utils.Infof("[ERROR] Cups: все комнаты закрыты - пересоздайте комнаты на ноде (запуск без --url) и обновите строку в профиле")
			return nil, fmt.Errorf("no rooms joined: all rooms are gone")
		case t.isClient:
			utils.Infof("[ERROR] Cups: ни одна комната не отвечает - проверьте сеть")
			return nil, fmt.Errorf("no rooms joined")
		case !allGone:
			// New rooms because of a network hiccup would cost the phone its
			// string for nothing. Fail, and let the restart try again.
			utils.Infof("[ERROR] Cups: сохранённые комнаты не отвечают - новые не создаю, чтобы строка на телефоне осталась прежней")
			return nil, fmt.Errorf("saved rooms unreachable")
		}
		utils.Infof("[ERROR] Cups: сохранённые комнаты закрыты, создаю новые - строку на телефоне нужно будет заменить")
	}

	auths, err := createRooms(t.ctx, t.baseURL, t.config.NumRooms, t.config.RoomCreatePause)
	if err != nil {
		return nil, fmt.Errorf("create rooms: %w", err)
	}
	slots := make([]roomSlot, len(auths))
	ids := make([]string, len(auths))
	for i, a := range auths {
		slots[i] = roomSlot{id: a.roomUUID, auth: a}
		ids[i] = a.roomUUID
	}
	fmt.Printf("\n=== COPY THIS TO CLIENT ===\n")
	fmt.Printf("%s\n", packRooms(ids))
	fmt.Printf("===========================\n")
	fmt.Printf("Save it and pass it back as --url to reuse these rooms after a restart.\n\n")
	return slots, nil
}

// joinListed enters all the listed rooms at once rather than one by one: on
// a phone each join is two round trips, and Start blocks the app until
// they're done.
func (t *CupsonlineTransport) joinListed() []roomSlot {
	slots := make([]roomSlot, len(t.roomIDs))
	var wg sync.WaitGroup
	for i, id := range t.roomIDs {
		slots[i].id = id
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots[i].auth, slots[i].err = joinRoom(t.ctx, id, nil)
		}()
	}
	wg.Wait()
	for _, s := range slots {
		if s.err != nil && t.ctx.Err() == nil {
			utils.Infof("[ERROR] Cups: не удалось зайти в комнату %s: %v", shortRoom(s.id), s.err)
		}
	}
	return slots
}

// Stop can come at any time, even before Start is done, and more than once.
func (t *CupsonlineTransport) Stop() error {
	t.cancel()
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

// Send spreads frames across all the connected rooms, round-robin, so the
// four channels carry the load together instead of one. It can't keep a flow
// in one room - what arrives here is already a codec frame mixing many flows,
// with no IP header to hash - so frames from different rooms can arrive out of
// order; a whole frame always stays within one room (reassembly is per-room),
// and TCP above puts the rest back in order. A frame is never split across
// rooms.
func (t *CupsonlineTransport) Send(data []byte) error {
	if len(data) > t.config.MaxPayloadBytes {
		return fmt.Errorf("cups: посылка %d байт больше предела %d", len(data), t.config.MaxPayloadBytes)
	}
	ws := t.pickRoom()
	if ws == nil {
		return errNoRoom
	}
	return ws.Send(data)
}

// pickRoom returns the next connected room in round-robin order, or nil when
// none is up.
func (t *CupsonlineTransport) pickRoom() *cupsWS {
	n := len(t.wss)
	if n == 0 {
		return nil
	}
	start := int(t.rr.Add(1))
	for i := 0; i < n; i++ {
		ws := t.wss[(start+i)%n]
		if ws.connected.Load() {
			return ws
		}
	}
	return nil
}

func (t *CupsonlineTransport) Receive(callback func([]byte)) {
	t.BaseTransport.Receive(callback)
}

// IsConnected is true while at least one room's channel is up, so the app
// stops claiming "connected" once every room has gone away.
func (t *CupsonlineTransport) IsConnected() bool {
	if !t.BaseTransport.IsConnected() {
		return false
	}
	for _, ws := range t.wss {
		if ws.connected.Load() {
			return true
		}
	}
	return false
}

// reportRooms logs how many rooms are still alive whenever one of them
// closes or comes back.
func (t *CupsonlineTransport) reportRooms() {
	alive := 0
	for _, ws := range t.wss {
		if !ws.dead.Load() {
			alive++
		}
	}
	utils.Infof("[CUPS] Cups: живых комнат %d/%d", alive, len(t.wss))
	if alive == 0 {
		utils.Infof("[ERROR] Cups: все комнаты недоступны - пересоздайте комнаты на ноде (запуск без --url) и обновите строку в профиле")
	}
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
		case <-t.ctx.Done():
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

				utils.Debugf("[CH-%02d %s] tx=%d pkt/s (%.1f KB/s)  rx=%d pkt/s (%.1f KB/s)  drop=%d  reconn=%d  conn=%v",
					i, shortRoom(ws.roomUUID),
					dS/uint64(t.config.StatsInterval.Seconds()),
					float64(dBS)/t.config.StatsInterval.Seconds()/1024,
					dR/uint64(t.config.StatsInterval.Seconds()),
					float64(dBR)/t.config.StatsInterval.Seconds()/1024,
					ws.stats.dropped.Load(),
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

			utils.Debugf("[CUPS-TOTAL] tx=%d pkt/s (%.1f KB/s)  rx=%d pkt/s (%.1f KB/s)  uptime=%v",
				dtS/uint64(t.config.StatsInterval.Seconds()),
				float64(dtBS)/t.config.StatsInterval.Seconds()/1024,
				dtR/uint64(t.config.StatsInterval.Seconds()),
				float64(dtBR)/t.config.StatsInterval.Seconds()/1024,
				time.Since(t.statsStart).Round(time.Second),
			)
		}
	}
}

func (t *CupsonlineTransport) RoomUUIDs() []string {
	out := make([]string, 0, len(t.wss))
	for _, ws := range t.wss {
		out = append(out, ws.roomUUID)
	}
	return out
}

// ---- helpers ----

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
