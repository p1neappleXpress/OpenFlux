package yandex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// ErrCaptchaRequired signals that the transport hit a SmartCaptcha challenge
// (showcaptcha?cc=1) which cannot be solved by the internal PoW solver.
// The caller is expected to obtain fresh cookies out of band (e.g. WebView
// on the client) and hand them over via CookieExchanger.ApplyCookies.
var ErrCaptchaRequired = errors.New("yandex docs: captcha required")

// ErrLoginRequired signals a redirect to the passport login page. The
// document is not public from this IP / account.
var ErrLoginRequired = errors.New("yandex docs: login required")

// Precompiled once. cursorPayloadRe in particular runs on every inbound
// message, so compiling it per call (as before) was pure overhead on the hot
// receive path.
var (
	cursorPayloadRe = regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)
	clientConfigRe  = regexp.MustCompile(`<script[^>]*id="client-config"[^>]*>(.*?)</script>`)
)

// ---------- saveChanges: шаблоны и сборка ----------
//
// В дампе веб-клиента каждое поле фиксировано; вариативны только:
//
//   - UserId / UserShortId  — id участника (из editor_config.user.id);
//   - cursorOps             — позиции/состояние каретки (по одному на сдвиг);
//   - appVersion            — версия клиента (в оп-метаданных, зашита в base64).
//
// Формат самих оп — закрытый бинарник редактора. Мы не пытаемся его
// изобрести заново: воспроизводим ровно тот же префикс-entity и
// base64-полезную нагрузку, что и оригинал, а меняем лишь то, что реально
// можно менять, не порождая «мусорных» операций.
//
// modelMetaOp — служебный оп модели: заголовок сессии + версия клиента.
// Он не зависит от пользователя, но привязан к версии editor’а. Менять его
// содержимое нельзя без смены формата; вынесен отдельной константой, чтобы
// при апдейте редактора его можно было заменить одной строкой.
const (
	modelMetaOp  = "76;AgAAADEA//8BAJ+fl7jkAwIALQEAAAMAAAAAAAAAAAAAAAAAAAAAAAAA9v///xoAAAAyADAAMgA2AC4AMgAuADEALgAyADIANgA4AA=="
	entityCursor = "35" // id потока курсора (совпадает с оригиналом)
	cursorStream = "14" // id курсор-потока в CursorInfo
)

// cursorOp собирает один курсор-оп: "<entity>;<base64>". Base64-нагрузка —
// та же форма, что в дампе (4-байтовый заголовок длины строки, затем строка
// userShortID в UTF-16LE, затем фиксированные поля и байт seq), но с
// настоящим userShortID и меняющимся счётчиком seq, поэтому каждое
// сообщение отличается и выглядит живой активностью.
func cursorOp(userShortID string, seq int) string {
	payload := make([]byte, 0, 32)
	payload = append(payload, 0x06, 0x00, 0x00, 0x00) // длина строки = 6 (UTF-16 units)
	for _, r := range userShortID {
		payload = append(payload, byte(r), 0x00)
	}
	payload = append(payload,
		0x01, 0x00, 0x1c, 0x00, // позиция/флаг курсора
		0x01, 0x00, 0x00, 0x00,
		byte(seq), 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	)
	return entityCursor + ";" + base64.StdEncoding.EncodeToString(payload)
}

// cursorInfo строит CursorInfo-строку: "<stream>;<base64>". В base64 лежит
// 6-байтовая строка userShortID (UTF-16LE) и 4 байта позиции; формат ровно
// такой же, как в дампе, но id и позиция — настоящие.
func cursorInfo(userShortID string, seq int) string {
	payload := make([]byte, 0, 16)
	payload = append(payload, 0x06, 0x00, 0x00, 0x00)
	for _, r := range userShortID {
		payload = append(payload, byte(r), 0x00)
	}
	payload = append(payload, 0x03, 0x00, 0x00, 0x00, byte(seq), 0x00)
	return cursorStream + ";" + base64.StdEncoding.EncodeToString(payload)
}

