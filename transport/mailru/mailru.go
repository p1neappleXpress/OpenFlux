// Package mailru implements a transport that tunnels packets through
// Mail.ru's cloud document editor (docs.datacloudmail.ru), the same
// coauthoring backend family as Yandex.Docs. Two peers open the same
// public document and smuggle packets through the "cursor" field of the
// collaborative editing protocol.
package mailru

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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

const mailruUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

var cursorPayloadRe = regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)

// saveChangesMessageTemplate — шаблон saveChanges-сообщения в формате
// веб-клиента Mail.ru: две подстановки %s — UserId и UserShortId в
// excelAdditionalInfo. Остальные поля (op-ы в changes, анкоры, версия)
// оставлены ровно такими, какими их шлёт живой редактор; это шаблон, а не
// «правильная» структура — формат оп закрытый, и мы только подставляем свои
// значения участника.
//
// Поля isCoAuthoring/releaseLocks = false — особенность именно mail.ru
// (у yandex.docs там true/true).
const saveChangesMessageTemplate = `42["message",{"type":"saveChanges","changes":"[\"76;AgAAADEA//8BAOwbfF7pEAAALQEAAAMAAAAAAAAAAAAAAAAAAAAAAAAA9v///xoAAAAyADAAMgA2AC4AMgAuADEALgAyADIANgA4AA==\",\"35;BgAAADYAMgA3AAEAHAABAAAAAAAAAAEAAABhAAAAAAMAAAA=\",\"35;BgAAADYAMgA3AAEAHAABAAAAAQAAAAEAAABzAAAAAAMAAAA=\",\"35;BgAAADYAMgA3AAEAHAABAAAAAgAAAAEAAABkAAAAAAMAAAA=\"]","startSaveChanges":true,"endSaveChanges":true,"isCoAuthoring":false,"isExcel":false,"deleteIndex":null,"excelAdditionalInfo":"{\"UserId\":\"%s\",\"UserShortId\":\"%s\",\"CursorInfo\":\"14;BgAAADYAMgA3AAMAAAA=\"}","unlock":false,"releaseLocks":false}]`

type MailruDocsInfo struct {
	Token        string
	DocKey       string
	WsURL        string
	FileType     string
	DocURL       string
	DocTitle     string
	Permissions  map[string]interface{}
	CallbackURL  string
	EditorUserID string
}

type DocSession struct {
	Info       MailruDocsInfo
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

type MailruDocsTransport struct {
	*transport.BaseTransport

	weblink string
	session *DocSession

	userCounter atomic.Int32
	baseUserID  string

	cookieJar *cookiejar.Jar
	jarMu     sync.RWMutex
}

// NewMailruDocsTransport accepts either a bare weblink ("AbCdEfGh1/IjKlMnOp2")
// or a full public URL ("https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2"),
// normalizing the latter to the former.
func NewMailruDocsTransport(weblink string, config transport.TransportConfig) *MailruDocsTransport {
	t := &MailruDocsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		weblink:       normalizeWeblink(weblink),
	}
	t.baseUserID = randUserID()
	jar, _ := cookiejar.New(nil)
	t.cookieJar = jar
	return t
}

func normalizeWeblink(weblink string) string {
	weblink = strings.TrimSpace(weblink)
	for _, prefix := range []string{
		"https://cloud.mail.ru/public/",
		"http://cloud.mail.ru/public/",
		"https://cloud.mail.ru/",
		"http://cloud.mail.ru/",
	} {
		if strings.HasPrefix(weblink, prefix) {
			return strings.Trim(strings.TrimPrefix(weblink, prefix), "/")
		}
	}
	return weblink
}

func (t *MailruDocsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	t.baseUserID = randUserID()
	utils.SafeGo("mailru.keepAlive", t.keepAliveLoop)
	utils.SafeGo("mailru.editorActivity", t.editorActivityLoop)
	t.connectToDoc(0)

	return nil
}

