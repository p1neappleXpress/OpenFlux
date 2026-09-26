package transport

import (
	"testing"
	"time"

	"openflux/transport/control"
)

var testParams = PeerParameters{
	Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP | control.CapabilityUDP,
	MaxPacketSize: 1500,
}

// linkedSessions builds a client and an exit joined by one wire pair per
// name, in the given priority order.
func linkedSessions(t *testing.T, names ...string) (client, exit *Session, cw, ew map[string]*startCountingWire) {
	t.Helper()
	var err error
	if client, err = NewSession(testParams, false); err != nil {
		t.Fatal(err)
	}
	if exit, err = NewSession(testParams, true); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{client, exit} {
		s.restartMin = 10 * time.Millisecond
		s.restartMax = 20 * time.Millisecond
		s.handshakeTimeout = 3 * time.Second
	}
	cw, ew = map[string]*startCountingWire{}, map[string]*startCountingWire{}
	for i, name := range names {
		a, b := &startCountingWire{}, &startCountingWire{}
		a.peer, b.peer = &b.negotiationWire, &a.negotiationWire
		cw[name], ew[name] = a, b
		priority := 100 - i*10
		if err := client.AddTransport(name, a, testSessionSecret, testSessionCtx, priority); err != nil {
			t.Fatal(err)
		}
		if err := exit.AddTransport(name, b, testSessionSecret, testSessionCtx, priority); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = client.Stop(); _ = exit.Stop() })
	return client, exit, cw, ew
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionStartsEveryTransport(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"direct", "yandex"} {
		if cw[name].starts.Load() != 1 || ew[name].starts.Load() != 1 {
			t.Fatalf("%s started client=%d exit=%d, want 1 each",
				name, cw[name].starts.Load(), ew[name].starts.Load())
		}
	}
}

func TestSessionRetriesFailedCarrier(t *testing.T) {
	client, exit, cw, _ := linkedSessions(t, "direct", "yandex")
	cw["yandex"].failures.Store(2)
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("a failing secondary carrier must not block the session: %v", err)
	}
	eventually(t, "the failed carrier to be retried", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.links["yandex"].started
	})
	if n := cw["yandex"].starts.Load(); n != 3 {
		t.Fatalf("yandex Start called %d times, want 3", n)
	}
}

func TestSessionExitStartsWithoutClient(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct")
	began := time.Now()
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 100*time.Millisecond {
		t.Fatalf("exit Start blocked %v waiting for a client", took)
	}

	time.Sleep(200 * time.Millisecond)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the exit to become ready", exit.IsConnected)
}
