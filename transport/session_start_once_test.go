package transport

import (
	"errors"
	"sync/atomic"
	"testing"
)

// startCountingWire counts Start calls on a negotiationWire and can be made
// to fail the first few of them.
type startCountingWire struct {
	negotiationWire
	starts   atomic.Int32
	failures atomic.Int32
}

func (w *startCountingWire) Start() error {
	w.starts.Add(1)
	if w.failures.Add(-1) >= 0 {
		return errors.New("carrier not ready")
	}
	return nil
}

// Each carrier must be started exactly once: the batched wrapper already
// starts the raw transport through the encryption layer.
func TestSessionStartsCarrierOnce(t *testing.T) {
	a, b := &startCountingWire{}, &startCountingWire{}
	a.peer, b.peer = &b.negotiationWire, &a.negotiationWire
	params := PeerParameters{Capabilities: CapabilityIPv4 | CapabilityTCP, MaxPacketSize: 1500}
	client, _ := NewSession(params, false)
	exit, _ := NewSession(params, true)
	t.Cleanup(func() { _ = client.Stop(); _ = exit.Stop() })
	if err := client.AddTransport("primary", a, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}
	if err := exit.AddTransport("primary", b, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { errs <- client.Start() }()
	go func() { errs <- exit.Start() }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	extra := &startCountingWire{}
	if err := client.AddTransportPostStart("extra", extra, testSessionSecret, testSessionCtx, 50); err != nil {
		t.Fatal(err)
	}
	if a.starts.Load() != 1 || b.starts.Load() != 1 || extra.starts.Load() != 1 {
		t.Fatalf("carrier starts: client=%d exit=%d post-start=%d, want 1 each",
			a.starts.Load(), b.starts.Load(), extra.starts.Load())
	}
}
