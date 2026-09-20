package yandex

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"

	"openflux/transport"
)

// Exercise the actual HTTP/WebSocket entry points: installing routes alone is
// insufficient if any carrier connection silently repeats DNS after TUN setup.
func TestBootstrapDialerCoversVolgaSockets(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[string]int)
	cfg := transport.DefaultConfig()
	cfg.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[address]++
		return nil, errors.New("synthetic guarded dial failure")
	}
	tn := NewYandexVolgaTransport("https://document.invalid/fixture", cfg, MobileVolgaConfig())
	if _, err := authorize(tn.docURL, tn.config); err == nil {
		t.Fatal("authorization ignored failed carrier dial")
	}
	auth := &volgaAuth{Session: &http.Client{}}
	relay := newRelayClient(auth, tn.config, tn.stats)
	defer relay.Stop()
	if resp, err := relay.httpClient.Get("https://volga.yandex.ru/"); err == nil {
		resp.Body.Close()
		t.Fatal("relay ignored failed carrier dial")
	}
	ws := newWSListener(auth, tn.config, tn.stats, relay, nil)
	defer ws.Stop()
	if err := ws.connect(); err == nil {
		t.Fatal("WebSocket ignored failed carrier dial")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, endpoint := range []string{"document.invalid:443", "volga.yandex.ru:443", "push.yandex.ru:443"} {
		if calls[endpoint] != 1 {
			t.Errorf("carrier endpoint %s bypassed bootstrap dialer", endpoint)
		}
	}
}

func TestBootstrapDialerCoversLegacyDocumentRequest(t *testing.T) {
	var called bool
	cfg := transport.DefaultConfig()
	cfg.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		called = address == "document.invalid:443"
		return nil, errors.New("synthetic guarded dial failure")
	}
	tn := NewYandexDocsTransport("https://document.invalid/fixture", cfg)
	if _, err := tn.fetchDocInfo(tn.url, "fixture"); err == nil || !called {
		t.Fatal("legacy document request bypassed bootstrap dialer")
	}
}
