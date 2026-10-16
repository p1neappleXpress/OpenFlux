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
	"os"
	"regexp"
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

	// authed is closed once the server has accepted this connection's editor
	// auth (it sends type:"auth" result:1). A make-before-break rotation waits
	// for it before moving outgoing traffic onto the new connection, so a packet
	// is never sent on a connection the server will not yet relay (which would
	// otherwise be lost until the Session ARQ retransmits a whole RTO later — a
	// visible stall). See markAuthed and rotateCarrier.
	authed     chan struct{}
	authedOnce sync.Once

	// retired is set once a make-before-break rotation has finished with this
	// session (after the brief receive-overlap grace). Its reader then stops
	// delivering and exits silently on close without scheduling a reconnect. The
	// multi-identity echo filter (MailruDocsTransport.ourIDs) lets the old reader
	// keep delivering safely during the grace, so there is no receive gap.
	retired atomic.Bool

	// sentCount is the number of chat frames written on this connection. The R7
	// server cuts an anonymous editor's socket with disconnectReason 4004
	// ("access deny") after a per-connection chat-message budget is spent —
	// measured live at ~160-220 messages, independent of size, so under load it
	// is reached in tens of seconds, before the ~45s time kick. writerLoop
	// rotates make-before-break once this reaches mailruRotateMsgs (well under
	// the budget), so the cut never happens. It also lets a drop be attributed
	// to the message budget (readLoop logs the count at the drop).
	sentCount atomic.Int64

	// active marks the one session whose reader delivers received frames.
	// Exactly one is active at a time, so a rotation never delivers a frame
	// twice (two readers delivering the same server broadcast would ACK it
	// twice, and the duplicate ACKs make the far side fast-retransmit). The new
	// connection reads from the moment it is dialed — to catch its auth and any
	// early frames — but only delivers once it is installed as active; the old
	// one stops delivering at that same instant. installSession sets it.
	active atomic.Bool
}

// markReady unblocks co-editing writes on this session. Safe to call more than
// once (the authChanges ack and the fallback timer may both fire).
func (s *DocSession) markReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

// markAuthed signals that the server accepted this connection's editor auth.
// Safe to call more than once.
func (s *DocSession) markAuthed() {
	s.authedOnce.Do(func() { close(s.authed) })
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

	// recvCh decouples the single WebSocket read goroutine from downstream
	// delivery. handleMessage runs on that goroutine; calling CallReceive
	// directly made a downstream stall under heavy receive load block the
	// reader, so the next Engine.IO ping went unread and unanswered and the R7
	// server dropped the socket at its ~45s ping timeout (close 1005). The
	// reader now pongs at once and hands data here; a worker drains it into
	// CallReceive. A full channel drops the packet (the Session's ARQ recovers
	// it) rather than stalling the reader.
	recvCh chan []byte

	// sessionStart is the UnixNano at which the current active session was
	// installed. rotationLoop uses it to rotate the carrier by connection age
	// (reset on every reconnect and rotation), so rotation stays aligned to the
	// live connection rather than to process start.
	sessionStart atomic.Int64

	// rotating guards against launching a second make-before-break rotation
	// while one is already in flight.
	rotating atomic.Bool

	// ourIDs holds every anonymous editor id this transport has authenticated as
	// (one per carrier connection; the server assigns a fresh one each time).
	// The receive path skips any chat frame whose useridoriginal is in this set
	// — the server echoes our own frames back, and during a rotation's overlap
	// the old connection also sees the new connection's echoes. Without tracking
	// all of them, the old reader would re-deliver our own egress as ingress.
	ourIDs sync.Map
}

// mailruRecvQueue bounds the decoupling receive buffer (whole batches, so kept
// modest): it absorbs delivery jitter, and a genuine downstream stall fills it
// and drops rather than ever blocking the reader.
const mailruRecvQueue = 256

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

	// Drain received tunnel packets off the read goroutine so a slow downstream
	// never blocks the reader (and thus the Engine.IO pong). See recvCh.
	t.recvCh = make(chan []byte, mailruRecvQueue)
	utils.SafeGo("mailru.recv", func() {
		for {
			select {
			case pkt := <-t.recvCh:
				t.CallReceive(pkt)
			case <-t.Done():
				return
			}
		}
	})

	t.baseUserID = randUserID()
	// Data rides the editor chat channel (see writerLoop/handleMessage). The old
	// cursor keepalive and saveChanges "editor activity" are gone: the current
	// Mail.ru server (R7 2026.2.1.2268) validates cursor/saveChanges against the
	// live document model and closes the socket ("access deny", code 4004) on any
	// such write. The Engine.IO ping/pong (handled in handleMessage) keeps the WS up.
	t.connectToDoc(0)

	// Preemptively rotate the carrier before the server's ~45s drop under load
	// (make-before-break, no tunnel gap). See rotationLoop / rotateCarrier.
	if mailruRotateInterval > 0 {
		utils.SafeGo("mailru.rotation", t.rotationLoop)
	}

	return nil
}