// buildSaveChanges собирает saveChanges-сообщение под конкретную сессию и
// номер итерации: актуальные UserId/UserShortId и слегка меняющиеся позиции
// курсора. Оригинальные entity-префиксы и модель-оп сохранены побайтово;
// меняется только то, что действительно зависит от участника.
func (t *YandexDocsTransport) buildSaveChanges(session *DocSession, seq int) []byte {
	userID := session.Info.EditorUserID
	if userID == "" {
		userID = session.UserID // fallback: псевдо-id, если jwt не дал реального
	}
	short := userID
	if len(short) > 10 {
		short = short[:10]
	}

	changes := []string{
		modelMetaOp,
		cursorOp(short, seq),
		cursorOp(short, seq+1),
		cursorOp(short, seq+2),
	}
	changesJSON, _ := json.Marshal(changes) // ["...","..."]

	excelInfo := map[string]string{
		"UserId":      userID,
		"UserShortId": short,
		"CursorInfo":  cursorInfo(short, seq),
	}
	excelJSON, _ := json.Marshal(excelInfo)

	msg := map[string]interface{}{
		"type":                "saveChanges",
		"changes":             string(changesJSON),
		"startSaveChanges":    true,
		"endSaveChanges":      true,
		"isCoAuthoring":       true,
		"isExcel":             session.Info.IsExcel,
		"deleteIndex":         nil,
		"excelAdditionalInfo": string(excelJSON),
		"unlock":              false,
		"releaseLocks":        true,
	}
	body, _ := json.Marshal([]interface{}{"message", msg})
	return append([]byte("42"), body...)
}

type YandexDocsInfo struct {
	CookieStr    string
	Token        string
	DocID        string
	CallbackURL  string
	UserID       string
	EditorUserID string // editor_config.user.id: настоящий id участника в документе
	IsExcel      bool   // editor_config.document.fileType ∈ {xlsx,xls,xlsm,csv}
	Origin       string
	Host         string
	WsURL        string
	Permissions  map[string]interface{}
	OpenCmd      map[string]interface{}
}

type DocSession struct {
	Info       YandexDocsInfo
	Conn       *websocket.Conn
	WriteQueue chan []byte
	UserID     string
	writeMu    sync.Mutex
}

func (s *DocSession) safeWrite(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// A write into a half-open connection (NAT dropped it, the network
	// changed) would otherwise block until the kernel gives up, minutes.
	_ = s.Conn.SetWriteDeadline(time.Now().Add(docWriteTimeout))
	return s.Conn.WriteMessage(messageType, data)
}

// docWriteTimeout bounds one WebSocket write to the document.
const docWriteTimeout = 20 * time.Second

type YandexDocsTransport struct {
	*transport.BaseTransport

	url     string
	session *DocSession

	userCounter atomic.Int32
	baseUserID  string

	// cookieJar holds the shared cookie jar for all fetchDocInfo / WebSocket
	// dials. It is preserved across reconnects and can be replaced by
	// ApplyCookies (see CookieExchanger).
	cookieJar *cookiejar.Jar
	jarMu     sync.RWMutex

	errNotifier func(err error, transportName, url, reason string)

	// cookiesApplied wakes a scheduleReconnectNoCaptcha wait early. Unbuffered
	// on purpose: a send only succeeds while such a wait is in progress.
	cookiesApplied chan struct{}
}

func NewYandexDocsTransport(url string, config transport.TransportConfig) *YandexDocsTransport {
	t := &YandexDocsTransport{
		BaseTransport:  transport.NewBaseTransport(config),
		url:            url,
		cookiesApplied: make(chan struct{}),
	}
	t.baseUserID = randUserID()
	jar, _ := cookiejar.New(nil)
	t.cookieJar = jar
	return t
}

func (t *YandexDocsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	t.baseUserID = randUserID()
	utils.SafeGo("yandex.keepAlive", t.keepAliveLoop)
	utils.SafeGo("yandex.editorActivity", t.editorActivityLoop)
	t.connectToDoc(0)

	return nil
}

// Stop also closes the document connection. Otherwise the reader sits in
// ReadMessage until the server's next message and then leaves the socket
// open, keeping a participant attached to the document after the transport
// is gone.
func (t *YandexDocsTransport) Stop() error {
	err := t.BaseTransport.Stop()
	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()
	if session != nil && session.Conn != nil {
		_ = session.Conn.Close()
	}
	return err
}

