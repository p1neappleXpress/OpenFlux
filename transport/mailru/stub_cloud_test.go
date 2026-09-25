package mailru

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// stubCloud stands in for cloud.mail.ru: the editor API that hands out a token
// and a WebSocket base, plus the coauthoring socket itself. The socket models
// the one property the transport relies on — a cursor message is broadcast to
// the OTHER participants and never echoed back to its sender.
//
// Participants are identified by the username in their auth message, not by
// connection, so a peer keeps its identity across reconnects. Two knobs model a
// process that the VM scheduler is starving:
//
//	delayFor    — pure latency: that peer sees every frame N later, order kept
//	throttleFor — pure starvation: that peer is fed at most one frame per N
//	dropAfter   — that peer's socket is torn down every N, forcing reconnect churn
type stubCloud struct {
	srv *httptest.Server

	// Set before the first dial; idx is the participant's first-seen order.
	delayFor    func(idx int) time.Duration
	throttleFor func(idx int) time.Duration
	dropAfter   func(idx int) time.Duration

	mu      sync.Mutex
	conns   []*stubConn
	userSeq map[string]int
	frames  []stubFrame
	ctrl    []string
	dials   int
}

type stubFrame struct {
	from int // participant index, -1 before auth
	data []byte
}

// stubOut carries the enqueue time so delivery delay is modelled as latency
// (deliver at enqueue+delay) rather than as a sleep before every write, which
// would silently be a throttle instead.
type stubOut struct {
	data []byte
	at   time.Time
}

type stubConn struct {
	conn *websocket.Conn
	out  chan stubOut

	mu   sync.Mutex
	part int // participant index, -1 until auth seen
}

func (s *stubConn) participant() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.part
}

var (
	stubUpgrader = websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	stubUsernameRe = regexp.MustCompile(`"username":"([^"]+)"`)
)

