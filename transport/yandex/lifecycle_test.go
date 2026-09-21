package yandex

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

// fakeDocServer imitates the two Yandex endpoints the transport talks to: the
// document page carrying client-config, and the TLS WebSocket balancer.
type fakeDocServer struct {
	pageURL string

	mu    sync.Mutex
	conns []*websocket.Conn
	recv  []string
}

func newFakeDocServer(t *testing.T) *fakeDocServer {
	t.Helper()
	f := &fakeDocServer{}

	upgrader := websocket.Upgrader{}
	ws := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.recv = append(f.recv, string(msg))
			f.mu.Unlock()
		}
	}))
	t.Cleanup(ws.Close)

	config := fmt.Sprintf(`{"officeActionData":{"balancer_url":%q,"editor_config":{"token":"jwt",`+
		`"document":{"key":"docKey","fileType":"xlsx","url":"u","title":"t"}}}}`, ws.URL)
	f.pageURL = serveConfig(t, clientConfigPage(config))
	return f
}

func (f *fakeDocServer) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
}

// push sends a server message on the latest connection.
func (f *fakeDocServer) push(t *testing.T, msg string) {
	t.Helper()
	f.mu.Lock()
	conn := f.conns[len(f.conns)-1]
	f.mu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server push: %v", err)
	}
}