func (t *YandexDocsTransport) Send(data []byte) error {
	if !t.IsConnected() {
		return fmt.Errorf("transport not connected")
	}

	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()

	if session == nil {
		return fmt.Errorf("no active session")
	}

	select {
	case session.WriteQueue <- data:
		t.RecordSend(len(data))
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

func (t *YandexDocsTransport) connectToDoc(attempt int) {
	if !t.IsRunning() {
		return
	}

	utils.Debugf("[YDOCS] connectToDoc attempt ...")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[PANIC] recovered in yandex.connect: %v", r)
			}
		}()
		t.Mu.Lock()
		existingSession := t.session
		t.Mu.Unlock()

		var userID string
		if existingSession != nil {
			userID = existingSession.UserID
		} else {
			suffix := fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
			userID = t.baseUserID + suffix
		}

		info, err := t.fetchDocInfo(t.url, userID)
		if err != nil {
			if errors.Is(err, ErrCaptchaRequired) || errors.Is(err, ErrLoginRequired) {
				utils.Debugf("[YDOCS] fetchDocInfo needs external help: %v", err)
				reason := "smartcaptcha"
				if errors.Is(err, ErrLoginRequired) {
					reason = "login"
				}
				if t.errNotifier != nil {
					t.errNotifier(err, "yandex", t.url, reason)
				}
				t.scheduleReconnectNoCaptcha(attempt)
				return
			}
			utils.Debugf("[YDOCS] fetchDocInfo failed: %v", err)
			if utils.Throttled("ydocs.fetch", time.Minute) {
				utils.Infof("[YDOCS] cannot open the document: %v; retrying", err)
			}
			t.scheduleReconnect(attempt)
			return
		}

		// Hard TCP dial timeout so a stuck connect/DNS to the balancer host
		// can't hang the whole transport (HandshakeTimeout alone proved
		// insufficient on iOS).
		dialer := websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			NetDialContext: netbind.Wrap(&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", "Mozilla/5.0")
		headers.Set("Origin", info.Origin)
		headers.Set("Cookie", info.CookieStr)
		headers.Set("Host", info.Host)

		utils.Debugf("[YDOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.Dial(info.WsURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Debugf("[YDOCS] WebSocket dial failed (http %d): %v", status, err)
			t.scheduleReconnect(attempt)
			return
		}
		utils.Debugf("[YDOCS] WebSocket connected to %s", info.Host)

		writeQueue := make(chan []byte, t.GetConfig().MaxQueueSize)
		if existingSession != nil {
			writeQueue = existingSession.WriteQueue
		}

		session := &DocSession{
			Info:       info,
			Conn:       conn,
			WriteQueue: writeQueue,
			UserID:     userID,
		}

		t.Mu.Lock()
		t.session = session
		t.SetConnected(true)
		t.Mu.Unlock()

		if existingSession == nil {
			utils.SafeGo("yandex.writer", t.writerLoop)
		}

		// Auth - use safeWrite
		auth1 := fmt.Sprintf(`40{"token":"%s"}`, info.Token)
		session.safeWrite(websocket.TextMessage, []byte(auth1))

		authData := map[string]interface{}{
			"type": "auth", "docid": info.DocID, "token": "fghhfgsjdgfjs",
			"user": map[string]interface{}{"id": userID}, "editorType": 0,
			"lastOtherSaveTime": -1, "permissions": info.Permissions,
			"openCmd": info.OpenCmd, "coEditingMode": "fast", "jwtOpen": info.Token,
		}
		messagePart, _ := json.Marshal([]interface{}{"message", authData})
		session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf("42%s", string(messagePart))))

		connectedAt := time.Now()
		for t.IsRunning() {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[YDOCS] Read error: %v", err)
				if utils.Throttled("ydocs.drop", time.Minute) {
					utils.Infof("[YDOCS] connection to the document dropped: %v; reconnecting", err)
				}
				t.SetConnected(false)
				conn.Close()
				// If the session was healthy for a while, treat the next
				// connect as fresh (attempt -1 -> next attempt 0) so backoff
				// doesn't keep growing across normal long-lived reconnects.
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
				return
			}
			t.handleMessage(session, message)
		}
	}()
}

