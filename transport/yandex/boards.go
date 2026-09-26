package yandex

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
	"openflux/utils"
)

const (
	boardsBase = "boards.yandex.ru"
	boardsUA   = "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/153.0.0.0 Mobile Safari/537.36"
	boardsSocketHostDefault = "socket33.boards.yandex.ru"

	// Engine.io heartbeat. Сервер шлёт pingInterval=25000, pingTimeout=30000.
	// Мы пингуем сами каждые 20с, чтобы NAT не рвал idle-соединение.
	boardsPingInterval = 20 * time.Second

	// Дедлайн чтения в основном цикле. С запасом над boardsPingInterval.
	boardsReadDeadline = 90 * time.Second

	// Таймаут ожидания 431 в handshake.
	boardsHandshakeWait = 15 * time.Second
)

// ---- внутренние типы ----

type boardsInfo struct {
	hash         string
	name         string
	userHash     string // payload.u из JWT
	jwt          string
	cookies      []*http.Cookie
	wsHost       string
	session      string // пустой до participant-connected/431
	dashboard    string
	currentSlide string
}

type boardsSession struct {
	Info    boardsInfo
	Conn    *websocket.Conn
	Queue   chan []byte
	writeMu sync.Mutex
	ack     atomic.Int64

	// participant — тот, что мы отправили в subscribe. Это userHash из JWT.
	participant atomic.Pointer[string]

	// creatorHash — серверный хэш (из participant-connected/431).
	creatorHash atomic.Pointer[string]
}

func (s *boardsSession) safeWrite(msgType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.Conn.WriteMessage(msgType, data)
}

func shortStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *boardsSession) writeEventObj(ns string, obj interface{}) error {
	ack := s.ack.Add(1) - 1
	payload, err := json.Marshal([]interface{}{ns, obj})
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("42%d%s", ack, payload)
	return s.safeWrite(websocket.TextMessage, []byte(msg))
}

func (s *boardsSession) writeRaw(raw string) error {
	return s.safeWrite(websocket.TextMessage, []byte(raw))
}

// ---- транспорт ----

type BoardsTransport struct {
	*transport.BaseTransport

	url string

	session atomic.Pointer[boardsSession]

	onDataMu sync.RWMutex
	onData   func([]byte)

	closeOnce sync.Once
	done      chan struct{}

	cookieJar *cookiejar.Jar
	jarMu     sync.RWMutex

	errNotifier func(err error, transportName, url, reason string)
}

func NewBoardsTransport(rawURL string, config transport.TransportConfig) *BoardsTransport {
	jar, _ := cookiejar.New(nil)
	return &BoardsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		url:           rawURL,
		done:          make(chan struct{}),
		cookieJar:     jar,
	}
}

// SetErrorNotifier installs a callback for out-of-band errors. Called once
// by the manager. Boards currently never returns sentinel errors from
// fetchDocInfo (its captcha path is the old showcaptchafast, which the
// internal PoW solver handles), but the hook is wired for parity with the
// other transports.
func (t *BoardsTransport) SetErrorNotifier(fn func(err error, transportName, url, reason string)) {
	t.errNotifier = fn
}

func (t *BoardsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}
	hash := extractBoardsHash(t.url)
	if hash == "" {
		return fmt.Errorf("boards: no hash in URL %q", t.url)
	}

	name := randomGuestName()
	info, err := t.authorize(hash, name)
	if err != nil {
		return fmt.Errorf("boards auth: %w", err)
	}
	utils.Debugf("[BOARDS] auth OK: hash=%s name=%q userHash=%s dashboard=%q wsHost=%s",
		info.hash, info.name, info.userHash, info.dashboard, info.wsHost)

	t.closeOnce = sync.Once{}
	t.done = make(chan struct{})
	utils.SafeGo("boards.connect", func() { t.connectLoop(info) })

	return nil
}

func (t *BoardsTransport) Stop() error {
	t.closeOnce.Do(func() { close(t.done) })
	if s := t.session.Load(); s != nil && s.Conn != nil {
		s.Conn.Close()
	}
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

func (t *BoardsTransport) Send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	s := t.session.Load()
	if s == nil {
		return fmt.Errorf("boards: no session")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case s.Queue <- cp:
		return nil
	case <-t.done:
		return fmt.Errorf("boards: closed")
	default:
		return fmt.Errorf("boards: queue full")
	}
}

