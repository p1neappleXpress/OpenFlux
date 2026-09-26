package main

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"openflux/transport"
	"openflux/transport/control"
	"openflux/transport/ipc"
	"openflux/transport/manager"
)

// e2eTransport is a fake raw transport that implements ErrorNotifier and
// CookieProvider, so it can play both roles in the test.
type e2eTransport struct {
	mu        sync.Mutex
	connected bool
	cb        func([]byte)

	notifyMu sync.Mutex
	notify   func(error, string, string, string)

	jarMu sync.Mutex
	jar   map[string]string
}

func (t *e2eTransport) Start() error            { t.mu.Lock(); t.connected = true; t.mu.Unlock(); return nil }
func (t *e2eTransport) Stop() error             { t.mu.Lock(); t.connected = false; t.mu.Unlock(); return nil }
func (t *e2eTransport) Send(data []byte) error  { return nil }
func (t *e2eTransport) Receive(cb func([]byte)) { t.mu.Lock(); t.cb = cb; t.mu.Unlock() }
func (t *e2eTransport) IsConnected() bool       { t.mu.Lock(); defer t.mu.Unlock(); return t.connected }
func (t *e2eTransport) Stats() transport.TransportStats {
	return transport.TransportStats{Connected: t.connected}
}

func (t *e2eTransport) SetErrorNotifier(fn func(error, string, string, string)) {
	t.notifyMu.Lock()
	t.notify = fn
	t.notifyMu.Unlock()
}

func (t *e2eTransport) FireCaptcha(reason string) {
	t.notifyMu.Lock()
	fn := t.notify
	t.notifyMu.Unlock()
	if fn != nil {
		fn(errors.New("captcha"), "yandex", "https://x", reason)
	}
}

func (t *e2eTransport) FetchCookies() (map[string]string, error) {
	t.jarMu.Lock()
	defer t.jarMu.Unlock()
	out := make(map[string]string, len(t.jar))
	for k, v := range t.jar {
		out[k] = v
	}
	return out, nil
}

func (t *e2eTransport) ApplyCookies(jar map[string]string) error {
	t.jarMu.Lock()
	defer t.jarMu.Unlock()
	t.jar = make(map[string]string, len(jar))
	for k, v := range jar {
		t.jar[k] = v
	}
	return nil
}

func (t *e2eTransport) getJar() map[string]string {
	t.jarMu.Lock()
	defer t.jarMu.Unlock()
	out := make(map[string]string, len(t.jar))
	for k, v := range t.jar {
		out[k] = v
	}
	return out
}

func TestCaptchaOverIPC(t *testing.T) {
	// 1. Manager with a fake transport.
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()

	m := manager.New(sess, nil, "test-secret-long-enough", "test-ctx")
	raw := &e2eTransport{}
	if err := m.Add("yandex", "yandex", raw, 100, raw); err != nil {
		t.Fatal(err)
	}

	// 2. IPC server on a temp socket.
	dir := t.TempDir()
	sock := filepath.Join(dir, "oflx.sock")
	h := &coreIPCHandler{manager: m}
	srv := ipc.NewServer(sock, h)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Wire the captcha notifier exactly the way main.go does.
	m.SetCaptchaNotifier(func(_name, url, reason string) {
		_ = srv.SendCookiesRequest(&ipc.CookiesRequestPayload{
			Transport: "yandex",
			URL:       url,
			Reason:    reason,
		})
	})

	// 3. IPC client with a handler that answers CookiesRequest with an offer.
	cli, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	cli.SetHandler(func(typ ipc.MsgType, payload []byte) {
		if typ != ipc.MsgCookiesRequest {
			return
		}
		var req ipc.CookiesRequestPayload
		if err := ipc.DecodeJSON(payload, &req); err != nil {
			return
		}
		_ = cli.SendCookiesOffer(&ipc.CookiesOfferPayload{
			Transport: req.Transport,
			Jar:       map[string]string{"session_id": "from-app"},
		})
	})

	// Give the client a moment to install the handler.
	time.Sleep(100 * time.Millisecond)

	// 4. Fire the captcha signal from the fake transport.
	raw.FireCaptcha("smartcaptcha")

	// 5. Expect the jar to appear on the transport within a second.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if raw.getJar()["session_id"] == "from-app" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cookies not applied: %+v", raw.getJar())
}