func (t *YandexDocsTransport) writerLoop() {
	// The write queue is created once and preserved across reconnects, so we
	// capture it and block on it instead of polling with a 10ms sleep. The old
	// poll added up to 10ms of latency to every send and woke the CPU 100x/sec
	// while idle.
	var queue chan []byte
	for t.IsRunning() && queue == nil {
		t.Mu.Lock()
		if t.session != nil {
			queue = t.session.WriteQueue
		}
		t.Mu.Unlock()
		if queue == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if queue == nil {
		return
	}

	var pending []byte
	for t.IsRunning() {
		if pending == nil {
			select {
			case packet, ok := <-queue:
				if !ok {
					return
				}
				pending = packet
			case <-t.Done():
				return
			}
		}

		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			// Mid-reconnect: hold the packet and retry rather than drop it.
			time.Sleep(15 * time.Millisecond)
			continue
		}

		payload := base64.StdEncoding.EncodeToString(pending)
		msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"18;%s"}]`, payload)
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			utils.Debugf("[YDOCS] Write error: %v", err)
			time.Sleep(15 * time.Millisecond)
			continue // keep pending; the reconnect will bring up a new conn
		}
		pending = nil
	}
}

func (t *YandexDocsTransport) keepAliveLoop() {
	ticker := time.NewTicker(t.GetConfig().KeepAliveInterval)
	defer ticker.Stop()
	keepAliveMsg := `42["message",{"type":"cursor","cursor":"18;---KA---"}]`

	for t.IsRunning() {
		<-ticker.C
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()

		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				utils.Debugf("[YDOCS] Keep-alive failed, closing the connection to reconnect: %v", err)
				t.SetConnected(false)
				// Close it so the reader, which may sit in ReadMessage on
				// a half-open socket forever, errors out and reconnects.
				_ = session.Conn.Close()
			}
		}
	}
}

// editorActivityLoop периодически отправляет в документ saveChanges-сообщение,
// собранное под текущую сессию (см. buildSaveChanges): актуальные
// UserId/UserShortId и слегка меняющиеся позиции курсора. Задержка между
// отправками — случайная в диапазоне [0.5 с, 5 с], чтобы поток выглядел как
// правки живого человека: ровный по сути, но не метрономом. Сообщение уходит
// через ту же сессию, что writerLoop и keepAliveLoop, и так же терпит
// переподключение — если сессии сейчас нет, итерация просто пропускается.
func (t *YandexDocsTransport) editorActivityLoop() {
	const (
		minDelay = 500 * time.Millisecond
		maxDelay = 5 * time.Second
	)
	seq := 0
	for t.IsRunning() {
		span := int64(maxDelay - minDelay)
		delay := minDelay + time.Duration(rand.Int63n(span+1))

		select {
		case <-time.After(delay):
		case <-t.Done():
			return
		}

		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			continue // mid-reconnect: nothing to write into
		}

		seq++
		msg := t.buildSaveChanges(session, seq)
		if err := session.safeWrite(websocket.TextMessage, msg); err != nil {
			utils.Debugf("[YDOCS] saveChanges write error: %v", err)
			// Держим цикл: следующая итерация попробует снова, а
			// переподключение (keepAliveLoop / reader) поднимет новый conn.
		}
	}
}

func (t *YandexDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)

	if strings.Contains(text, "---KA---") {
		return
	}

	// Socket.IO ping - respond with pong (use safeWrite)
	if text == "2" {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte("3"))
		}
		return
	}
	if text == "3" {
		return
	}

	if strings.Contains(text, "saveChanges") || strings.Contains(text, "cursor") {
		base64Str := t.extractBase64String(text)
		if base64Str == "" {
			return
		}

		decoded, err := base64.StdEncoding.DecodeString(base64Str)
		if err != nil {
			utils.Debugf("[YDOCS] Base64 decode error: %v", err)
			return
		}

		t.RecordReceive(len(decoded))
		t.CallReceive(decoded)
	}
}

func (t *YandexDocsTransport) extractBase64String(response string) string {
	if strings.Contains(response, "saveChanges") {
		marker := `"excelAdditionalInfo":"`
		left := strings.Index(response, marker) + len(marker)
		if left < len(marker) {
			return ""
		}
		right := strings.Index(response[left:], `"`)
		if right == -1 {
			return ""
		}
		return response[left : left+right]
	}

	matches := cursorPayloadRe.FindStringSubmatch(response)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func (t *YandexDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}

	// Back off before retrying so a server that closes us immediately doesn't
	// turn into a tight connect/close loop (previously reconnect was instant).
	d := reconnectBackoff(next)
	utils.Debugf("[YDOCS] reconnecting in %v (attempt %d)", d, next)
	select {
	case <-time.After(d):
	case <-t.Done():
		return
	}
	if !t.IsRunning() {
		return
	}

	t.RecordReconnect()
	t.connectToDoc(next)
}