// mailruPreferredBatchBytes is the default coalescing batch size for the chat
// carrier. The R7 server cuts an anonymous editor connection with 4004 after
// ~12 chat messages regardless of their size, so a large batch fits far more
// tunnel data into that fixed message budget before the reconnect. Measured
// end-to-end (d14 exit <-> 212 client, cachefly 10 MB via socks5h): the 8 KB
// default gave the old tens-of-KB/s; ~500 KB reaches ~860 KB/s per burst, while
// 1 MB is slower and far more variable (a bigger loss unit per 4004 cut).
// OPENFLUX_BATCH_BYTES overrides this (see transport.BatchSizeHinter).
const mailruPreferredBatchBytes = 500_000

// PreferredBatchBytes implements transport.BatchSizeHinter.
func (t *MailruDocsTransport) PreferredBatchBytes() int { return mailruPreferredBatchBytes }

// defaultMailruMsgIntervalMs is the minimum gap between chat writes on one
// carrier connection. The R7 4004 budget behaves like a message token bucket
// (capacity ~80-90, refill ~2/s, measured live): a steady trickle is fine, but
// a BURST empties it in a few seconds and the server cuts the socket (4004).
// The burst source under load is the Session ARQ's retransmits — after any
// hiccup it refills the write queue at tens of messages/s. Capping the write
// rate (200 ms = 5/s) flattens that burst so the bucket is never drained faster
// than a make-before-break rotation (mailruRotateMsgs) can swap in a fresh
// connection, which is what finally stopped the exit's 4004 churn under an 8x
// download. 5/s still far exceeds the real send rate the server throttle allows
// (~1-2 x 500 KB batches/s), so it does not cap throughput. 0 disables pacing.
// OPENFLUX_MAILRU_MSG_INTERVAL_MS overrides it.
const defaultMailruMsgIntervalMs = 200

var mailruMsgInterval = func() time.Duration {
	n := defaultMailruMsgIntervalMs
	if v := os.Getenv("OPENFLUX_MAILRU_MSG_INTERVAL_MS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			n = parsed
		}
	}
	return time.Duration(n) * time.Millisecond
}()

// defaultMailruRotateMs is the connection age at which the carrier WebSocket is
// preemptively rotated make-before-break (see rotateCarrier/rotationLoop). The
// R7 server drops an anonymous editor's socket at a fixed age under load —
// ~45s, matching Engine.IO pingInterval 25s + pingTimeout 20s — and that drop
// stalls the single-lane tunnel ("отваливается под нагрузкой"). Neither send
// pacing nor decoupling the reader stopped it (the receive-queue drops stayed
// 0, so the reader was never blocked), which points at the server kick rather
// than a client stall. Rotating a fresh connection in before the old one is
// ever kicked keeps every connection well under that age, so the tunnel never
// waits on the kick. 25s leaves a wide margin under ~45s (room for a slow or
// retried rotation to finish before the kick) while keeping the editor-API
// connect rate low (~2-3/min) to stay under its throttle. rotationLoop retries
// a failed rotation every couple of seconds, so a transient API hiccup does not
// let the old connection reach the kick. 0 disables rotation.
// OPENFLUX_MAILRU_ROTATE_MS overrides it.
const defaultMailruRotateMs = 25000

var mailruRotateInterval = func() time.Duration {
	n := defaultMailruRotateMs
	if v := os.Getenv("OPENFLUX_MAILRU_ROTATE_MS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			n = parsed
		}
	}
	return time.Duration(n) * time.Millisecond
}()

// mailruRotateAuthWait bounds how long a rotation waits for the server to accept
// the new connection's auth before swapping sends onto it. A healthy auth
// round-trip is well under a second; the cap only guards against a stuck new
// connection (then we swap anyway and let the normal drop/reconnect recover).
const mailruRotateAuthWait = 8 * time.Second

