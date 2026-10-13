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

// Mail.ru rejects the old Chrome/137 UA on the WebSocket upgrade with HTTP 409
// on some edge nodes; a current Firefox UA is accepted for both the API and the
// WebSocket. Reported by @Shiller70 (issue #164).
const mailruUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:155.0) Gecko/20100101 Firefox/155.0"

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

	// ready is closed once co-editing writes are allowed on this session:
	// either we have replied authChangesAck to the server's authChanges, or
	// the readiness fallback fired (a legacy server that never sends
	// authChanges). The write paths hold their first co-editing write until
	// it is closed — see markReady and handleMessage.
	ready     chan struct{}
	readyOnce sync.Once
}

// markReady unblocks co-editing writes on this session. Safe to call more than
// once (the authChanges ack and the fallback timer may both fire).
func (s *DocSession) markReady() {
	s.readyOnce.Do(func() { close(s.ready) })
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

// authChangesAckMsg acknowledges the server's authChanges. A participant that
// advertised supportAuthChangesAck (our auth does) must reply with this after
// the server sends authChanges; otherwise the modern Mail.ru/R7 co-editing
// backend keeps the session un-ready and closes the socket (close 1005) on the
// first write. Captured from the live editor (docs.datacloudmail.ru,
// v2026.2.1.2268): the ack carries only its type, in our usual "message" envelope.
const authChangesAckMsg = `42["message",{"type":"authChangesAck"}]`

// authChangesReadyFallback is how long a session waits for the server's
// authChanges before releasing co-editing writes anyway, so a server that does
// not run the authChangesAck handshake is not stalled. A modern server sends
// authChanges within a round-trip, so this only matters for a legacy one.
const authChangesReadyFallback = 3 * time.Second

const mailruSocketIOHandshakeTimeout = 15 * time.Second

// waitMailruSocketIO completes the Engine.IO / Socket.IO handshake before
// editor authentication is sent. Mail.ru currently requires the Engine.IO
// open frame to be consumed before the Socket.IO connect packet is sent.
func waitMailruSocketIO(session *DocSession, token string) error {
	if session == nil || session.Conn == nil {
		return fmt.Errorf("mailru: websocket session is nil")
	}

	conn := session.Conn

	if err := conn.SetReadDeadline(time.Now().Add(mailruSocketIOHandshakeTimeout)); err != nil {
		return fmt.Errorf("mailru: set handshake deadline: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	waitFor := func(match func(string) bool) error {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return err
			}

			if messageType != websocket.TextMessage {
				continue
			}

			text := string(payload)

			// Engine.IO heartbeat may arrive while handshaking.
			if text == "2" {
				if err := session.safeWrite(websocket.TextMessage, []byte("3")); err != nil {
					return fmt.Errorf("mailru: send Engine.IO pong: %w", err)
				}
				continue
			}

			if match(text) {
				return nil
			}

			utils.Debugf("[M-DOCS] handshake: ignoring server frame %q", text)
		}
	}

	// Engine.IO open:
	//   0{"sid":"...", ...}
	if err := waitFor(func(text string) bool {
		return strings.HasPrefix(text, "0{")
	}); err != nil {
		return fmt.Errorf("mailru: wait for Engine.IO open: %w", err)
	}

	utils.Debugf("[M-DOCS] Engine.IO open")

	socketConnect := fmt.Sprintf(`40{"token":"%s"}`, token)

	if err := session.safeWrite(websocket.TextMessage, []byte(socketConnect)); err != nil {
		return fmt.Errorf("mailru: send Socket.IO connect: %w", err)
	}

	// Socket.IO acknowledgement:
	//   40{"sid":"..."}
	if err := waitFor(func(text string) bool {
		return strings.HasPrefix(text, "40")
	}); err != nil {
		return fmt.Errorf("mailru: wait for Socket.IO connect: %w", err)
	}

	utils.Debugf("[M-DOCS] Socket.IO connected")

	return nil
}