// SetErrorNotifier installs a callback for out-of-band errors such as
// ErrCaptchaRequired or ErrLoginRequired. Called once by the manager.
func (t *YandexDocsTransport) SetErrorNotifier(fn func(err error, transportName, url, reason string)) {
	t.errNotifier = fn
}

// scheduleReconnectNoCaptcha is called when fetchDocInfo returned a sentinel
// error (ErrCaptchaRequired / ErrLoginRequired). Retrying with a backoff would
// just hit the same captcha again, so we slow down to a fixed long delay and
// rely on external cookie injection to break the cycle.
func (t *YandexDocsTransport) scheduleReconnectNoCaptcha(attempt int) {
	if !t.IsRunning() {
		return
	}
	const longDelay = 30 * time.Second
	utils.Debugf("[YDOCS] external solver needed; waiting %v before next attempt", longDelay)
	select {
	case <-time.After(longDelay):
	case <-t.cookiesApplied:
	case <-t.Done():
		return
	}
	if !t.IsRunning() {
		return
	}
	t.RecordReconnect()
	t.connectToDoc(attempt + 1)
}

// reconnectBackoff returns an exponential backoff with jitter, capped at 30s.
//
// Each reconnect dials a brand new WebSocket, which the doc-collab server
// registers as a brand new participant in the doc's room regardless of
// client-side user-id reuse - a fast connect/close/reconnect loop piles up
// visible "ghost" participants quickly (confirmed by logging the server's
// participant-list messages during a failure streak). The floor here (was
// 500ms) is raised to slow that churn down; this doesn't change steady-state
// throughput since successful connects never hit backoff at all.
func reconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 4 {
		shift = 4
	}
	d := 1500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	// add up to +50% jitter
	d += time.Duration(rand.Int63n(int64(d/2) + 1))
	return d
}

// ---- CookieExchanger ----

// FetchCookies returns a snapshot of the transport's current cookie jar as
// name -> value. Used by the exit node to answer a SubtypeCookiesRequest.
func (t *YandexDocsTransport) FetchCookies() (map[string]string, error) {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		return nil, fmt.Errorf("ydocs: cookie jar is nil")
	}
	// cookiejar.Cookies(u) needs a URL; use the document URL because every
	// cookie we care about was set on that host.
	u := mustParseURL(t.url)
	out := make(map[string]string)
	for _, c := range jar.Cookies(u) {
		out[c.Name] = c.Value
	}
	return out, nil
}

// ApplyCookies replaces the transport's cookie jar with the provided values
// and forces the current session to reconnect so the next fetchDocInfo uses
// the new cookies. It is idempotent.
func (t *YandexDocsTransport) ApplyCookies(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	u := mustParseURL(t.url)
	jar, _ := cookiejar.New(nil)
	cookies := siteCookies(u, values)
	jar.SetCookies(u, cookies)

	t.jarMu.Lock()
	t.cookieJar = jar
	t.jarMu.Unlock()

	utils.Debugf("[YDOCS] applied %d cookies, forcing reconnect", len(cookies))

	// Drop the current session so the next connectToDoc re-runs fetchDocInfo
	// with the new jar.
	t.Mu.Lock()
	session := t.session
	t.session = nil
	t.SetConnected(false)
	t.Mu.Unlock()
	if session != nil && session.Conn != nil {
		_ = session.Conn.Close()
	}
	if t.IsRunning() {
		select {
		case t.cookiesApplied <- struct{}{}:
			// The captcha wait reconnects now; a second reconnect here would
			// open a duplicate session to the document.
		default:
			t.scheduleReconnect(0)
		}
	}
	return nil
}

