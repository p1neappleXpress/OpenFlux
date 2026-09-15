package transport

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockStream is a Transport we can flip up/down and inspect. Deliberately
// separate from batched_test.go's fakeTransport because we need per-stream
// state for the multi-stream tests (that fake is loopback-only).
type mockStream struct {
	mu        sync.Mutex
	connected atomic.Bool
	sent      [][]byte
	cb        func([]byte)
	failSend  atomic.Bool
}

func (m *mockStream) Start() error { m.connected.Store(true); return nil }
func (m *mockStream) Stop() error  { m.connected.Store(false); return nil }
func (m *mockStream) Send(data []byte) error {
	if m.failSend.Load() {
		return fmt.Errorf("mock: send failure")
	}
	cp := append([]byte(nil), data...)
	m.mu.Lock()
	m.sent = append(m.sent, cp)
	m.mu.Unlock()
	return nil
}
func (m *mockStream) Receive(cb func([]byte)) {
	m.mu.Lock()
	m.cb = cb
	m.mu.Unlock()
}
func (m *mockStream) IsConnected() bool     { return m.connected.Load() }
func (m *mockStream) Stats() TransportStats { return TransportStats{Connected: m.IsConnected()} }
func (m *mockStream) inject(data []byte) {
	m.mu.Lock()
	cb := m.cb
	m.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}
func (m *mockStream) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// N=3 healthy streams should each get exactly 1/3 of the packets.
func TestMultiStreamRoundRobinHealthy(t *testing.T) {
	a, b, c := &mockStream{}, &mockStream{}, &mockStream{}
	ms := NewMultiStreamTransport([]Transport{a, b, c})
	if err := ms.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ms.Stop()

	for i := 0; i < 30; i++ {
		if err := ms.Send([]byte{byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if a.sentCount() != 10 || b.sentCount() != 10 || c.sentCount() != 10 {
		t.Fatalf("uneven RR distribution: a=%d b=%d c=%d", a.sentCount(), b.sentCount(), c.sentCount())
	}
}

// A stream that reports IsConnected()=false must be skipped without hanging or
// erroring, and every packet must land on a healthy peer.
func TestMultiStreamSkipsDisconnected(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	ms := NewMultiStreamTransport([]Transport{a, b})
	if err := ms.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ms.Stop()

	// Simulate 'a' dying (its own IsConnected flips false; MultiStream sees it
	// on the next Send).
	a.Stop()

	for i := 0; i < 10; i++ {
		if err := ms.Send([]byte{1}); err != nil {
			t.Fatalf("Send %d over 1 live stream: %v", i, err)
		}
	}
	if a.sentCount() != 0 {
		t.Fatalf("dead stream received %d packets", a.sentCount())
	}
	if b.sentCount() != 10 {
		t.Fatalf("live stream missed packets: got %d", b.sentCount())
	}
}

// Same but a healthy stream whose Send() returns an error (queue full,
// mid-reconnect) must NOT drop the packet — MultiStream tries the next.
func TestMultiStreamFailoverOnSendError(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	ms := NewMultiStreamTransport([]Transport{a, b})
	_ = ms.Start()
	defer ms.Stop()

	a.failSend.Store(true) // a is up but transiently refusing writes

	for i := 0; i < 5; i++ {
		if err := ms.Send([]byte{9}); err != nil {
			t.Fatalf("Send should have failed over to b: %v", err)
		}
	}
	if a.sentCount() != 0 {
		t.Fatalf("failing stream unexpectedly stored packets: %d", a.sentCount())
	}
	if b.sentCount() != 5 {
		t.Fatalf("failover target should have all 5: got %d", b.sentCount())
	}
}

// All streams down -> error, so the caller can back off.
func TestMultiStreamAllDown(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	ms := NewMultiStreamTransport([]Transport{a, b})
	_ = ms.Start()
	defer ms.Stop()

	a.Stop()
	b.Stop()

	if err := ms.Send([]byte("x")); err == nil {
		t.Fatal("expected error with no healthy stream")
	}
	if ms.IsConnected() {
		t.Fatal("IsConnected should be false when every stream is down")
	}
}

// Fan-in: frames from any stream must reach the user's single callback.
func TestMultiStreamReceiveFanIn(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	ms := NewMultiStreamTransport([]Transport{a, b})
	_ = ms.Start()
	defer ms.Stop()

	got := make(chan []byte, 4)
	ms.Receive(func(p []byte) { got <- p })
	a.inject([]byte("from-a"))
	b.inject([]byte("from-b"))

	var g1, g2 []byte
	select {
	case g1 = <-got:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for first inject")
	}
	select {
	case g2 = <-got:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for second inject")
	}
	okOrder1 := bytes.Equal(g1, []byte("from-a")) && bytes.Equal(g2, []byte("from-b"))
	okOrder2 := bytes.Equal(g1, []byte("from-b")) && bytes.Equal(g2, []byte("from-a"))
	if !(okOrder1 || okOrder2) {
		t.Fatalf("unexpected fan-in: %q %q", g1, g2)
	}
}

// Stats must aggregate over inner streams' counters and OR the Connected bit.
func TestMultiStreamStatsAggregation(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	// Give the mocks non-zero counters through Stats() by way of the real
	// interface: our mock's Stats() reports only Connected, so this test is
	// primarily about the Connected OR.
	ms := NewMultiStreamTransport([]Transport{a, b})
	_ = ms.Start()
	defer ms.Stop()

	if !ms.Stats().Connected {
		t.Fatal("Stats.Connected should be true when both up")
	}
	a.Stop()
	if !ms.Stats().Connected {
		t.Fatal("Stats.Connected should stay true while ANY stream is up")
	}
	b.Stop()
	if ms.Stats().Connected {
		t.Fatal("Stats.Connected should be false when all down")
	}
}

// A stream that comes back after being marked down must resume receiving
// traffic on the next round-robin tick.
func TestMultiStreamRecoversAfterFlap(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	ms := NewMultiStreamTransport([]Transport{a, b})
	_ = ms.Start()
	defer ms.Stop()

	// First: both up, 10 packets, roughly even.
	for i := 0; i < 10; i++ {
		_ = ms.Send([]byte{1})
	}
	beforeA, beforeB := a.sentCount(), b.sentCount()
	if beforeA == 0 || beforeB == 0 {
		t.Fatalf("initial distribution missed a stream: a=%d b=%d", beforeA, beforeB)
	}

	// a dies; next 10 all go to b.
	a.Stop()
	for i := 0; i < 10; i++ {
		_ = ms.Send([]byte{2})
	}
	if a.sentCount() != beforeA {
		t.Fatal("dead a should not have received traffic")
	}
	if b.sentCount()-beforeB != 10 {
		t.Fatalf("live b should have absorbed all 10 while a was down, got %d", b.sentCount()-beforeB)
	}

	// a recovers; next 10 split roughly evenly again.
	a.Start()
	beforeA2 := a.sentCount()
	for i := 0; i < 10; i++ {
		_ = ms.Send([]byte{3})
	}
	if a.sentCount()-beforeA2 == 0 {
		t.Fatal("recovered a should be receiving traffic again")
	}
}