// defaultMailruRotateMsgs rotates the carrier make-before-break once this many
// chat frames have been written on it, staying under the R7 server's anti-abuse
// budget. The server cuts the connection with disconnectReason 4004 ("access
// deny") when that budget is spent — it behaves like a message token bucket of
// capacity ~80-90 (measured live, d14<->212 8x download: a connection sending
// in a burst was cut at 81-91 messages in ~4 s; a slower one lasted to ~160-220
// over tens of seconds as the bucket refilled). 60 keeps a margin under the
// worst-case ~80 capacity even allowing for the handful of extra messages sent
// while a rotation is still being set up. Paired with the send pacing above
// (which stops a retransmit burst from blowing the bucket mid-rotation), this
// took the exit's 4004 cuts under an 8x download from ~11 per run to 0. At
// ~1-2 x 500 KB batches/s under the server throttle it rotates roughly every
// 30-60 s. 0 disables count-based rotation (time only).
// OPENFLUX_MAILRU_ROTATE_MSGS overrides it.
const defaultMailruRotateMsgs = 60

var mailruRotateMsgs = func() int64 {
	n := int64(defaultMailruRotateMsgs)
	if v := os.Getenv("OPENFLUX_MAILRU_ROTATE_MSGS"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 0 {
			n = parsed
		}
	}
	return n
}()

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
		var writeQueue chan []byte
		if existingSession != nil {
			// A reconnect keeps the editor identity and, crucially, the same
			// write queue, so packets already queued by Send are not lost.
			userID = existingSession.UserID
			writeQueue = existingSession.WriteQueue
		} else {
			suffix := fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
			userID = t.baseUserID + suffix
		}

		session, err := t.dialAndAuth(userID, writeQueue)
		if err != nil {
			utils.Debugf("[M-DOCS] connect failed: %v", err)
			if utils.Throttled("m-docs.fetch", time.Minute) {
				utils.Infof("[M-DOCS] cannot open the document: %v; retrying", err)
			}
			t.scheduleReconnect(attempt)
			return
		}

		firstConnect := existingSession == nil
		t.installSession(session)
		if firstConnect {
			utils.SafeGo("mailru.writer", t.writerLoop)
		}
		t.readLoop(session, attempt)
	}()
}

// dialAndAuth opens a fresh carrier WebSocket, completes the Engine.IO/Socket.IO
// handshake and sends the editor auth, returning a session whose read loop has
// not yet been started and which is not yet installed as the active session. It
// is the shared body of the first connect, a reconnect and a make-before-break
// rotation. userID is reused across reconnects/rotations (one editor identity);
// writeQueue, when non-nil, is reused so queued packets survive the swap. On any
// failure it leaves no connection open and returns the error; the caller decides
// whether to reconnect (connectToDoc) or keep the current carrier
// (rotateCarrier).
func (t *MailruDocsTransport) dialAndAuth(userID string, writeQueue chan []byte) (*DocSession, error) {
	info, err := t.fetchDocInfo(t.weblink)
	if err != nil {
		return nil, fmt.Errorf("fetchDocInfo: %w", err)
	}

	dialer := websocket.Dialer{
		// Mail.ru's nginx answers 409 Conflict to an upgrade that does
		// not offer permessage-deflate. gorilla/websocket only sends the
		// Sec-WebSocket-Extensions header when compression is enabled, so
		// without this the handshake was rejected before any auth and the
		// carrier never came up. Reported by @pirsasha (issue #163).
		EnableCompression: true,
		HandshakeTimeout:  15 * time.Second,
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
		return nil, fmt.Errorf("websocket dial (http %d): %w", status, err)
	}
	utils.Debugf("[M-DOCS] WebSocket connected")

	if writeQueue == nil {
		writeQueue = make(chan []byte, t.GetConfig().MaxQueueSize)
	}

	session := &DocSession{
		Info:       info,
		Conn:       conn,
		WriteQueue: writeQueue,
		UserID:     userID,
		ready:      make(chan struct{}),
		authed:     make(chan struct{}),
	}

	// Remember this connection's editor id so the receive path can skip its
	// echoes — including on the old connection during a rotation's overlap.
	if info.EditorUserID != "" {
		t.ourIDs.Store(info.EditorUserID, struct{}{})
	}

	if err := waitMailruSocketIO(session, info.Token); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("socket.io handshake: %w", err)
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

	return session, nil
}

// installSession makes session the active carrier and records when, so
// rotationLoop can rotate it by connection age. A rotation retires the old
// session before calling this.
func (t *MailruDocsTransport) installSession(session *DocSession) {
	session.active.Store(true)
	t.Mu.Lock()
	t.session = session
	t.SetConnected(true)
	t.Mu.Unlock()
	t.sessionStart.Store(time.Now().UnixNano())
}