type MailruDocsTransport struct {
	*transport.BaseTransport

	weblink string
	session *DocSession

	userCounter atomic.Int32
	baseUserID  string

	cookieJar *cookiejar.Jar
	jarMu     sync.RWMutex

	// reconnecting is set while a scheduled reconnect waits out its backoff:
	// the reader's error and ApplyCookies both schedule one when the cookies
	// are replaced under a live connection, and two reconnects open two
	// sessions to the document.
	reconnecting atomic.Bool
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
	// Data rides the editor chat channel (see writerLoop/handleMessage). The old
	// cursor keepalive and saveChanges "editor activity" are gone: the current
	// Mail.ru server (R7 2026.2.1.2268) validates cursor/saveChanges against the
	// live document model and closes the socket ("access deny", code 4004) on any
	// such write. The Engine.IO ping/pong (handled in handleMessage) keeps the WS up.
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
			// Mail.ru's nginx answers 409 Conflict to an upgrade that does
			// not offer permessage-deflate. gorilla/websocket only sends the
			// Sec-WebSocket-Extensions header when compression is enabled, so
			// without this the handshake was rejected before any auth and the
			// carrier never came up. Reported by @pirsasha (issue #163).
			EnableCompression: true,
			HandshakeTimeout: 15 * time.Second,
			NetDialContext: netbind.WrapFor(t.network(), &net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", mailruUserAgent)
		// Some Mail.ru edge nodes still answer 409 to the upgrade when the
		// request looks less like a browser than the others; a real browser
		// always sends Accept-Language. Reported to still hit iOS (#164).
		headers.Set("Accept-Language", "en-US,en;q=0.9")
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
			ready:      make(chan struct{}),
		}

		if err := waitMailruSocketIO(session, info.Token); err != nil {
			utils.Debugf("[M-DOCS] Socket.IO handshake failed: %v", err)
			_ = conn.Close()
			t.scheduleReconnect(attempt)
			return
		}

		t.Mu.Lock()
		t.session = session
		t.SetConnected(true)
		t.Mu.Unlock()

		if existingSession == nil {
			utils.SafeGo("mailru.writer", t.writerLoop)
		}

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

		// A modern Mail.ru/R7 server replies to this auth (which advertises
		// supportAuthChangesAck) with an authChanges message and only accepts
		// our writes after we ack it; handleMessage does that and marks the
		// session ready. Guard against an older server that never sends
		// authChanges: release the writes after a short grace period so the
		// carrier is not stalled forever waiting for an ack that never comes.
		time.AfterFunc(authChangesReadyFallback, session.markReady)

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

		// Tunnel data rides the document's chat channel: the server broadcasts a
		// chat "message" verbatim to every other participant and does not validate
		// its text against the document model (unlike cursor/saveChanges, which it
		// now rejects). We tag the base64 payload so our frames are told apart from
		// a human's chat and from the server's echo of our own messages.
		payload := chatDataTag + base64.StdEncoding.EncodeToString(pending)
		msg := fmt.Sprintf(`42["message",{"type":"message","message":%q}]`, payload)
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
			// Skip the keepalive until co-editing is ready (authChanges acked /
			// fallback fired): a cursor write before then gets the socket closed.
			select {
			case <-session.ready:
			default:
				continue
			}
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				utils.Debugf("[M-DOCS] Keep-alive failed, closing the connection to reconnect: %v", err)
				t.SetConnected(false)
				_ = session.Conn.Close()
			}
		}
	}
}

// unlockDocumentMsg releases the lock a joining editor puts on this one.
const unlockDocumentMsg = `42["message",{"type":"unLockDocument","unlock":true,"isSave":false,"releaseLocks":false,"deleteIndex":-1}]`
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

		// Skip until co-editing is ready (authChanges acked / fallback fired),
		// so saveChanges never goes out before the server accepts our writes.
		select {
		case <-session.ready:
		default:
			continue
		}

		// A link that only lets the document be read ("edit": false) is not allowed to save: the
		// server closes the connection (no close frame) on the first saveChanges, over and over.
		if !canEdit(session.Info.Permissions) {
			continue
		}

		msg := buildSaveChanges(session)
		if err := session.safeWrite(websocket.TextMessage, msg); err != nil {
			utils.Debugf("[M-DOCS] saveChanges write error: %v", err)
		}
	}
}

// canEdit says whether the document may be edited through the link this session joined with.
// The server reports it in the document's permissions; a document that does not say is treated
// as editable, as the transport always did.
func canEdit(permissions map[string]interface{}) bool {
	edit, ok := permissions["edit"].(bool)
	return !ok || edit
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
	return BuildSaveChanges(userID)
}