func (t *BoardsTransport) Receive(cb func([]byte)) {
	t.onDataMu.Lock()
	t.onData = cb
	t.onDataMu.Unlock()
}

func (t *BoardsTransport) IsConnected() bool {
	return t.BaseTransport.IsConnected()
}

// ---- авторизация ----
//
// 1) GET /whiteboard/?hash=<hash>   → начальные cookies
//    (может редиректнуть на showcaptchafast — тогда проходим капчу)
// 2) POST /api request-guest-token  → Set-Cookie token_<hash>=<JWT>
// 3) POST /api get-whiteboard-info  → dashboard/current_slide/ws_host

// errCaptchaRequired — sentinel error: сервер требует капчу.
var errCaptchaRequired = fmt.Errorf("captcha required")

// getAllowCaptcha делает GET и, если встречает редирект на showcaptchafast,
// возвращает errCaptchaRequired.
func (t *BoardsTransport) getAllowCaptcha(client *http.Client, u, hash string) error {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", boardsUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://"+boardsBase+"/guest/?hash="+hash)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if strings.Contains(loc, "showcaptchafast") {
			utils.Debugf("[BOARDS] redirect to captcha: %s", shortStr(loc, 100))
			return errCaptchaRequired
		}
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func (t *BoardsTransport) authorize(hash, name string) (boardsInfo, error) {
	jar := t.jar()
	client := &http.Client{
		Jar:     jar,
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	docURL := "https://" + boardsBase + "/whiteboard/?hash=" + hash

	// GET whiteboard — может вернуть 302 на showcaptchafast
	if err := t.getAllowCaptcha(client, docURL, hash); err != nil {
		if err == errCaptchaRequired {
			utils.Debugf("[BOARDS] captcha required, solving...")
			if _, cerr := solveCaptcha(docURL, jar, boardsUA); cerr != nil {
				return boardsInfo{}, fmt.Errorf("captcha solve: %w", cerr)
			}
			utils.Debugf("[BOARDS] captcha solved, re-fetching whiteboard")

			// После капчи повторяем GET /whiteboard — сервер выдаёт
			// свежие cookies, нужные для последующих /api запросов.
			if err := t.getAllowCaptcha(client, docURL, hash); err != nil && err != errCaptchaRequired {
				return boardsInfo{}, fmt.Errorf("GET whiteboard (post-captcha): %w", err)
			}
		} else {
			return boardsInfo{}, err
		}
	}

	if err := t.postAPI(client, hash, "request-guest-token",
		map[string]string{"name": name, "hash": hash}); err != nil {
		return boardsInfo{}, fmt.Errorf("request-guest-token: %w", err)
	}

	u, _ := url.Parse("https://" + boardsBase)
	var jwt string
	for _, c := range jar.Cookies(u) {
		if c.Name == "token_"+hash {
			jwt = c.Value
		}
	}
	if jwt == "" {
		return boardsInfo{}, fmt.Errorf("token_%s not found", hash)
	}
	payload := jwtPayload(jwt)
	userHash, _ := payload["u"].(string)

	state, err := t.getWhiteboardInfo(client, hash)
	if err != nil {
		utils.Debugf("[BOARDS] get-whiteboard-info failed: %v", err)
	}

	var cookies []*http.Cookie
	cookies = append(cookies, jar.Cookies(u)...)

	wsHost := state["ws_host"]
	if wsHost == "" {
		wsHost = boardsSocketHostDefault
	}

	return boardsInfo{
		hash:         hash,
		name:         name,
		userHash:     userHash,
		jwt:          jwt,
		cookies:      cookies,
		wsHost:       wsHost,
		session:      "",
		dashboard:    state["dashboard"],
		currentSlide: state["current_slide"],
	}, nil
}

func (t *BoardsTransport) get(client *http.Client, u, accept, referer string) error {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", boardsUA)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", referer)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (t *BoardsTransport) postAPI(client *http.Client, hash, action string, content interface{}) error {
	raw, _ := json.Marshal(content)
	contentB64 := base64.StdEncoding.EncodeToString(raw)
	form := url.Values{}
	form.Set("action", action)
	form.Set("content", contentB64)

	req, _ := http.NewRequest("POST", "https://"+boardsBase+"/api",
		strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", boardsUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Referer", "https://"+boardsBase+"/guest/?hash="+hash)
	req.Header.Set("Origin", "https://"+boardsBase)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		n := len(body)
		if n > 200 {
			n = 200
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body[:n]))
	}
	return nil
}