func (f *fakeDocServer) countReceived(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.recv {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

func (f *fakeDocServer) received(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.recv {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func newTestTransport(url string) *YandexDocsTransport {
	cfg := transport.DefaultConfig()
	cfg.KeepAliveInterval = 50 * time.Millisecond
	tr := NewYandexDocsTransport(url, cfg)
	tr.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	return tr
}

// transportGoroutines counts live goroutines running YandexDocsTransport code.
func transportGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*YandexDocsTransport)") {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stopQuickly stops tr and checks that Stop returns promptly and leaves no
// transport goroutine behind.
func stopQuickly(t *testing.T, tr *YandexDocsTransport) {
	t.Helper()
	start := time.Now()
	if err := tr.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Stop took %v", d)
	}
	if n := transportGoroutines(); n != 0 {
		t.Fatalf("%d transport goroutines still running after Stop", n)
	}
}

func TestYandexDocsTransportDataKeepaliveAndStop(t *testing.T) {
	srv := newFakeDocServer(t)
	tr := newTestTransport(srv.pageURL)

	got := make(chan []byte, 1)
	tr.Receive(func(b []byte) { got <- b })
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connect", 5*time.Second, func() bool { return tr.IsConnected() && srv.connCount() == 1 })

	// Outbound packet and our own keepalive reach the server.
	if err := tr.Send([]byte("hello")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("hello"))
	waitFor(t, "outbound packet", 2*time.Second, func() bool { return srv.received(b64) })
	waitFor(t, "keepalive", 2*time.Second, func() bool { return srv.received("---KA---") })

	// The peer's keepalive marks the peer as heard from without delivering data.
	if !tr.Stats().LastRecv.IsZero() {
		t.Fatal("LastRecv set before anything arrived from the peer")
	}
	srv.push(t, `42["message",{"type":"cursor","messages":[{"cursor":"18;---KA---"}]}]`)
	waitFor(t, "peer keepalive", 2*time.Second, func() bool { return !tr.Stats().LastRecv.IsZero() })

	// Inbound packet reaches the callback.
	srv.push(t, `42["message",{"type":"cursor","messages":[{"cursor":"18;`+
		base64.StdEncoding.EncodeToString([]byte("world"))+`"}]}]`)
	select {
	case b := <-got:
		if string(b) != "world" {
			t.Fatalf("callback got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound packet not delivered")
	}

	stopQuickly(t, tr)
}

// A server-side drop (disconnectReason + close) is followed by a reconnect.
func TestYandexDocsTransportReconnectsAfterServerDrop(t *testing.T) {
	srv := newFakeDocServer(t)
	tr := newTestTransport(srv.pageURL)
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "connect", 5*time.Second, func() bool { return tr.IsConnected() && srv.connCount() == 1 })

	srv.push(t, `42["message",{"type":"disconnectReason","code":4007,"description":"drop"}]`)
	srv.mu.Lock()
	srv.conns[0].Close()
	srv.mu.Unlock()

	waitFor(t, "disconnect", 2*time.Second, func() bool { return !tr.IsConnected() })
	waitFor(t, "reconnect", 6*time.Second, func() bool { return tr.IsConnected() && srv.connCount() == 2 })
	stopQuickly(t, tr)
}

// Stop must not wait out a reconnect backoff.
func TestYandexDocsTransportStopInterruptsBackoff(t *testing.T) {
	url := serveConfig(t, "<html>no config here</html>")
	tr := newTestTransport(url)
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // fetch failed, now in a >=1.5s backoff
	stopQuickly(t, tr)
}

// Stop must not wait for a document page that never answers.
func TestYandexDocsTransportStopInterruptsHangingFetch(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	tr := newTestTransport(srv.URL)
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	stopQuickly(t, tr)
}

// A participant list with nobody but us means the peer left the document: it
// no longer counts as heard from until it sends again.
func TestYandexDocsTransportForgetsPeerWhenAlone(t *testing.T) {
	srv := newFakeDocServer(t)
	tr := newTestTransport(srv.pageURL)
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()
	waitFor(t, "connect", 5*time.Second, func() bool { return tr.IsConnected() && srv.connCount() == 1 })

	heard := func() bool { return !tr.Stats().LastRecv.IsZero() }
	const ka = `42["message",{"type":"cursor","messages":[{"cursor":"18;---KA---"}]}]`
	srv.push(t, `40{"sid":"me"}`)
	srv.push(t, ka)
	waitFor(t, "peer keepalive", 2*time.Second, heard)

	// Someone else is still listed: nothing changes.
	srv.push(t, `42["message",{"type":"connectState","participants":[{"connectionId":"me"},{"connectionId":"peer"}]}]`)
	srv.push(t, `42["message",{"type":"auth","sessionId":"me","participants":[{"connectionId":"peer"}]}]`)
	time.Sleep(100 * time.Millisecond)
	if !heard() {
		t.Fatal("peer forgotten while still listed")
	}

	// Only us left.
	srv.push(t, `42["message",{"type":"connectState","participants":[{"connectionId":"me"}]}]`)
	waitFor(t, "peer forgotten", 2*time.Second, func() bool { return !heard() })

	srv.push(t, ka)
	waitFor(t, "peer heard again", 2*time.Second, heard)
	srv.push(t, `42["message",{"type":"auth","sessionId":"me","participants":[]}]`)
	waitFor(t, "peer forgotten on empty list", 2*time.Second, func() bool { return !heard() })
}

// Whenever a participant list shows someone else (our auth reply with the peer
// already in the document, or a peer that just joined), the peer hears us
// without waiting for the keepalive tick. Nothing is sent before the server
// answers our auth.
func TestYandexDocsTransportGreetsPeer(t *testing.T) {
	srv := newFakeDocServer(t)
	tr := newTestTransport(srv.pageURL)
	tr.BaseTransport = transport.NewBaseTransport(transport.TransportConfig{
		MaxReconnectAttempts: 10, MaxQueueSize: 16, KeepAliveInterval: time.Hour,
	})
	if err := tr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tr.Stop()
	waitFor(t, "connect", 5*time.Second, func() bool { return tr.IsConnected() && srv.connCount() == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := srv.countReceived("---KA---"); n != 0 {
		t.Fatalf("keepalive sent before the auth reply (%d)", n)
	}

	srv.push(t, `40{"sid":"me"}`)
	srv.push(t, `42["message",{"type":"auth","sessionId":"me","participants":[{"connectionId":"me"},{"connectionId":"peer"}]}]`)
	waitFor(t, "keepalive for peer in auth reply", 2*time.Second, func() bool { return srv.countReceived("---KA---") == 1 })
	srv.push(t, `42["message",{"type":"connectState","participants":[{"connectionId":"me"},{"connectionId":"peer2"}]}]`)
	waitFor(t, "keepalive for new participant", 2*time.Second, func() bool { return srv.countReceived("---KA---") == 2 })

	srv.push(t, `42["message",{"type":"connectState","participants":[{"connectionId":"me"}]}]`)
	time.Sleep(100 * time.Millisecond)
	if n := srv.countReceived("---KA---"); n != 2 {
		t.Fatalf("keepalive sent with nobody else in the document (%d total)", n)
	}
}
