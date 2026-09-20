package yandex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"openflux/transport"
)

func TestMobileVolgaResourceLimits(t *testing.T) {
	c := MobileVolgaConfig()
	if c.WorkerCount < 8 || c.WorkerCount > 32 || c.QueueSize > 4096 || c.MaxQueuedBytes > 1<<20 || c.WSBufferSize > 16<<10 || c.MaxConnsPerHost > 32 {
		t.Fatal("unbounded mobile preset")
	}
	if DefaultVolgaConfig().WorkerCount != 2000 || DefaultVolgaConfig().QueueSize != 1000000 {
		t.Fatal("server defaults changed")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	tn := NewYandexVolgaTransport("https://document.invalid/fixture", transport.DefaultConfig(), c)
	r := newRelayClient(&volgaAuth{Session: &http.Client{}}, tn.config, tn.stats)
	r.Start()
	r.Stop()
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 2<<20 {
		t.Fatalf("mobile relay startup allocated %d bytes", n)
	}
	if r.httpClient.Transport.(*http.Transport).MaxConnsPerHost != 8 {
		t.Fatal("HTTP connections not capped")
	}
}

func TestVolgaWebSocketReconnectAndStop(t *testing.T) {
	connections := make(chan *websocket.Conn, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }} // local test server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			connections <- conn
		}
	}))
	defer srv.Close()
	cfg := MobileVolgaConfig()
	cfg.ReconnectMinDelay = 100 * time.Millisecond
	cfg.ReconnectMaxDelay = 100 * time.Millisecond
	tn := NewYandexVolgaTransport("https://document.invalid/fixture", transport.DefaultConfig(), cfg)
	tn.BaseTransport.Start()
	w := newWSListener(&volgaAuth{}, cfg, tn.stats, nil, nil)
	w.dial = func(ctx context.Context, _ string, h http.Header) (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), h)
	}
	tn.ws = w
	w.Start()
	t.Cleanup(func() { w.Stop(); tn.BaseTransport.Stop() })
	next := func() *websocket.Conn {
		select {
		case c := <-connections:
			return c
		case <-time.After(2 * time.Second):
			t.Fatal("missing WebSocket connection")
			return nil
		}
	}
	awaitState := func(want bool) {
		deadline := time.Now().Add(time.Second)
		for tn.IsConnected() != want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if tn.IsConnected() != want {
			t.Fatalf("connected state != %v", want)
		}
	}
	first := next()
	awaitState(true)
	first.Close()
	awaitState(false)
	if !tn.IsRunning() {
		t.Fatal("temporary disconnect stopped transport")
	}
	second := next()
	defer second.Close()
	awaitState(true)
	if tn.ws != w || tn.stats.WSReconnects.Load() != 1 {
		t.Fatal("reconnect duplicated listener")
	}
	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked in WebSocket ReadMessage")
	}
	awaitState(false)
}

func TestStoppedRelayDoesNotPanicOrAcceptPackets(t *testing.T) {
	r := newRelayClient(&volgaAuth{Session: &http.Client{}}, MobileVolgaConfig(), &VolgaStats{})
	r.Start()
	r.Stop()
	r.Stop()
	if err := r.Send([]byte("packet")); err == nil {
		t.Fatal("stopped relay accepted data")
	}
}

func TestVolgaConnectionTracksListener(t *testing.T) {
	tn := NewYandexVolgaTransport("https://document.invalid/fixture", transport.DefaultConfig(), MobileVolgaConfig())
	tn.BaseTransport.Start()
	tn.ws = &wsListener{}
	if tn.IsConnected() {
		t.Fatal("connected before WebSocket handshake")
	}
	tn.ws.connected.Store(true)
	if !tn.IsConnected() {
		t.Fatal("connected handshake not visible")
	}
	tn.ws.connected.Store(false)
	if tn.IsConnected() {
		t.Fatal("disconnect not visible")
	}
	// A transient disconnect must leave the same listener/relay transport running.
	if !tn.IsRunning() {
		t.Fatal("disconnect stopped transport")
	}
	tn.ws.connected.Store(true)
	if !tn.IsConnected() {
		t.Fatal("reconnect not visible")
	}
}

func BenchmarkMobileVolgaStartup(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := MobileVolgaConfig()
		c.BatchTimeout = time.Millisecond
		r := newRelayClient(&volgaAuth{Session: &http.Client{}}, c, &VolgaStats{})
		r.Start()
		r.Stop()
	}
}
