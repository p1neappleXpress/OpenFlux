package transport

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingTransport is a fake inner transport: it records every frame Send
// receives and lets the test push frames into the registered receive callback.
type recordingTransport struct {
	connected atomic.Bool

	mu    sync.Mutex
	sent  [][]byte
	rxcb  func([]byte)
	onSnd func([]byte)
}

func (r *recordingTransport) Start() error { r.connected.Store(true); return nil }
func (r *recordingTransport) Stop() error  { r.connected.Store(false); return nil }
func (r *recordingTransport) Send(data []byte) error {
	cp := append([]byte(nil), data...)
	r.mu.Lock()
	r.sent = append(r.sent, cp)
	cb := r.onSnd
	r.mu.Unlock()
	if cb != nil {
		cb(cp)
	}
	return nil
}
func (r *recordingTransport) Receive(cb func([]byte)) { r.mu.Lock(); r.rxcb = cb; r.mu.Unlock() }
func (r *recordingTransport) IsConnected() bool       { return r.connected.Load() }
func (r *recordingTransport) Stats() TransportStats   { return TransportStats{Connected: r.connected.Load()} }

func (r *recordingTransport) push(frame []byte) {
	r.mu.Lock()
	cb := r.rxcb
	r.mu.Unlock()
	if cb != nil {
		cb(frame)
	}
}

func (r *recordingTransport) sentFrames() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, len(r.sent))
	copy(out, r.sent)
	return out
}

// TestAdaptiveReceiveDispatch: the receive path must decode BOTH families and
// deliver the original packets, and a batch frame must flip peerBatch.
func TestAdaptiveReceiveDispatch(t *testing.T) {
	inner := &recordingTransport{}
	a := NewAdaptiveTransport(inner)

	var got [][]byte
	var mu sync.Mutex
	a.Receive(func(p []byte) { mu.Lock(); got = append(got, append([]byte(nil), p...)); mu.Unlock() })

	// A legacy frame must decode without flipping peerBatch.
	pkt := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{1, 1, 1, 1}, 1234, 443)
	inner.push(compress(pkt))
	if a.peerBatch.Load() {
		t.Fatal("legacy frame must NOT flip peerBatch")
	}

	// A batch frame must decode all its packets AND flip peerBatch.
	p1 := []byte("hello-packet-one")
	p2 := []byte("second-packet-two")
	inner.push(encodeBatch([][]byte{p1, p2}))
	if !a.peerBatch.Load() {
		t.Fatal("batch frame must flip peerBatch")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("expected 3 delivered packets, got %d", len(got))
	}
	if !bytes.Equal(got[0], pkt) || !bytes.Equal(got[1], p1) || !bytes.Equal(got[2], p2) {
		t.Fatalf("delivered packets do not match originals")
	}
}

// TestAdaptiveEmptyProbeDeliversNothing: the capability probe (empty batch)
// flips peerBatch but must not deliver a phantom packet.
func TestAdaptiveEmptyProbeDeliversNothing(t *testing.T) {
	inner := &recordingTransport{}
	a := NewAdaptiveTransport(inner)

	var count atomic.Int32
	a.Receive(func([]byte) { count.Add(1) })

	inner.push(encodeBatch(nil)) // the probe
	if !a.peerBatch.Load() {
		t.Fatal("probe should flip peerBatch")
	}
	if count.Load() != 0 {
		t.Fatalf("probe must deliver 0 packets, delivered %d", count.Load())
	}
}