// readLoop reads one carrier connection until it fails or the transport stops.
// On a read error it reconnects only if this session is still the active one and
// was not retired by a rotation (a retired connection's replacement is already
// live, so it exits silently, no reconnect). A retired session also stops
// delivering frames: its anonymous editor id differs from the new connection's,
// so it can no longer filter the new connection's own echoes out of the data.
func (t *MailruDocsTransport) readLoop(session *DocSession, attempt int) {
	conn := session.Conn
	connectedAt := time.Now()
	for t.IsRunning() {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if session.retired.Load() {
				utils.Debugf("[M-DOCS] retired carrier closed: %v", err)
				return
			}
			utils.Debugf("[M-DOCS] Read error: %v", err)
			if utils.Throttled("m-docs.drop", time.Minute) {
				utils.Infof("[M-DOCS] connection to the document dropped after %d sends, age %s: %v; reconnecting",
					session.sentCount.Load(), time.Since(connectedAt).Round(time.Second), err)
			}
			t.Mu.Lock()
			stillActive := t.session == session
			if stillActive {
				t.SetConnected(false)
			}
			t.Mu.Unlock()
			_ = conn.Close()
			if stillActive {
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
			}
			return
		}
		if session.retired.Load() {
			continue
		}
		t.handleMessage(session, message)
	}
}

// rotationLoop preemptively rotates the carrier once the active connection
// reaches mailruRotateInterval, staying ahead of the R7 server's ~45s drop of an
// anonymous editor under load. It works off the live connection's age
// (sessionStart), so an ordinary reconnect resets the clock too.
func (t *MailruDocsTransport) rotationLoop() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.Done():
			return
		case <-tick.C:
			if !t.IsRunning() || !t.IsConnected() {
				continue
			}
			start := t.sessionStart.Load()
			if start == 0 || time.Since(time.Unix(0, start)) < mailruRotateInterval {
				continue
			}
			t.Mu.RLock()
			s := t.session
			t.Mu.RUnlock()
			if s == nil {
				continue
			}
			if t.rotating.CompareAndSwap(false, true) {
				old := s
				utils.SafeGo("mailru.rotate", func() { t.rotateCarrier(old) })
			}
		}
	}
}

// rotateCarrier performs a make-before-break rotation with no visible seam: it
// brings up and authenticates a fresh carrier connection while old still carries
// traffic, starts reading it, and waits for the server to accept its auth before
// moving outgoing traffic onto it (so a packet is never sent on a connection the
// server will not yet relay — which would stall a whole RTO). At the swap the
// new connection becomes the delivering one and old is retired and closed at
// once: old keeps nothing in flight that new will not also receive (every chat
// is broadcast to all our connections), and having exactly one deliverer avoids
// the duplicate ACKs that would make the far side fast-retransmit. writerLoop
// and Send follow t.session and share the write queue, so no queued packet is
// lost. If the new connection cannot be established, old is left untouched and
// the normal drop/reconnect path remains the fallback; rotationLoop retries soon.
func (t *MailruDocsTransport) rotateCarrier(old *DocSession) {
	defer t.rotating.Store(false)
	if !t.IsRunning() {
		return
	}
	utils.Debugf("[M-DOCS] rotating carrier (age >= %v)", mailruRotateInterval)

	session, err := t.dialAndAuth(old.UserID, old.WriteQueue)
	if err != nil {
		utils.Debugf("[M-DOCS] rotation failed, keeping current carrier: %v", err)
		return
	}

	// Start reading the new connection so it processes its auth and receives
	// peer frames, but keep sending on old until the server has accepted the new
	// one. Both readers deliver during this window; the multi-identity echo
	// filter keeps either from re-delivering our own frames.
	utils.SafeGo("mailru.read", func() { t.readLoop(session, 0) })
	select {
	case <-session.authed:
	case <-time.After(mailruRotateAuthWait):
		utils.Debugf("[M-DOCS] rotation: new carrier auth not confirmed in %v; swapping anyway", mailruRotateAuthWait)
	case <-t.Done():
		session.retired.Store(true)
		if session.Conn != nil {
			_ = session.Conn.Close()
		}
		return
	}

	// Swap: the new connection becomes active (and starts delivering) and old
	// stops delivering, in one step. Then retire and close old at once — new
	// already receives every broadcast, so there is nothing to wait for.
	old.active.Store(false)
	t.installSession(session)
	old.retired.Store(true)
	if old.Conn != nil {
		_ = old.Conn.Close()
	}
	t.RecordReconnect()
	utils.Debugf("[M-DOCS] carrier rotated")
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
	var lastWrite time.Time
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

		// Pace the chat-message rate to flatten retransmit bursts so they do
		// not drain R7's 4004 message token bucket faster than a rotation can
		// swap in a fresh connection (see mailruMsgInterval / mailruRotateMsgs).
		// Backpressure propagates — a full WriteQueue makes Send fail and the
		// batcher holds the batch.
		if mailruMsgInterval > 0 && !lastWrite.IsZero() {
			if wait := mailruMsgInterval - time.Since(lastWrite); wait > 0 {
				time.Sleep(wait)
			}
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
		lastWrite = time.Now()
		pending = nil

		// Rotate make-before-break once this connection has sent its share of
		// the server's per-connection message budget, before the server cuts it.
		// Triggering here (rather than only on a timer) is what keeps a loaded
		// connection — which burns the budget in seconds — from ever being cut.
		if n := session.sentCount.Add(1); mailruRotateMsgs > 0 && n == mailruRotateMsgs {
			if t.rotating.CompareAndSwap(false, true) {
				s := session
				utils.SafeGo("mailru.rotate", func() { t.rotateCarrier(s) })
			}
		}
	}
}