func (t *BoardsTransport) getWhiteboardInfo(client *http.Client, hash string) (map[string]string, error) {
	raw, _ := json.Marshal(map[string]string{"hash": hash})
	contentB64 := base64.StdEncoding.EncodeToString(raw)
	form := url.Values{}
	form.Set("action", "get-whiteboard-info")
	form.Set("content", contentB64)

	req, _ := http.NewRequest("POST", "https://"+boardsBase+"/api",
		strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", boardsUA)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Referer", "https://"+boardsBase+"/guest/?hash="+hash)
	req.Header.Set("Origin", "https://"+boardsBase)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var info struct {
		Presentation struct {
			Properties struct {
				CurrentSlide string `json:"current_slide"`
			} `json:"properties"`
			Items string `json:"items"`
		} `json:"presentation"`
		SocketServers []struct {
			IP string `json:"ip"`
		} `json:"socket_servers"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if info.Presentation.Properties.CurrentSlide != "" {
		out["current_slide"] = info.Presentation.Properties.CurrentSlide
		out["dashboard"] = info.Presentation.Properties.CurrentSlide
	}
	if info.Presentation.Items != "" {
		if rawItems, err := base64.StdEncoding.DecodeString(info.Presentation.Items); err == nil {
			var arr []map[string]interface{}
			if json.Unmarshal(rawItems, &arr) == nil && len(arr) > 0 {
				if h, _ := arr[0]["hash"].(string); h != "" && out["dashboard"] == "" {
					out["dashboard"] = h
					out["current_slide"] = h
				}
			}
		}
	}
	if len(info.SocketServers) > 0 {
		out["ws_host"] = info.SocketServers[0].IP
	}
	return out, nil
}

func jwtPayload(jwt string) map[string]interface{} {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	pad := (4 - len(parts[1])%4) % 4
	b := parts[1] + strings.Repeat("=", pad)
	raw, err := base64.URLEncoding.DecodeString(b)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func extractBoardsHash(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("hash")
}

func randomGuestName() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("guest_%06x", time.Now().UnixNano()&0xffffff)
	}
	return "guest_" + hex.EncodeToString(b[:])
}

func randomHex(n int) string {
	buf := make([]byte, n/2)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%0*x", n, mrand.Int63())
	}
	return hex.EncodeToString(buf)
}

// ---- WS lifecycle ----

func (t *BoardsTransport) connectLoop(info boardsInfo) {
	attempt := 0
	for {
		select {
		case <-t.done:
			return
		default:
		}
		if err := t.connectAndServe(info); err != nil {
			utils.Debugf("[BOARDS] ws error: %v", err)
		}
		t.SetConnected(false)
		select {
		case <-t.done:
			return
		case <-time.After(reconnectBackoffBoards(attempt)):
		}
		attempt++
		if attempt > 10 {
			attempt = 10
		}
	}
}

func reconnectBackoffBoards(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 4 {
		shift = 4
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	d += time.Duration(mrand.Int63n(int64(d/2) + 1))
	return d
}

func (t *BoardsTransport) connectAndServe(info boardsInfo) error {
	wsURL := fmt.Sprintf("wss://%s/socket.io/?EIO=4&transport=websocket", info.wsHost)

	header := http.Header{}
	header.Set("User-Agent", boardsUA)
	header.Set("Origin", "https://"+boardsBase)
	header.Set("Accept-Language", "en-US,en;q=0.9")

	var cookieParts []string
	for _, c := range info.cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	if !strings.Contains(strings.Join(cookieParts, ";"), "token_"+info.hash) {
		cookieParts = append(cookieParts, "token_"+info.hash+"="+info.jwt)
	}
	header.Set("Cookie", strings.Join(cookieParts, "; "))

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	utils.Debugf("[BOARDS] dial %s", wsURL)
	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return fmt.Errorf("dial %s (http %d): %w", wsURL, status, err)
	}
	utils.Debugf("[BOARDS] WS connected: %s", info.wsHost)

	participant := info.userHash
	creator := info.userHash
	sess := &boardsSession{
		Info:  info,
		Conn:  conn,
		Queue: make(chan []byte, t.GetConfig().MaxQueueSize),
	}
	sess.participant.Store(&participant)
	sess.creatorHash.Store(&creator)
	t.session.Store(sess)

	if err := t.handshake(sess); err != nil {
		conn.Close()
		t.session.Store(nil)
		return fmt.Errorf("handshake: %w", err)
	}

	_ = conn.SetReadDeadline(time.Time{})

	t.SetConnected(true)
	utils.SafeGo("boards.writer", func() { t.writerLoop(sess) })

	kaStop := make(chan struct{})
	utils.SafeGo("boards.keepalive", func() { t.keepAliveLoop(sess, kaStop) })
	utils.SafeGo("boards.ping", func() { t.pingLoop(sess, kaStop) })
	defer close(kaStop)

	for {
		select {
		case <-t.done:
			return nil
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(boardsReadDeadline))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(boardsReadDeadline))

		t.handleMessage(sess, msg)
	}
}

func (t *BoardsTransport) handshake(sess *boardsSession) error {
	conn := sess.Conn
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	if _, err := readRaw(conn); err != nil {
		return fmt.Errorf("engine.io hello: %w", err)
	}

	if err := sess.writeRaw("40"); err != nil {
		return err
	}
	if _, err := readRaw(conn); err != nil {
		return fmt.Errorf("socket.io connect ack: %w", err)
	}

	if err := sess.writeEventObj("im", map[string]interface{}{
		"operation": "subscribe", "user": nil,
	}); err != nil {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		m, err := readRaw(conn)
		if err != nil {
			return fmt.Errorf("wait subscribed: %w", err)
		}
		if bytes.Contains(m, []byte(`"subscribed"`)) {
			break
		}
	}

	part := *sess.participant.Load()
	utils.Debugf("[BOARDS] subscribe-slide-dashboard participant=%s", shortStr(part, 8))
	if err := t.sendSubscribe(sess, part); err != nil {
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(boardsHandshakeWait))
	deadline = time.Now().Add(boardsHandshakeWait)
	for time.Now().Before(deadline) {
		m, err := readRaw(conn)
		if err != nil {
			return fmt.Errorf("wait 431: %w", err)
		}
		t.handleMessage(sess, m)
		if bytes.HasPrefix(m, []byte("431[")) {
			break
		}
		if bytes.Contains(m, []byte(`"subscribed":true`)) &&
			bytes.Contains(m, []byte(`"dashboard_link"`)) {
			break
		}
	}

	_ = conn.SetReadDeadline(time.Time{})
	utils.Debugf("[BOARDS] handshake done")
	return nil
}

func readRaw(conn *websocket.Conn) ([]byte, error) {
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func (t *BoardsTransport) sendSubscribe(sess *boardsSession, participant string) error {
	data := map[string]interface{}{
		"session":      sess.Info.session,
		"dashboard":    sess.Info.dashboard,
		"presentation": sess.Info.hash,
		"properties": map[string]interface{}{
			"guest_mode":                     true,
			"guest_role":                     1,
			"guest_password":                 nil,
			"guest_password_expiration_date": nil,
			"current_slide":                  sess.Info.currentSlide,
		},
		"participant_team_role": -1,
		"participant":           participant,
		"options": map[string]interface{}{
			"type": "landing",
			"participant": map[string]interface{}{
				"hash":         participant,
				"partner":      "yandex",
				"userHash":     sess.Info.userHash,
				"name":         sess.Info.name,
				"additional":   map[string]interface{}{"guest": true},
				"module":       "yandex",
				"presentation": sess.Info.hash,
				"identityCandidates": map[string]interface{}{
					"uidHash":    nil,
					"legacyHash": participant,
					"uid":        nil,
					"partner":    "yandex",
				},
				"module_type":            "yandex",
				"participantCaptionName": sess.Info.name,
			},
			"intermediate": "",
			"device": map[string]interface{}{
				"screen":              "674 x 619",
				"screen_width":        674,
				"screen_height":       619,
				"browser":             "Chrome",
				"browserVersion":      "153.0.0.0",
				"browserMajorVersion": 153,
				"mobile":              true,
				"os":                  "Android",
				"osVersion":           "15",
				"osMajorVersion":      15,
				"cookies":             true,
				"flashVersion":        "no check",
				"agent":               "Chrome",
				"appVersion":          boardsUA,
				"userAgent":           boardsUA,
				"appName":             "Netscape",
				"platform":            "MacIntel",
			},
		},
	}
	return sess.writeEventObj("dashboard", map[string]interface{}{
		"action":      "subscribe-slide-dashboard",
		"data":        data,
		"participant": participant,
	})
}

// ---- отправка ----
//
// Канал — notify-position. Формат (объект, как в оригинале):
//
//   42N["dashboard",{
//       "action":"notify-position",
//       "data":{
//           "position":{"x":"<base64_payload>","y":123.0},
//           "vpt":{"translate":{"x":0,"y":0},"scale":1,"whyrugay":1}
//       },
//       "participant":"<participant_hash>"
//   }]
//
// payload — base64 от tunnel-пакета, кладётся в data.position.x.

func (t *BoardsTransport) writerLoop(sess *boardsSession) {
	queue := sess.Queue
	for {
		select {
		case <-t.done:
			return
		case pkt := <-queue:
			if err := t.sendNotifyPosition(sess, pkt); err != nil {
				utils.Debugf("[BOARDS] notify-position: %v", err)
			} else {
				t.RecordSend(len(pkt))
			}
		}
	}
}

// sendNotifyPosition — точная копия формата из Python-скрипта:
//
//	cmd_pos(x, y) -> send_dashboard("notify-position",
//	    {"position": {"x": x, "y": y},
//	     "vpt": {"translate": {"x": 0, "y": 0}, "scale": 1}})
//
// send_dashboard добавляет participant в envelope.
// payload кладём в position.x как base64-строку, position.y = 123.0,
// vpt.whyrugay = 1 (как в оригинале).
func (t *BoardsTransport) sendNotifyPosition(sess *boardsSession, pkt []byte) error {
	b64 := base64.StdEncoding.EncodeToString(pkt)

	data := map[string]interface{}{
		"position": map[string]interface{}{
			"x": b64,
			"y": 123.0,
		},
		"vpt": map[string]interface{}{
			"translate": map[string]interface{}{"x": 0, "y": 0},
			"scale":     1,
			"whyrugay":  1,
		},
	}

	obj := map[string]interface{}{
		"action":      "notify-position",
		"data":        data,
		"participant": *sess.participant.Load(),
	}
	return sess.writeEventObj("dashboard", obj)
}

// ---- keepalive ----

func (t *BoardsTransport) keepAliveLoop(sess *boardsSession, stop chan struct{}) {
	tick := time.NewTicker(boardsPingInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.done:
			return
		case <-tick.C:
			obj := map[string]interface{}{
				"action":      "heartbeat",
				"data":        map[string]interface{}{},
				"participant": *sess.participant.Load(),
			}
			if err := sess.writeEventObj("dashboard", obj); err != nil {
				utils.Debugf("[BOARDS] heartbeat: %v", err)
				return
			}
		}
	}
}

func (t *BoardsTransport) pingLoop(sess *boardsSession, stop chan struct{}) {
	tick := time.NewTicker(boardsPingInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.done:
			return
		case <-tick.C:
			if err := sess.writeRaw("2"); err != nil {
				utils.Debugf("[BOARDS] engine.io ping: %v", err)
				return
			}
		}
	}
}

// ---- приём ----

func (t *BoardsTransport) handleMessage(sess *boardsSession, raw []byte) {
	if len(raw) == 1 && raw[0] == '2' {
		utils.Debugf("[BOARDS] ping -> pong")
		_ = sess.writeRaw("3")
		return
	}
	if len(raw) == 1 && raw[0] == '3' {
		return
	}

	if bytes.HasPrefix(raw, []byte("43")) {
		t.handle431(sess, raw)
		return
	}
	if !bytes.HasPrefix(raw, []byte("42[")) {
		return
	}
	idx := bytes.IndexByte(raw, '[')
	if idx < 0 {
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw[idx:], &arr); err != nil {
		return
	}
	if len(arr) < 2 {
		return
	}

	var envelope struct {
		Action      string          `json:"action"`
		Data        json.RawMessage `json:"data"`
		Participant string          `json:"participant"`
	}
	if err := json.Unmarshal(arr[1], &envelope); err != nil {
		return
	}

	switch envelope.Action {
	case "participant-connected":
		t.handleParticipantConnected(sess, envelope.Data)
	case "notify-position":
		t.handleNotifyPosition(sess, envelope.Data, envelope.Participant)
	case "server-modify-objects", "modify-objects":
		t.handleServerModifyObjects(sess, envelope.Data, envelope.Action)
	}
}

func (t *BoardsTransport) handleParticipantConnected(sess *boardsSession, raw json.RawMessage) {
	var d struct {
		Participant struct {
			Hash    string `json:"hash"`
			Session string `json:"session"`
			Name    string `json:"name"`
		} `json:"participant"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return
	}
	utils.Debugf("[BOARDS] participant-connected: name=%q hash=%s session=%s",
		d.Participant.Name, shortStr(d.Participant.Hash, 8), shortStr(d.Participant.Session, 8))

	if d.Participant.Session != "" && sess.Info.session == "" {
		sess.Info.session = d.Participant.Session
	}

	if d.Participant.Name == sess.Info.name && d.Participant.Hash != "" {
		h := d.Participant.Hash
		sess.creatorHash.Store(&h)
		utils.Debugf("[BOARDS] creatorHash from participant-connected: %s", shortStr(h, 8))
	}
}