// BuildSaveChanges is the saveChanges message for one participant: a pure
// function of the user id, exported so the JS port of this transport can be
// compared with it byte for byte.
func BuildSaveChanges(userID string) []byte {
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

	if !strings.Contains(text, "cursor") && !strings.Contains(text, `"messages"`) {
		utils.Debugf("[M-DOCS] server message: %.300s", text)
	}

	// A second editor joining locks the document on the first one
	// (lockDocument) until that one unlocks it; the server drops a holder
	// that does not within 30s (disconnectReason 4007) and the dropped
	// peer, rejoining, locks it on the other: the carrier went down every
	// half minute while both peers were on it.
	if strings.Contains(text, `"type":"connectState"`) {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte(unlockDocumentMsg))
		}
		return
	}

	// Tunnel data arrives as a chat broadcast the server relays from the other
	// participant:
	//   42["message",{"type":"message","messages":[{message,useridoriginal,…}]}]
	// decodeChatData keeps only our-tagged frames from other participants.
	if strings.Contains(text, `"messages"`) {
		ownID := ""
		if session != nil {
			ownID = session.Info.EditorUserID
		}
		for _, pkt := range decodeChatData(data, ownID) {
			t.RecordReceive(len(pkt))
			t.CallReceive(pkt)
		}
	}
}

// chatDataTag prefixes every tunnel payload carried over the chat channel, so a
// receiver can tell our frames from a human's chat message and skip anything it
// cannot decode as ours.
const chatDataTag = "OFX1"

// decodeChatData extracts tunnel packets from a server chat broadcast
//
//	42["message",{"type":"message","messages":[{"message":"…","useridoriginal":"…"},…]}]
//
// It keeps only entries from other participants (useridoriginal != ownID, since
// the server echoes every chat back to its own sender) whose text carries
// chatDataTag, and base64-decodes the remainder. Human chat and presence entries
// (no tag) are ignored.
func decodeChatData(frame []byte, ownID string) [][]byte {
	i := bytes.IndexByte(frame, '[')
	if i < 0 {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(frame[i:], &arr); err != nil || len(arr) < 2 {
		return nil
	}
	var payload struct {
		Type     string `json:"type"`
		Messages []struct {
			Message        string `json:"message"`
			UserIDOriginal string `json:"useridoriginal"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(arr[1], &payload); err != nil || payload.Type != "message" {
		return nil
	}
	var out [][]byte
	for _, m := range payload.Messages {
		if ownID != "" && m.UserIDOriginal == ownID {
			continue // the server's echo of our own chat
		}
		if !strings.HasPrefix(m.Message, chatDataTag) {
			continue // a human's chat, or a presence entry
		}
		dec, err := base64.StdEncoding.DecodeString(m.Message[len(chatDataTag):])
		if err != nil {
			utils.Debugf("[M-DOCS] chat payload decode error: %v", err)
			continue
		}
		out = append(out, dec)
	}
	return out
}

// DecodeChatData is the exported form of decodeChatData (pure; the JS port is
// compared with it in transport/script/parity_test.go).
func DecodeChatData(frame []byte, ownID string) [][]byte { return decodeChatData(frame, ownID) }

// cursorPayloads returns the base64 payload of every cursor entry in a server
// message, in order, without the keep-alive entries.
func cursorPayloads(text string) []string { return CursorPayloads(text) }

// CursorPayloads is the exported form of cursorPayloads (pure; the JS port is
// compared with it in tests).
func CursorPayloads(text string) []string {
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

	if !t.reconnecting.CompareAndSwap(false, true) {
		return // one is already waiting
	}
	d := reconnectBackoff(next)
	utils.Debugf("[M-DOCS] reconnecting in %v (attempt %d)", d, next)
	time.Sleep(d)
	t.reconnecting.Store(false) // a failed connect schedules the next one itself
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
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second, Transport: netbind.HTTPTransport(t.network())}

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
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
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

// network is the network this carrier dials through (TransportConfig.Network).
func (t *MailruDocsTransport) network() string {
	if t.BaseTransport == nil {
		return ""
	}
	return t.GetConfig().Network
}
