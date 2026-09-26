package manager

import (
	"sync"
	"testing"
	"time"

	"openflux/transport"
	"openflux/transport/control"
)

// pipe is an in-process carrier: whatever one end sends, the other receives.
type pipe struct {
	mu   sync.Mutex
	cb   func([]byte)
	peer *pipe
}

func (p *pipe) Start() error                    { return nil }
func (p *pipe) Stop() error                     { return nil }
func (p *pipe) IsConnected() bool               { return true }
func (p *pipe) Stats() transport.TransportStats { return transport.TransportStats{} }
func (p *pipe) Receive(cb func([]byte))         { p.mu.Lock(); p.cb = cb; p.mu.Unlock() }
func (p *pipe) Send(b []byte) error {
	b = append([]byte(nil), b...)
	p.peer.mu.Lock()
	cb := p.peer.cb
	p.peer.mu.Unlock()
	if cb != nil {
		cb(b)
	}
	return nil
}

func connectedManagers(t *testing.T, exitProvider CookieProvider) (client, exit *Manager) {
	t.Helper()
	const secret, ctx = "test-secret-long-enough", "test-ctx"
	params := transport.PeerParameters{Capabilities: control.CapabilityIPv4 | control.CapabilityTCP, MaxPacketSize: 1500}
	a, b := &pipe{}, &pipe{}
	a.peer, b.peer = b, a
	for i, side := range []struct {
		exit bool
		wire *pipe
		prov CookieProvider
	}{{false, a, nil}, {true, b, exitProvider}} {
		sess, err := transport.NewSession(params, side.exit)
		if err != nil {
			t.Fatal(err)
		}
		m := New(sess, nil, secret, ctx)
		if err := sess.AddTransport("yandex", side.wire, secret, ctx, 100); err != nil {
			t.Fatal(err)
		}
		if err := m.Add("yandex", "yandex", side.wire, 100, side.prov); err != nil {
			t.Fatal(err)
		}
		sess.SetControlHandler(m.DispatchControl)
		t.Cleanup(func() { _ = m.Stop() })
		if i == 0 {
			client = m
		} else {
			exit = m
		}
	}
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	return client, exit
}

// A check the exit's transport hits reaches the client, and the cookies
// the client answers with land on that transport on the exit.
func TestAuthRequiredRoundTrip(t *testing.T) {
	provider := &fakeCookieProvider{}
	client, exit := connectedManagers(t, provider)

	asked := make(chan [3]string, 1)
	client.SetRemoteAuthNotifier(func(name, url, reason string) { asked <- [3]string{name, url, reason} })
	var localReports int
	exit.SetCaptchaNotifier(func(string, string, string) { localReports++ })

	exit.NotifyCaptcha("yandex", "https://docs.example/d", "smartcaptcha")
	select {
	case got := <-asked:
		if got != [3]string{"yandex", "https://docs.example/d", "smartcaptcha"} {
			t.Fatalf("client was asked %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AuthRequired did not reach the client")
	}
	if localReports != 1 {
		t.Fatalf("exit's local notifier got %d reports, want 1", localReports)
	}

	if err := client.OfferCookies("yandex", map[string]string{"spravka": "s1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if jar, _ := provider.FetchCookies(); jar["spravka"] == "s1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offered cookies did not reach the exit's transport")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The client's own checks stay local: only the exit forwards.
func TestClientDoesNotForwardAuth(t *testing.T) {
	client, exit := connectedManagers(t, &fakeCookieProvider{})
	asked := make(chan struct{}, 1)
	exit.SetRemoteAuthNotifier(func(string, string, string) { asked <- struct{}{} })
	client.NotifyCaptcha("yandex", "https://docs.example/d", "smartcaptcha")
	select {
	case <-asked:
		t.Fatal("client forwarded its own check to the exit")
	case <-time.After(200 * time.Millisecond):
	}
}