func (t *MailruDocsTransport) Send(data []byte) error {
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

func (t *MailruDocsTransport) connectToDoc(attempt int) {
	if !t.IsRunning() {
		return
	}

	utils.Debugf("[M-DOCS] connectToDoc attempt %d", attempt)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[PANIC] recovered in mailru.connect: %v", r)
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

		info, err := t.fetchDocInfo(t.weblink)
		if err != nil {
			utils.Debugf("[M-DOCS] fetchDocInfo failed: %v", err)
			if utils.Throttled("m-docs.fetch", time.Minute) {
				utils.Infof("[M-DOCS] cannot open the document: %v; retrying", err)
			}
			t.scheduleReconnect(attempt)
			return
		}

		dialer := websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			NetDialContext: netbind.Wrap(&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", mailruUserAgent)
		headers.Set("Origin", "https://docs.datacloudmail.ru")

		utils.Debugf("[M-DOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.Dial(info.WsURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Debugf("[M-DOCS] WebSocket dial failed (http %d): %v", status, err)
			t.scheduleReconnect(attempt)
			return
		}
		utils.Debugf("[M-DOCS] WebSocket connected")

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
			utils.SafeGo("mailru.writer", t.writerLoop)
		}

		// Auth - fired immediately, same as the Yandex.Docs transport.
		auth1 := fmt.Sprintf(`40{"token":"%s"}`, info.Token)
		session.safeWrite(websocket.TextMessage, []byte(auth1))

		authMsg := map[string]interface{}{
			"type":                "auth",
			"docid":               info.DocKey,
			"documentCallbackUrl": info.CallbackURL,
			"token":               "fghhfgsjdgfjs",
			"user": map[string]interface{}{
				"id":        info.EditorUserID,
				"username":  userID,
				"indexUser": -1,
			},
			"editorType":         0,
			"lastOtherSaveTime":  -1,
			"block":              []interface{}{},
			"documentFormatSave": 65,
			"view":               false,
			"isCloseCoAuthoring": false,
			"openCmd": map[string]interface{}{
				"c":               "open",
				"id":              info.DocKey,
				"userid":          info.EditorUserID,
				"format":          info.FileType,
				"url":             info.DocURL,
				"title":           info.DocTitle,
				"lcid":            25,
				"nobase64":        true,
				"convertToOrigin": ".pdf.xps.oxps.djvu",
			},
			"lang":                  "ru",
			"mode":                  "edit",
			"permissions":           info.Permissions,
			"IsAnonymousUser":       false,
			"timezoneOffset":        -180,
			"coEditingMode":         "fast",
			"jwtOpen":               info.Token,
			"time":                  1000,
			"supportAuthChangesAck": true,
		}
		messagePart, _ := json.Marshal([]interface{}{"message", authMsg})
		session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf("42%s", string(messagePart))))

		connectedAt := time.Now()
		for t.IsRunning() {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[M-DOCS] Read error: %v", err)
				if utils.Throttled("m-docs.drop", time.Minute) {
					utils.Infof("[M-DOCS] connection to the document dropped: %v; reconnecting", err)
				}
				t.SetConnected(false)
				conn.Close()

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

func (t *MailruDocsTransport) writerLoop() {
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
			packet, ok := <-queue
			if !ok {
				return
			}
			pending = packet
		}

		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}

		payload := base64.StdEncoding.EncodeToString(pending)
		msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"18;%s"}]`, payload)
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			utils.Debugf("[M-DOCS] Write error: %v", err)
			time.Sleep(15 * time.Millisecond)
			continue
		}
		pending = nil
	}
}

func (t *MailruDocsTransport) keepAliveLoop() {
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
				utils.Debugf("[M-DOCS] Keep-alive failed, closing the connection to reconnect: %v", err)
				t.SetConnected(false)
				_ = session.Conn.Close()
			}
		}
	}
}

// editorActivityLoop периодически отправляет в документ saveChanges-сообщение
// в формате веб-клиента Mail.ru: значения UserId/UserShortId подставляются
// из текущей сессии, остальное — фиксированный шаблон (см.
// saveChangesMessageTemplate). Задержка между отправками — случайная в
// диапазоне [0.5 с, 5 с], чтобы поток выглядел как правки живого человека:
// ровный по сути, но не метрономом. Сообщение уходит через ту же сессию, что
// writerLoop и keepAliveLoop, и так же терпит переподключение — если сессии
// сейчас нет, итерация просто пропускается.
func (t *MailruDocsTransport) editorActivityLoop() {
	const (
		minDelay = 500 * time.Millisecond
		maxDelay = 5 * time.Second
	)
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
			continue
		}

		msg := buildSaveChanges(session)
		if err := session.safeWrite(websocket.TextMessage, msg); err != nil {
			utils.Debugf("[M-DOCS] saveChanges write error: %v", err)
		}
	}
}

// buildSaveChanges собирает saveChanges-сообщение под конкретную сессию:
// UserId — настоящий id участника (info.EditorUserID, при отсутствии —
// сгенерированный session.UserID), UserShortId — производная от UserId
// (в веб-клиенте Mail.ru это UserId без последнего символа). Остальное — из
// шаблона.
func buildSaveChanges(session *DocSession) []byte {
	userID := session.Info.EditorUserID
	if userID == "" {
		userID = session.UserID
	}
	short := userID
	if len(short) > 1 {
		short = short[:len(short)-1]
	}
	return []byte(fmt.Sprintf(saveChangesMessageTemplate, userID, short))
}

func (t *MailruDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)

	if text == "2" {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte("3"))
		}
		return
	}
	if text == "3" {
		return
	}

	if strings.Contains(text, `"type":"auth"`) && strings.Contains(text, `"result":1`) {
		utils.Debugf("[M-DOCS] Auth OK for user %s", session.UserID)
		return
	}

	if strings.Contains(text, "cursor") {
		for _, base64Str := range cursorPayloads(text) {
			decoded, err := base64.StdEncoding.DecodeString(base64Str)
			if err != nil {
				utils.Debugf("[M-DOCS] Base64 decode error: %v", err)
				continue
			}
			t.RecordReceive(len(decoded))
			t.CallReceive(decoded)
		}
	}
}

// cursorPayloads returns the base64 payload of every cursor entry in a server
// message, in order, without the keep-alive entries.
func cursorPayloads(text string) []string {
	var out []string
	for _, m := range cursorPayloadRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 && m[1] != "---KA---" {
			out = append(out, m[1])
		}
	}
	return out
}

func (t *MailruDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}

	d := reconnectBackoff(next)
	utils.Debugf("[M-DOCS] reconnecting in %v (attempt %d)", d, next)
	time.Sleep(d)
	if !t.IsRunning() {
		return
	}

	t.RecordReconnect()
	t.connectToDoc(next)
}

// reconnectBackoff returns an exponential backoff with jitter, capped at 15s.
func reconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 5 {
		shift = 5
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	d += time.Duration(rand.Int63n(int64(d/2) + 1))
	return d
}

// fetchDocInfo POSTs to Mail.ru's public-document editor API and parses the
// response into the fields needed to open the collaborative WebSocket.
func (t *MailruDocsTransport) fetchDocInfo(weblink string) (MailruDocsInfo, error) {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		var err error
		jar, err = cookiejar.New(nil)
		if err != nil {
			return MailruDocsInfo{}, err
		}
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	reqBody := map[string]string{
		"x-email":  "anonym",
		"public":   "/" + weblink,
		"platform": "desktop_web",
	}
	jsonData, _ := json.Marshal(reqBody)

	apiURL := "https://cloud.mail.ru/api/v4/r7/edit"
	utils.Debugf("[M-DOCS] fetchDocInfo POST %s", apiURL)

	req, _ := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", mailruUserAgent)
	req.Header.Set("X-Api-Version", "4")
	req.Header.Set("Referer", fmt.Sprintf("https://cloud.mail.ru/public/%s?weblink=%s", weblink, weblink))

	resp, err := client.Do(req)
	if err != nil {
		return MailruDocsInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return MailruDocsInfo{}, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	bodyBytes, _ := io.ReadAll(resp.Body)

	var res map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return MailruDocsInfo{}, fmt.Errorf("failed to parse JSON: %w", err)
	}

	apiBase, _ := res["api"].(string)
	token, _ := res["token"].(string)

	document, ok := res["document"].(map[string]interface{})
	if !ok || document == nil {
		return MailruDocsInfo{}, fmt.Errorf("document object missing")
	}

	docKey, _ := document["key"].(string)
	fileType, _ := document["fileType"].(string)
	docURL, _ := document["url"].(string)
	docTitle, _ := document["title"].(string)
	permissions, _ := document["permissions"].(map[string]interface{})
	if permissions == nil {
		permissions = make(map[string]interface{})
	}

	editorConfig, ok := res["editorConfig"].(map[string]interface{})
	if !ok || editorConfig == nil {
		return MailruDocsInfo{}, fmt.Errorf("editorConfig object missing")
	}
	callbackURL, _ := editorConfig["callbackUrl"].(string)

	userObj, _ := editorConfig["user"].(map[string]interface{})
	var editorUserID string
	if userObj != nil {
		editorUserID, _ = userObj["id"].(string)
	}

	wsBase := strings.Replace(apiBase, "https://", "wss://", 1)
	wsURL := fmt.Sprintf("%s/doc/%s/c/?EIO=4&transport=websocket", wsBase, docKey)

	return MailruDocsInfo{
		Token:        token,
		DocKey:       docKey,
		WsURL:        wsURL,
		FileType:     fileType,
		DocURL:       docURL,
		DocTitle:     docTitle,
		Permissions:  permissions,
		CallbackURL:  callbackURL,
		EditorUserID: editorUserID,
	}, nil
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}

// ---- CookieExchanger ----

// FetchCookies returns a snapshot of the transport's current cookie jar as
// name -> value. Used by the exit node to answer a SubtypeCookiesRequest.
func (t *MailruDocsTransport) FetchCookies() (map[string]string, error) {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		return nil, fmt.Errorf("mailru: cookie jar is nil")
	}
	u, err := url.Parse("https://cloud.mail.ru/")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, c := range jar.Cookies(u) {
		out[c.Name] = c.Value
	}
	return out, nil
}

// ApplyCookies replaces the transport's cookie jar with the provided values
// and forces the current session to reconnect.
func (t *MailruDocsTransport) ApplyCookies(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	u, _ := url.Parse("https://cloud.mail.ru/")
	jar, _ := cookiejar.New(nil)
	cookies := make([]*http.Cookie, 0, len(values))
	for k, v := range values {
		cookies = append(cookies, &http.Cookie{Name: k, Value: v, Path: "/"})
	}
	jar.SetCookies(u, cookies)

	t.jarMu.Lock()
	t.cookieJar = jar
	t.jarMu.Unlock()

	utils.Debugf("[M-DOCS] applied %d cookies, forcing reconnect", len(cookies))

	t.Mu.Lock()
	session := t.session
	t.session = nil
	t.SetConnected(false)
	t.Mu.Unlock()
	if session != nil && session.Conn != nil {
		_ = session.Conn.Close()
	}
	if t.IsRunning() {
		t.scheduleReconnect(0)
	}
	return nil
}