// handleNotifyPosition — основной канал приёма. Пробуем сначала объект
// (position.x), затем массив (data[4]).
//
// Объект:
//
//	data.position.x — base64
//
// Массив:
//
//	data[2] — имя отправителя, data[4] — base64
//
// Своё эхо фильтруем по:
//   - envelope.participant == наш participant (для объекта)
//   - data[2] == наше имя (для массива)
func (t *BoardsTransport) handleNotifyPosition(sess *boardsSession, raw json.RawMessage, envelopePart string) {
	// Сначала пробуем объект (position.x)
	var objForm struct {
		Position struct {
			X json.RawMessage `json:"x"`
			Y json.RawMessage `json:"y"`
		} `json:"position"`
	}
	if err := json.Unmarshal(raw, &objForm); err == nil {
		var xStr string
		if err := json.Unmarshal(objForm.Position.X, &xStr); err == nil && xStr != "" {
			// Своё эхо по participant в envelope
			myPart := *sess.participant.Load()
			if envelopePart != "" && envelopePart == myPart {
				return
			}
			decoded, derr := base64.StdEncoding.DecodeString(xStr)
			if derr != nil || len(decoded) == 0 {
				return
			}
			utils.Debugf("[BOARDS<-] notify-position obj from=%s pktlen=%d",
				shortStr(envelopePart, 8), len(decoded))
			t.RecordReceive(len(decoded))
			t.onDataMu.RLock()
			cb := t.onData
			t.onDataMu.RUnlock()
			if cb != nil {
				cb(decoded)
			}
			return
		}
	}

	// Массив (data[2]=name, data[4]=base64)
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return
	}
	if len(arr) < 5 {
		return
	}
	var sender string
	_ = json.Unmarshal(arr[2], &sender)
	if sender == sess.Info.name {
		return
	}
	var b64 string
	if err := json.Unmarshal(arr[4], &b64); err != nil || b64 == "" {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(decoded) == 0 {
		return
	}
	utils.Debugf("[BOARDS<-] notify-position arr from=%q pktlen=%d", sender, len(decoded))
	t.RecordReceive(len(decoded))
	t.onDataMu.RLock()
	cb := t.onData
	t.onDataMu.RUnlock()
	if cb != nil {
		cb(decoded)
	}
}