// TestAdaptiveSendStartsLegacyThenUpgrades: Send must emit legacy frames until
// the peer proves batch, then coalesce into batch frames.
func TestAdaptiveSendStartsLegacyThenUpgrades(t *testing.T) {
	inner := &recordingTransport{}
	inner.connected.Store(true)
	a := NewAdaptiveTransport(inner)
	a.Receive(func([]byte) {})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	// Phase 1: peer unknown -> legacy. Each Send should become a legacy frame
	// (first byte 0x00 or 0x1F), never a batch frame (0x02).
	pkt := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{1, 1, 1, 1}, 1000, 443)
	_ = a.Send(pkt)
	waitFor(t, func() bool { return len(nonProbe(inner.sentFrames())) >= 1 })

	for _, f := range nonProbe(inner.sentFrames()) {
		if f[0] == batchFormatVersion {
			t.Fatal("sent a batch frame before peer proved batch support")
		}
		// legacy frame must round-trip back to the original packet
		dec, err := decompress(f)
		if err != nil || !bytes.Equal(dec, pkt) {
			t.Fatalf("legacy frame did not round-trip: err=%v", err)
		}
	}

	// Phase 2: peer proves batch -> subsequent sends coalesce into batch frames.
	a.peerBatch.Store(true)
	before := len(inner.sentFrames())
	for i := 0; i < 20; i++ {
		_ = a.Send(pkt)
	}
	waitFor(t, func() bool {
		for _, f := range inner.sentFrames()[before:] {
			if len(f) > 0 && f[0] == batchFormatVersion && len(f) > 2 {
				return true // a non-empty batch frame appeared
			}
		}
		return false
	})
}

// TestAdaptiveForceBatch: with forceBatch set, Send must coalesce into batch
// frames from the very first packet, without the peer ever proving batch — for
// talking to a known batch-only node (upstream BatchedTransport).
func TestAdaptiveForceBatch(t *testing.T) {
	inner := &recordingTransport{}
	inner.connected.Store(true)
	a := NewAdaptiveTransport(inner)
	a.forceBatch = true
	a.Receive(func([]byte) {})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	pkt := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{1, 1, 1, 1}, 1000, 443)
	for i := 0; i < 20; i++ {
		_ = a.Send(pkt)
	}
	waitFor(t, func() bool {
		for _, f := range inner.sentFrames() {
			if len(f) > 2 && f[0] == batchFormatVersion {
				return true // a non-empty batch frame appeared without peerBatch
			}
		}
		return false
	})
	if a.peerBatch.Load() {
		t.Fatal("forceBatch must not depend on peerBatch being set")
	}
}

// TestAdaptiveOptimisticUpgrade: a silent peer after a real send flips us to
// batch (batch-only node), but the guards must hold otherwise.
func TestAdaptiveOptimisticUpgrade(t *testing.T) {
	// Silent peer, real packet sent long ago -> upgrade.
	a := NewAdaptiveTransport(&recordingTransport{})
	a.firstRealSend.Store(time.Now().Add(-2 * optimisticUpgradeDelay).UnixNano())
	a.maybeOptimisticUpgrade()
	if !a.peerBatch.Load() {
		t.Fatal("silent peer after real send should upgrade to batch")
	}

	// No real send yet (idle) -> must NOT upgrade.
	b := NewAdaptiveTransport(&recordingTransport{})
	b.maybeOptimisticUpgrade()
	if b.peerBatch.Load() {
		t.Fatal("idle connection (no real send) must not upgrade")
	}

	// Peer answered (everRx) -> live legacy peer, must NOT upgrade.
	c := NewAdaptiveTransport(&recordingTransport{})
	c.firstRealSend.Store(time.Now().Add(-2 * optimisticUpgradeDelay).UnixNano())
	c.everRx.Store(true)
	c.maybeOptimisticUpgrade()
	if c.peerBatch.Load() {
		t.Fatal("a peer that answered must stay on legacy")
	}

	// Within the grace window -> too early, must NOT upgrade.
	d := NewAdaptiveTransport(&recordingTransport{})
	d.firstRealSend.Store(time.Now().UnixNano())
	d.maybeOptimisticUpgrade()
	if d.peerBatch.Load() {
		t.Fatal("within grace window must not upgrade yet")
	}
}

// nonProbe filters out the 2-byte empty-batch probes so send-path assertions
// look only at real data frames.
func nonProbe(frames [][]byte) [][]byte {
	var out [][]byte
	for _, f := range frames {
		if len(f) == 2 && f[0] == batchFormatVersion {
			continue // empty-batch probe
		}
		out = append(out, f)
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