func (t *MailruDocsTransport) keepAliveLoop() {
	// Disabled 2026-10-15: the application keepalive wrote a cursor message
	// (`18;---KA---`) on the cursor channel, which the current R7 server
	// rejects with 4004 ("access deny") and tears the whole connection down —
	// this is what made Mail.ru fall over after a while even on a single lane.
	// The engine.io ping/pong keeps the WebSocket alive on its own and an
	// active tunnel is never idle, so no cursor keepalive is needed. Kept as a
	// no-op so the Start() call site stays unchanged; a proper liveness probe
	// (engine.io ping or an OFX1 chat no-op) can replace it later if idle
	// sessions turn out to need one.
	return
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
		// The server has accepted this connection: it will now relay our chat.
		// A rotation waits on this before moving sends onto the new connection.
		if session != nil {
			session.markAuthed()
		}
		return
	}

	// The server announces why it is about to close us with a disconnectReason
	// frame (e.g. code 4004 "access deny" anti-abuse, 4007 "drop" lock
	// contention) just before the socket closes. Surface it at INFO — it is rare
	// and it is the one line that explains an otherwise bare "close 1005".
	if strings.Contains(text, "disconnectReason") {
		utils.Infof("[M-DOCS] server disconnectReason: %.200s", text)
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
	// decodeChatDataFilter keeps only our-tagged frames from participants that
	// are not us — isOwnID rejects every id this transport authenticated as, so
	// a rotation's old connection never re-delivers the new connection's echoes.
	// Only the active session delivers, so a frame is never delivered twice
	// during a rotation (both connections see every broadcast).
	if strings.Contains(text, `"messages"`) && session.active.Load() {
		for _, pkt := range decodeChatDataFilter(data, t.isOwnID) {
			t.RecordReceive(len(pkt))
			// Never block the reader: a full queue means downstream is behind,
			// so drop (the Session's ARQ retransmits) and keep reading so the
			// Engine.IO ping is answered and the carrier is not dropped at the
			// ~45s ping timeout.
			select {
			case t.recvCh <- pkt:
			default:
				if utils.Throttled("m-docs.recvdrop", time.Second) {
					utils.Debugf("[M-DOCS] recv queue full; dropping packet (downstream behind)")
				}
			}
		}
	}
}

// chatDataTag prefixes every tunnel payload carried over the chat channel, so a
// receiver can tell our frames from a human's chat message and skip anything it
// cannot decode as ours.
const chatDataTag = "OFX1"

// isOwnID reports whether an editor id is one of this transport's own
// connections (so a chat frame from it is our own echo, not peer data).
func (t *MailruDocsTransport) isOwnID(id string) bool {
	if id == "" {
		return false
	}
	_, ok := t.ourIDs.Load(id)
	return ok
}

// decodeChatDataFilter extracts tunnel packets from a server chat broadcast
//
//	42["message",{"type":"message","messages":[{"message":"…","useridoriginal":"…"},…]}]
//
// It keeps only tagged entries (chatDataTag) whose useridoriginal is NOT our own
// (isOwn reports that) — the server echoes every chat back to its sender, and a
// rotation's old connection also sees the new connection's echoes — and
// base64-decodes the remainder. Human chat and presence entries (no tag) are
// ignored.
func decodeChatDataFilter(frame []byte, isOwn func(string) bool) [][]byte {
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
		if isOwn != nil && isOwn(m.UserIDOriginal) {
			continue // the server's echo of one of our own connections
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

// decodeChatData keeps only tagged entries from a single other participant
// (useridoriginal != ownID). Retained for the parity test / exported form.
func decodeChatData(frame []byte, ownID string) [][]byte {
	return decodeChatDataFilter(frame, func(id string) bool { return ownID != "" && id == ownID })
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