// ---- fetchDocInfo ----

func (t *YandexDocsTransport) fetchDocInfo(url, userID string) (YandexDocsInfo, error) {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		var err error
		jar, err = cookiejar.New(nil)
		if err != nil {
			return YandexDocsInfo{}, err
		}
	}

	client := &http.Client{
		Jar: jar,
		// НЕ следуем редиректам автоматически — обрабатываем вручную.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 15 * time.Second,
	}

	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0"

	// Явно следуем по редиректам: до 10 хопов.
	currentURL := url
	var resp *http.Response
	var err error

	for hop := 0; hop < 10; hop++ {
		utils.Debugf("[YDOCS] hop %d: GET %s", hop, shortStr(currentURL, 120))

		req, _ := http.NewRequest("GET", currentURL, nil)
		req.Header.Set("User-Agent", ua)
		resp, err = client.Do(req)
		if err != nil {
			return YandexDocsInfo{}, fmt.Errorf("GET %s: %w", currentURL, err)
		}

		utils.Debugf("[YDOCS]   status=%d location=%s",
			resp.StatusCode, shortStr(resp.Header.Get("Location"), 120))

		// 200 — дошли до документа
		if resp.StatusCode == 200 {
			break
		}

		// 3xx — редирект
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			if loc == "" {
				return YandexDocsInfo{}, fmt.Errorf("redirect without Location from %s", currentURL)
			}

			// Second-tier captcha (SmartCaptcha, showcaptcha?cc=1). The PoW
			// solver cannot handle it; signal the caller to fetch fresh
			// cookies out of band.
			if strings.Contains(loc, "showcaptcha") && !strings.Contains(loc, "showcaptchafast") {
				utils.Debugf("[YDOCS] SmartCaptcha detected, external solver required")
				return YandexDocsInfo{}, ErrCaptchaRequired
			}

			// First-tier captcha (PoW, showcaptchafast). Solve and retry
			// the original url.
			if strings.Contains(loc, "showcaptchafast") {
				utils.Debugf("[YDOCS] captcha detected, solving...")
				if _, cerr := solveCaptcha(currentURL, jar, ua); cerr != nil {
					return YandexDocsInfo{}, fmt.Errorf("captcha solve: %w", cerr)
				}
				utils.Debugf("[YDOCS] captcha solved, retrying original url")
				currentURL = url
				continue
			}

			// Login page: not a captcha, not recoverable in-band.
			if strings.Contains(loc, "passport.yandex") {
				return YandexDocsInfo{}, ErrLoginRequired
			}

			// Обычный редирект — идём по нему.
			currentURL = loc
			continue
		}

		// Другой статус — ошибка
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return YandexDocsInfo{}, fmt.Errorf("unexpected status %d at %s", resp.StatusCode, currentURL)
	}

	if resp == nil {
		return YandexDocsInfo{}, fmt.Errorf("no response after redirects")
	}
	if resp.StatusCode != 200 {
		return YandexDocsInfo{}, fmt.Errorf("final status %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	htmlBytes, _ := io.ReadAll(resp.Body)
	html := string(htmlBytes)
	utils.Debugf("[YDOCS] response status=%d finalURL=%s body=%dB",
		resp.StatusCode, resp.Request.URL.String(), len(html))

	var cookies []string
	for _, c := range resp.Cookies() {
		cookies = append(cookies, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	for _, c := range jar.Cookies(resp.Request.URL) {
		cookies = append(cookies, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}

	matches := clientConfigRe.FindStringSubmatch(html)
	if len(matches) < 2 {
		hint := "no client-config script"
		if strings.Contains(html, "passport") || strings.Contains(strings.ToLower(html), "login") {
			hint = "looks like a login page (doc not public?)"
		}
		return YandexDocsInfo{}, fmt.Errorf("config not found: %s (status %d, final %s)",
			hint, resp.StatusCode, resp.Request.URL.String())
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(matches[1]), &config); err != nil {
		return YandexDocsInfo{}, fmt.Errorf("client-config is not valid JSON: %w", err)
	}

	officeAction, ok := config["officeActionData"].(map[string]interface{})
	if !ok || officeAction == nil {
		return YandexDocsInfo{}, fmt.Errorf("officeActionData missing - will reconnect")
	}

	editorConfigRaw, ok := officeAction["editor_config"].(map[string]interface{})
	if !ok || editorConfigRaw == nil {
		return YandexDocsInfo{}, fmt.Errorf("editor_config nil - will reconnect")
	}

	balancerURL, ok := officeAction["balancer_url"].(string)
	if !ok || balancerURL == "" {
		return YandexDocsInfo{}, fmt.Errorf("officeActionData.balancer_url missing - will reconnect")
	}
	host := strings.TrimPrefix(balancerURL, "https://")

	document, ok := editorConfigRaw["document"].(map[string]interface{})
	if !ok || document == nil {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.document missing - will reconnect")
	}

	token, ok := editorConfigRaw["token"].(string)
	if !ok || token == "" {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.token missing - will reconnect")
	}

	docKey, ok := document["key"].(string)
	if !ok || docKey == "" {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.document.key missing - will reconnect")
	}

	perms, _ := document["permissions"].(map[string]interface{})
	if perms == nil {
		perms = make(map[string]interface{})
	}

	// editor_config.user.id — настоящий id участника в документе: под него
	// сервер подписывает presence/курсоры, и именно его нужно нести в
	// saveChanges (иначе auth-сессия и saveChanges разойдутся).
	editorUserID := ""
	if userObj, ok := editorConfigRaw["user"].(map[string]interface{}); ok {
		if s, ok := userObj["id"].(string); ok {
			editorUserID = s
		}
	}
	fileType, _ := document["fileType"].(string)
	isExcel := isExcelFile(fileType)

	return YandexDocsInfo{
		CookieStr:    strings.Join(cookies, "; "),
		Token:        token,
		DocID:        docKey,
		Origin:       balancerURL,
		Host:         host,
		EditorUserID: editorUserID,
		IsExcel:      isExcel,
		WsURL:        fmt.Sprintf("wss://%s/2024.1.1-375/doc/%s/c/?EIO=4&transport=websocket", host, docKey),
		Permissions:  perms,
		OpenCmd: map[string]interface{}{
			"c":      "open",
			"id":     docKey,
			"userid": userID,
			"format": document["fileType"],
			"url":    document["url"],
			"title":  document["title"],
			"lcid":   25,
		},
	}, nil
}

// isExcelFile — таблица ли это, по fileType из editor_config.document.
// От этого зависит поле isExcel в saveChanges.
func isExcelFile(fileType string) bool {
	switch strings.ToLower(strings.TrimPrefix(fileType, ".")) {
	case "xlsx", "xls", "xlsm", "csv":
		return true
	}
	return false
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}

// mustParseURL parses a URL and panics on error. Used only where the input is
// a known-valid document URL.
// siteCookies scopes externally supplied cookies to the document's parent
// domain (disk.yandex.ru -> yandex.ru) instead of host-only: the document
// fetch is redirected across Yandex hosts, and an out-of-band solve (e.g.
// SmartCaptcha's spravka) is issued for .yandex.ru, so a host-only copy
// would never reach the host that actually asked for it.
func siteCookies(u *url.URL, values map[string]string) []*http.Cookie {
	domain := ""
	if u != nil {
		if labels := strings.Split(u.Hostname(), "."); len(labels) >= 3 {
			domain = strings.Join(labels[1:], ".")
		}
	}
	cookies := make([]*http.Cookie, 0, len(values))
	for k, v := range values {
		cookies = append(cookies, &http.Cookie{Name: k, Value: v, Path: "/", Domain: domain})
	}
	return cookies
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}