func (t *BoardsTransport) handleServerModifyObjects(sess *boardsSession, raw json.RawMessage, action string) {
	var d struct {
		Dashboard string `json:"dashboard"`
		Name      string `json:"name"`
		Session   string `json:"session"`
		Objects   []struct {
			Attributes map[string]interface{} `json:"_attributes_"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return
	}
	if len(d.Objects) == 0 {
		return
	}

	myPart := *sess.participant.Load()
	myUser := sess.Info.userHash
	myName := sess.Info.name

	for _, o := range d.Objects {
		val, _ := o.Attributes["value"].(string)
		if val == "" {
			continue
		}
		id, _ := o.Attributes["id"].(string)
		creator, _ := o.Attributes["creatorHash"].(string)
		typ, _ := o.Attributes["type"].(string)

		if creator != "" && (creator == myPart || creator == myUser) {
			utils.Debugf("[BOARDS] %s: own echo (creator=%s), skip",
				action, shortStr(creator, 8))
			continue
		}
		if d.Name != "" && d.Name == myName {
			utils.Debugf("[BOARDS] %s: own echo (name=%q), skip", action, d.Name)
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(val)
		if err != nil || len(decoded) == 0 {
			utils.Debugf("[BOARDS] %s: non-base64 value id=%s type=%s, skip",
				action, shortStr(id, 8), typ)
			continue
		}

		utils.Debugf("[BOARDS<-] %s from=%q creator=%s id=%s pktlen=%d",
			action, d.Name, shortStr(creator, 8), shortStr(id, 8), len(decoded))

		t.RecordReceive(len(decoded))
		t.onDataMu.RLock()
		cb := t.onData
		t.onDataMu.RUnlock()
		if cb != nil {
			cb(decoded)
		}
	}
}

func (t *BoardsTransport) handle431(sess *boardsSession, raw []byte) {
	idx := bytes.IndexByte(raw, '[')
	if idx < 0 {
		return
	}
	body := raw[idx:]
	if !bytes.Contains(body, []byte(`"dashboard_link"`)) &&
		!bytes.Contains(body, []byte(`"participantHash"`)) &&
		!bytes.Contains(body, []byte(`"creatorHash"`)) {
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err != nil {
		return
	}
	if len(arr) == 0 {
		return
	}
	var snap struct {
		Participant struct {
			Hash            string `json:"hash"`
			Session         string `json:"session"`
			ParticipantHash string `json:"participantHash"`
			CreatorHash     string `json:"creatorHash"`
			DashboardLink   struct {
				Session   string `json:"session"`
				Dashboard string `json:"dashboard"`
			} `json:"dashboard_link"`
		} `json:"participant"`
	}
	if err := json.Unmarshal(arr[0], &snap); err != nil {
		return
	}

	if snap.Participant.Session != "" && sess.Info.session == "" {
		sess.Info.session = snap.Participant.Session
	}
	if snap.Participant.DashboardLink.Session != "" && sess.Info.session == "" {
		sess.Info.session = snap.Participant.DashboardLink.Session
	}
	if snap.Participant.DashboardLink.Dashboard != "" && sess.Info.dashboard == "" {
		sess.Info.dashboard = snap.Participant.DashboardLink.Dashboard
		sess.Info.currentSlide = sess.Info.dashboard
		utils.Debugf("[BOARDS] dashboard from 431: %s", shortStr(sess.Info.dashboard, 8))
	}

	if snap.Participant.CreatorHash != "" {
		ch := snap.Participant.CreatorHash
		sess.creatorHash.Store(&ch)
		utils.Debugf("[BOARDS] creatorHash from 431: %s", shortStr(ch, 8))
	}
	if snap.Participant.ParticipantHash != "" {
		ch := snap.Participant.ParticipantHash
		sess.creatorHash.Store(&ch)
		utils.Debugf("[BOARDS] creatorHash from 431/participantHash: %s", shortStr(ch, 8))
	}
}

// jar returns the transport's shared cookie jar (never nil).
func (t *BoardsTransport) jar() *cookiejar.Jar {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		jar, _ = cookiejar.New(nil)
		t.jarMu.Lock()
		t.cookieJar = jar
		t.jarMu.Unlock()
	}
	return jar
}

// ---- CookieExchanger ----

// FetchCookies returns a snapshot of the transport's current cookie jar as
// name -> value for boards.yandex.ru.
func (t *BoardsTransport) FetchCookies() (map[string]string, error) {
	u, err := url.Parse("https://" + boardsBase + "/")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, c := range t.jar().Cookies(u) {
		out[c.Name] = c.Value
	}
	return out, nil
}

// ApplyCookies replaces the transport's cookie jar and forces the current WS
// session to reconnect.
func (t *BoardsTransport) ApplyCookies(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	u, _ := url.Parse("https://" + boardsBase + "/")
	jar, _ := cookiejar.New(nil)
	cookies := make([]*http.Cookie, 0, len(values))
	for k, v := range values {
		cookies = append(cookies, &http.Cookie{Name: k, Value: v, Path: "/"})
	}
	jar.SetCookies(u, cookies)

	t.jarMu.Lock()
	t.cookieJar = jar
	t.jarMu.Unlock()

	utils.Debugf("[BOARDS] applied %d cookies, forcing reconnect", len(cookies))

	if s := t.session.Load(); s != nil && s.Conn != nil {
		_ = s.Conn.Close()
	}
	return nil
}
