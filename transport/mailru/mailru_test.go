package mailru

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

const testWeblink = "AbCdEfGh1/IjKlMnOp2"

func testConfig() transport.TransportConfig {
	cfg := transport.DefaultConfig()
	cfg.KeepAliveInterval = 500 * time.Millisecond
	return cfg
}

// collector records everything a transport hands up to its user callback.
type collector struct {
	mu   sync.Mutex
	pkts [][]byte
}

func (c *collector) cb(p []byte) {
	c.mu.Lock()
	c.pkts = append(c.pkts, append([]byte(nil), p...))
	c.mu.Unlock()
}

func (c *collector) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.pkts...)
}

func (c *collector) has(want []byte) bool {
	for _, p := range c.all() {
		if bytes.Equal(p, want) {
			return true
		}
	}
	return false
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func (c *stubCloud) participantCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.userSeq)
}

func TestWsBaseFrom(t *testing.T) {
	cases := map[string]string{
		"https://r7.mail.ru": "wss://r7.mail.ru",
		"http://127.0.0.1:1": "ws://127.0.0.1:1",
		"r7.mail.ru":         "r7.mail.ru",
	}
	for in, want := range cases {
		if got := wsBaseFrom(in); got != want {
			t.Errorf("wsBaseFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeWeblink(t *testing.T) {
	cases := map[string]string{
		"AbCdEfGh1/IjKlMnOp2":                              "AbCdEfGh1/IjKlMnOp2",
		"https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2": "AbCdEfGh1/IjKlMnOp2",
		"http://cloud.mail.ru/AbCdEfGh1/IjKlMnOp2/":        "AbCdEfGh1/IjKlMnOp2",
	}
	for in, want := range cases {
		if got := normalizeWeblink(in); got != want {
			t.Errorf("normalizeWeblink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchDocInfo(t *testing.T) {
	c := newStubCloud(t)
	c.install(t)

	tr := NewMailruDocsTransport(testWeblink, testConfig())
	info, err := tr.fetchDocInfo(testWeblink)
	if err != nil {
		t.Fatalf("fetchDocInfo: %v", err)
	}
	if info.Token != "stub-token" || info.DocKey != "stubdoc" {
		t.Errorf("token/key = %q/%q", info.Token, info.DocKey)
	}
	if info.EditorUserID != "editor-user-1" || info.CallbackURL != "https://stub/callback" {
		t.Errorf("editorConfig fields = %q/%q", info.EditorUserID, info.CallbackURL)
	}
	// permissions must survive as an object: a number here makes the real
	// editor server answer "access deny".
	if v, ok := info.Permissions["edit"].(bool); !ok || !v {
		t.Errorf("permissions = %#v, want edit:true", info.Permissions)
	}
	wantWs := strings.Replace(c.srv.URL, "http://", "ws://", 1) + "/doc/stubdoc/c/?EIO=4&transport=websocket"
	if info.WsURL != wantWs {
		t.Errorf("WsURL = %q, want %q", info.WsURL, wantWs)
	}
}

// startPair brings up two transports on the same stub document and waits until
// the coauthoring server has seen both participants.
func startPair(t *testing.T, c *stubCloud, wrap func(transport.Transport) transport.Transport) (transport.Transport, transport.Transport, *collector, *collector) {
	t.Helper()

	mk := func() (transport.Transport, *collector) {
		var tr transport.Transport = NewMailruDocsTransport(testWeblink, testConfig())
		if wrap != nil {
			tr = wrap(tr)
		}
		col := &collector{}
		tr.Receive(col.cb)
		if err := tr.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = tr.Stop() })
		return tr, col
	}

	a, colA := mk()
	if !waitFor(5*time.Second, func() bool { return c.participantCount() >= 1 }) {
		t.Fatal("first participant never authenticated")
	}
	b, colB := mk()
	if !waitFor(5*time.Second, func() bool { return c.participantCount() >= 2 }) {
		t.Fatal("second participant never authenticated")
	}
	if !waitFor(5*time.Second, func() bool { return a.IsConnected() && b.IsConnected() }) {
		t.Fatal("transports never reported connected")
	}
	return a, b, colA, colB
}

func TestAuthHandshakeShape(t *testing.T) {
	c := newStubCloud(t)
	c.install(t)
	startPair(t, c, nil)

	msgs := c.controlMessages()
	var sawSocketIO, sawAuth bool
	for _, m := range msgs {
		if strings.HasPrefix(m, `40{"token":"stub-token"}`) {
			sawSocketIO = true
		}
		if strings.Contains(m, `"type":"auth"`) {
			sawAuth = true
			for _, want := range []string{`"docid":"stubdoc"`, `"mode":"edit"`, `"coEditingMode":"fast"`, `"jwtOpen":"stub-token"`} {
				if !strings.Contains(m, want) {
					t.Errorf("auth message missing %s:\n%s", want, m)
				}
			}
			if strings.Contains(m, `"permissions":1`) || strings.Contains(m, `"permissions":0`) {
				t.Errorf("permissions sent as a number, editor server would deny:\n%s", m)
			}
		}
	}
	if !sawSocketIO || !sawAuth {
		t.Fatalf("handshake incomplete: socket.io=%v auth=%v (%d control frames)", sawSocketIO, sawAuth, len(msgs))
	}
}

// TestCursorRoundTrip is the bare carrier: no codec on top, one peer's bytes
// must arrive at the other verbatim through the "cursor" field.
func TestCursorRoundTrip(t *testing.T) {
	c := newStubCloud(t)
	c.install(t)
	a, _, _, colB := startPair(t, c, nil)

	payload := []byte{0x45, 0x00, 0xde, 0xad, 0xbe, 0xef, 0x00, 0xff}
	ok := waitFor(5*time.Second, func() bool {
		_ = a.Send(payload)
		return colB.has(payload)
	})
	if !ok {
		t.Fatalf("peer never received the payload; got %d packets", len(colB.all()))
	}
}

// TestKeepAliveNotDelivered guards the ---KA--- marker: it shares the cursor
// field with real data and must never reach the tunnel.
func TestKeepAliveNotDelivered(t *testing.T) {
	c := newStubCloud(t)
	c.install(t)
	_, _, colA, colB := startPair(t, c, nil)

	time.Sleep(1500 * time.Millisecond) // several keep-alive ticks at 500ms

	for _, col := range []*collector{colA, colB} {
		for _, p := range col.all() {
			if bytes.Contains(p, []byte("---KA---")) {
				t.Fatalf("keep-alive leaked into the tunnel: %q", p)
			}
		}
	}
}

// negotiationResult is what the handshake tests report.
type negotiationResult struct {
	batchA, batchB bool
	probeA, probeB bool
	settledAt      time.Duration // time to the two-sided handshake; 0 = never
	rxA, rxB       int
	dials          int
}

// runNegotiation puts an AdaptiveTransport on each side, drives real traffic
// for the given window, and reports whether the codec handshake completed —
// i.e. whether each side ever put a batch frame (0x02) on the wire.
// stopWhenSettled returns as soon as it does; otherwise the full window is used
// to see whether the link stays healthy afterwards.
func runNegotiation(t *testing.T, c *stubCloud, window time.Duration, stopWhenSettled bool) negotiationResult {
	t.Helper()
	wrap := func(inner transport.Transport) transport.Transport {
		return transport.NewAdaptiveTransport(inner)
	}
	a, b, colA, colB := startPair(t, c, wrap)

	var res negotiationResult
	start := time.Now()
	deadline := start.Add(window)
	for i := 0; time.Now().Before(deadline); i++ {
		_ = a.Send([]byte{0x45, 0x00, byte(i), 0xaa})
		_ = b.Send([]byte{0x45, 0x00, byte(i), 0xbb})

		if res.settledAt == 0 && c.sentBatchFrame(0) && c.sentBatchFrame(1) {
			res.settledAt = time.Since(start)
			if stopWhenSettled && len(colA.all()) > 0 && len(colB.all()) > 0 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	res.batchA = c.sentBatchFrame(0)
	res.batchB = c.sentBatchFrame(1)
	res.probeA = c.sentProbe(0)
	res.probeB = c.sentProbe(1)
	res.rxA = len(colA.all())
	res.rxB = len(colB.all())
	res.dials = c.dialCount()
	return res
}

// TestNegotiationHandshake is the baseline: two healthy peers must leave legacy
// and settle on the batch codec within a few probe intervals.
func TestNegotiationHandshake(t *testing.T) {
	if testing.Short() {
		t.Skip("probe interval is 3s")
	}
	c := newStubCloud(t)
	c.install(t)

	r := runNegotiation(t, c, 15*time.Second, true)
	t.Logf("baseline: batchA=%v batchB=%v probeA=%v probeB=%v settled=%v rxA=%d rxB=%d dials=%d",
		r.batchA, r.batchB, r.probeA, r.probeB, r.settledAt, r.rxA, r.rxB, r.dials)

	if !r.batchA || !r.batchB {
		t.Errorf("codec handshake did not complete: batchA=%v batchB=%v", r.batchA, r.batchB)
	}
	if r.rxA == 0 || r.rxB == 0 {
		t.Errorf("no traffic crossed: rxA=%d rxB=%d", r.rxA, r.rxB)
	}
}

// TestNegotiationWithLatentPeer models a peer that is merely late: every frame
// reaches it 5s after the fact — past both the 3s probe interval and the 4s
// optimistic-upgrade window — but nothing is rate-limited. Negotiation should
// still converge, just later.
func TestNegotiationWithLatentPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~25s")
	}
	c := newStubCloud(t)
	c.delayFor = func(idx int) time.Duration {
		if idx == 1 {
			return 5 * time.Second
		}
		return 0
	}
	c.install(t)

	r := runNegotiation(t, c, 25*time.Second, true)
	t.Logf("latent peer: batchA=%v batchB=%v probeA=%v probeB=%v settled=%v rxA=%d rxB=%d dials=%d",
		r.batchA, r.batchB, r.probeA, r.probeB, r.settledAt, r.rxA, r.rxB, r.dials)

	if !r.batchA || !r.batchB {
		t.Errorf("codec handshake did not complete under pure latency: batchA=%v batchB=%v", r.batchA, r.batchB)
	}
	if r.rxA == 0 || r.rxB == 0 {
		t.Errorf("no traffic crossed: rxA=%d rxB=%d", r.rxA, r.rxB)
	}
}

// TestNegotiationWithThrottledPeer models the low-priority process on the VM:
// it is scheduled rarely enough that it only gets through a couple of frames a
// second, so its inbound stream head-of-line blocks behind the peer's early
// legacy traffic. This is the case the handshake is expected to lose.
func TestNegotiationWithThrottledPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~30s")
	}
	c := newStubCloud(t)
	c.throttleFor = func(idx int) time.Duration {
		if idx == 1 {
			return 2 * time.Second
		}
		return 0
	}
	c.install(t)

	r := runNegotiation(t, c, 30*time.Second, true)
	t.Logf("throttled peer: batchA=%v batchB=%v probeA=%v probeB=%v settled=%v rxA=%d rxB=%d dials=%d",
		r.batchA, r.batchB, r.probeA, r.probeB, r.settledAt, r.rxA, r.rxB, r.dials)

	if !r.batchA || !r.batchB {
		t.Errorf("codec handshake did not complete under CPU starvation: batchA=%v batchB=%v", r.batchA, r.batchB)
	}
	if r.rxA == 0 || r.rxB == 0 {
		t.Errorf("no traffic crossed: rxA=%d rxB=%d", r.rxA, r.rxB)
	}
}

// TestNegotiationUnderReconnectChurn models the same starvation the other way
// round: the process is descheduled long enough to lose its socket, so it
// reconnects constantly. probeLoop only fires while IsConnected, and the
// backoff grows on every short-lived session, so this is where the handshake is
// expected to stall.
func TestNegotiationUnderReconnectChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~30s")
	}
	c := newStubCloud(t)
	c.dropAfter = func(idx int) time.Duration {
		if idx == 1 {
			return 1200 * time.Millisecond
		}
		return 0
	}
	c.install(t)

	// Run the whole window: the interesting part is not only whether the
	// handshake ever happens but whether the link survives the churn after it.
	r := runNegotiation(t, c, 25*time.Second, false)
	t.Logf("reconnect churn: batchA=%v batchB=%v probeA=%v probeB=%v settled=%v rxA=%d rxB=%d dials=%d",
		r.batchA, r.batchB, r.probeA, r.probeB, r.settledAt, r.rxA, r.rxB, r.dials)

	if r.dials < 4 {
		t.Fatalf("churn never materialised: only %d dials", r.dials)
	}
	if !r.batchA || !r.batchB {
		t.Errorf("codec handshake did not complete under reconnect churn: batchA=%v batchB=%v", r.batchA, r.batchB)
	}
	if r.rxA == 0 || r.rxB == 0 {
		t.Errorf("link dead under churn: rxA=%d rxB=%d", r.rxA, r.rxB)
	}
}
