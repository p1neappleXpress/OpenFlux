package manager

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openflux/transport"
	"openflux/transport/control"
)

// fakeTransport is a minimal in-process Transport.
type fakeTransport struct {
	mu   sync.Mutex
	cb   func([]byte)
	live bool
}

func (f *fakeTransport) Start() error            { f.mu.Lock(); f.live = true; f.mu.Unlock(); return nil }
func (f *fakeTransport) Stop() error             { f.mu.Lock(); f.live = false; f.mu.Unlock(); return nil }
func (f *fakeTransport) Send(data []byte) error  { return nil }
func (f *fakeTransport) Receive(cb func([]byte)) { f.mu.Lock(); f.cb = cb; f.mu.Unlock() }
func (f *fakeTransport) IsConnected() bool       { f.mu.Lock(); defer f.mu.Unlock(); return f.live }
func (f *fakeTransport) Stats() transport.TransportStats {
	return transport.TransportStats{Connected: f.live}
}

func TestManagerAddRemoveTransport(t *testing.T) {
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	m := New(sess, nil, "test-secret-long-enough", "test-ctx")
	a := &fakeTransport{}
	b := &fakeTransport{}
	if err := m.Add("first", "fake", a, 100, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("second", "fake", b, 50, nil); err != nil {
		t.Fatal(err)
	}
	got := m.Transports()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("priority order wrong: %v", got)
	}
	if err := m.Remove("first"); err != nil {
		t.Fatal(err)
	}
	got = m.Transports()
	if len(got) != 1 || got[0] != "second" {
		t.Fatalf("after remove: %v", got)
	}
	_ = sess.Stop()
}

func TestManagerCookieExchangerPerTransport(t *testing.T) {
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	m := New(sess, nil, "test-secret-long-enough", "test-ctx")

	provider := &fakeCookieProvider{jar: map[string]string{"a": "1"}}
	if err := m.Add("yandex", "yandex", &fakeTransport{}, 100, provider); err != nil {
		t.Fatal(err)
	}
	jar, err := m.FetchCookiesFor("yandex")
	if err != nil {
		t.Fatal(err)
	}
	if jar["a"] != "1" {
		t.Fatalf("fetch = %v", jar)
	}
	if err := m.ApplyCookiesFor("yandex", map[string]string{"b": "2"}); err != nil {
		t.Fatal(err)
	}
	jar, _ = m.FetchCookiesFor("yandex")
	if jar["b"] != "2" {
		t.Fatalf("apply failed: %v", jar)
	}

	// Transport without cookies must fail cleanly.
	if err := m.Add("direct", "direct", &fakeTransport{}, 50, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FetchCookiesFor("direct"); err == nil {
		t.Fatal("expected error for cookie-less transport")
	}
	_ = sess.Stop()
}

type fakeCookieProvider struct {
	mu  sync.Mutex
	jar map[string]string
}

func (f *fakeCookieProvider) FetchCookies() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.jar))
	for k, v := range f.jar {
		out[k] = v
	}
	return out, nil
}

func (f *fakeCookieProvider) ApplyCookies(jar map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jar = make(map[string]string, len(jar))
	for k, v := range jar {
		f.jar[k] = v
	}
	return nil
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop() })
	return New(sess, nil, "test-secret-long-enough", "test-ctx")
}

func offer(t *testing.T, name string, jar map[string]string) []byte {
	t.Helper()
	body, err := (&control.CookiesPayload{Transport: name, Jar: jar}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Cookie offers reach the transport they name (or, from older peers that
// name none, the highest-priority cookie transport) and are persisted.
func TestManagerRoutesCookiesByTransport(t *testing.T) {
	m := newTestManager(t)
	yandex := &fakeCookieProvider{}
	mailru := &fakeCookieProvider{}
	if err := m.Add("yandex", "yandex", &fakeTransport{}, 100, yandex); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("mailru", "mailru", &fakeTransport{}, 50, mailru); err != nil {
		t.Fatal(err)
	}
	store, err := transport.NewCookieStore(filepath.Join(t.TempDir(), "cookies.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]string{"yandex": "doc-y", "mailru": "doc-m"} {
		if err := m.UseCookieStore(store, name, key); err != nil {
			t.Fatal(err)
		}
	}

	m.DispatchControl(control.SubtypeCookiesOffer, offer(t, "mailru", map[string]string{"m": "1"}))
	m.DispatchControl(control.SubtypeCookiesResponse, offer(t, "", map[string]string{"y": "2"}))
	if jar, _ := mailru.FetchCookies(); jar["m"] != "1" {
		t.Fatalf("mailru jar = %v", jar)
	}
	if jar, _ := yandex.FetchCookies(); jar["y"] != "2" || jar["m"] != "" {
		t.Fatalf("yandex jar = %v", jar)
	}

	// A restarted process replays what was persisted.
	next := newTestManager(t)
	replayed := &fakeCookieProvider{}
	if err := next.Add("mailru", "mailru", &fakeTransport{}, 50, replayed); err != nil {
		t.Fatal(err)
	}
	reopened, err := transport.NewCookieStore(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := next.UseCookieStore(reopened, "mailru", "doc-m"); err != nil {
		t.Fatal(err)
	}
	if jar, _ := replayed.FetchCookies(); jar["m"] != "1" {
		t.Fatalf("replayed jar = %v", jar)
	}
}

func TestManagerDispatchControlForwardsUnknownSubtypes(t *testing.T) {
	m := newTestManager(t)
	got := make(chan control.Subtype, 1)
	m.SetControlCallback(func(sub control.Subtype, payload []byte) { got <- sub })
	m.DispatchControl(control.Subtype(0x7f), nil)
	select {
	case sub := <-got:
		if sub != 0x7f {
			t.Fatalf("forwarded subtype = %v", sub)
		}
	case <-time.After(time.Second):
		t.Fatal("callback not invoked")
	}
}