func newStubCloud(t *testing.T) *stubCloud {
	t.Helper()
	c := &stubCloud{userSeq: map[string]int{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/r7/edit", c.handleEdit)
	mux.HandleFunc("/doc/", c.handleSocket)
	c.srv = httptest.NewServer(mux)

	t.Cleanup(c.srv.Close)
	return c
}

// install points the transport package at this stub for the duration of a test.
func (c *stubCloud) install(t *testing.T) {
	t.Helper()
	prev := mailruAPIURL
	mailruAPIURL = c.srv.URL + "/api/v4/r7/edit"
	t.Cleanup(func() { mailruAPIURL = prev })
}

func (c *stubCloud) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]interface{}{
		"api":   c.srv.URL,
		"token": "stub-token",
		"document": map[string]interface{}{
			"key":         "stubdoc",
			"fileType":    "docx",
			"url":         "https://stub/doc.docx",
			"title":       "stub.docx",
			"permissions": map[string]interface{}{"edit": true, "download": false},
		},
		"editorConfig": map[string]interface{}{
			"callbackUrl": "https://stub/callback",
			"user":        map[string]interface{}{"id": "editor-user-1"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (c *stubCloud) handleSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	sc := &stubConn{conn: conn, out: make(chan stubOut, 256), part: -1}
	c.mu.Lock()
	c.conns = append(c.conns, sc)
	c.dials++
	c.mu.Unlock()

	go c.writePump(sc)

	defer func() {
		c.mu.Lock()
		for i, x := range c.conns {
			if x == sc {
				c.conns = append(c.conns[:i], c.conns[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		close(sc.out)
		conn.Close()
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		c.onMessage(sc, msg)
	}
}

func (c *stubCloud) writePump(sc *stubConn) {
	var lastWrite time.Time
	for msg := range sc.out {
		part := sc.participant()
		if d := c.delay(part); d > 0 {
			if wait := time.Until(msg.at.Add(d)); wait > 0 {
				time.Sleep(wait)
			}
		}
		if g := c.gap(part); g > 0 && !lastWrite.IsZero() {
			if wait := time.Until(lastWrite.Add(g)); wait > 0 {
				time.Sleep(wait)
			}
		}
		if err := sc.conn.WriteMessage(websocket.TextMessage, msg.data); err != nil {
			return
		}
		lastWrite = time.Now()
	}
}

func (c *stubCloud) delay(part int) time.Duration {
	if c.delayFor == nil || part < 0 {
		return 0
	}
	return c.delayFor(part)
}

func (c *stubCloud) gap(part int) time.Duration {
	if c.throttleFor == nil || part < 0 {
		return 0
	}
	return c.throttleFor(part)
}

// bindParticipant resolves the auth message's username to a stable participant
// index and arms the drop timer the first time this connection is identified.
func (c *stubCloud) bindParticipant(sc *stubConn, text string) {
	m := stubUsernameRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return
	}
	c.mu.Lock()
	idx, ok := c.userSeq[m[1]]
	if !ok {
		idx = len(c.userSeq)
		c.userSeq[m[1]] = idx
	}
	c.mu.Unlock()

	sc.mu.Lock()
	sc.part = idx
	sc.mu.Unlock()

	if c.dropAfter != nil {
		if d := c.dropAfter(idx); d > 0 {
			time.AfterFunc(d, func() { sc.conn.Close() })
		}
	}
}

func (c *stubCloud) onMessage(from *stubConn, msg []byte) {
	text := string(msg)

	if !strings.Contains(text, `"type":"cursor"`) {
		if strings.Contains(text, `"type":"auth"`) {
			c.bindParticipant(from, text)
		}
		c.mu.Lock()
		c.ctrl = append(c.ctrl, text)
		c.mu.Unlock()
		return
	}

	// Record the decoded payload so a test can inspect what actually went on
	// the wire — in particular the codec family byte. Keep-alives carry a
	// non-base64 marker and fall out here.
	if m := cursorPayloadRe.FindStringSubmatch(text); len(m) > 1 {
		if data, err := base64.StdEncoding.DecodeString(m[1]); err == nil {
			c.mu.Lock()
			c.frames = append(c.frames, stubFrame{from: from.participant(), data: data})
			c.mu.Unlock()
		}
	}

	src := from.participant()
	c.mu.Lock()
	dsts := make([]*stubConn, 0, len(c.conns))
	for _, sc := range c.conns {
		if sc != from && sc.participant() != src {
			dsts = append(dsts, sc)
		}
	}
	c.mu.Unlock()

	for _, dst := range dsts {
		select {
		case dst.out <- stubOut{data: append([]byte(nil), msg...), at: time.Now()}:
		default: // slow participant: drop, exactly as a real bounded queue would
		}
	}
}

// framesFrom returns the payloads participant idx put on the wire.
func (c *stubCloud) framesFrom(idx int) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][]byte
	for _, f := range c.frames {
		if f.from == idx {
			out = append(out, f.data)
		}
	}
	return out
}

// sentBatchFrame reports whether participant idx ever emitted a batch frame
// CARRYING DATA — i.e. whether the codec negotiation actually completed on that
// side. The capability probe is also a 0x02 frame, but an empty one, exactly
// two bytes ([version, flags]); counting it would report a handshake that never
// happened, since every side probes unconditionally.
func (c *stubCloud) sentBatchFrame(idx int) bool {
	for _, f := range c.framesFrom(idx) {
		if len(f) > 2 && f[0] == 0x02 {
			return true
		}
	}
	return false
}

// sentProbe reports whether participant idx got an empty capability probe out —
// the precondition for the peer ever upgrading.
func (c *stubCloud) sentProbe(idx int) bool {
	for _, f := range c.framesFrom(idx) {
		if len(f) == 2 && f[0] == 0x02 {
			return true
		}
	}
	return false
}

// controlMessages returns the non-cursor frames (the socket.io auth handshake).
func (c *stubCloud) controlMessages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ctrl...)
}

func (c *stubCloud) dialCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dials
}
