package transport

import (
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTransport is a minimal Transport for exercising MultiplexTransport wiring.
type fakeTransport struct {
	id        int
	connected atomic.Bool
	sent      atomic.Int32
	cb        func([]byte)
	mu        sync.Mutex
}

func (f *fakeTransport) Start() error { f.connected.Store(true); return nil }
func (f *fakeTransport) Stop() error  { f.connected.Store(false); return nil }
func (f *fakeTransport) Send(data []byte) error {
	f.sent.Add(1)
	return nil
}
func (f *fakeTransport) Receive(cb func([]byte)) {
	f.mu.Lock()
	f.cb = cb
	f.mu.Unlock()
}
func (f *fakeTransport) IsConnected() bool { return f.connected.Load() }
func (f *fakeTransport) Stats() TransportStats {
	return TransportStats{PacketsSent: uint64(f.sent.Load()), Connected: f.connected.Load()}
}
func (f *fakeTransport) deliver(pkt []byte) {
	f.mu.Lock()
	cb := f.cb
	f.mu.Unlock()
	if cb != nil {
		cb(pkt)
	}
}

func newFakes(n int) []Transport {
	out := make([]Transport, n)
	for i := range out {
		out[i] = &fakeTransport{id: i}
	}
	return out
}

// TestMuxRoutesAcrossChannels: distinct flows must reach more than one channel.
func TestMuxRoutesAcrossChannels(t *testing.T) {
	fakes := newFakes(4)
	m := NewMultiplexTransport(fakes)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2000; i++ {
		pkt := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{1, 2, byte(i >> 8), byte(i)}, uint16(40000+i), 443)
		if err := m.Send(pkt); err != nil {
			t.Fatal(err)
		}
	}

	used := 0
	for _, f := range fakes {
		if f.(*fakeTransport).sent.Load() > 0 {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("expected traffic on multiple channels, only %d used", used)
	}
}

// TestMuxFlowAffinity: all packets of one flow must land on the same channel.
func TestMuxFlowAffinity(t *testing.T) {
	fakes := newFakes(4)
	m := NewMultiplexTransport(fakes)
	_ = m.Start()

	pkt := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 51000, 443)
	for i := 0; i < 50; i++ {
		_ = m.Send(pkt)
	}

	nonZero := 0
	for _, f := range fakes {
		if c := f.(*fakeTransport).sent.Load(); c > 0 {
			nonZero++
			if c != 50 {
				t.Fatalf("flow split: a channel got %d of 50 packets", c)
			}
		}
	}
	if nonZero != 1 {
		t.Fatalf("one flow spread over %d channels, want 1", nonZero)
	}
}

// TestMuxFailover: a disconnected channel must never be selected.
func TestMuxFailover(t *testing.T) {
	fakes := newFakes(3)
	m := NewMultiplexTransport(fakes)
	_ = m.Start()

	// Kill channel 1; its flows must rehash onto the survivors.
	fakes[1].(*fakeTransport).connected.Store(false)

	for i := 0; i < 1000; i++ {
		pkt := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{1, 2, byte(i >> 8), byte(i)}, uint16(40000+i), 443)
		_ = m.Send(pkt)
	}

	if got := fakes[1].(*fakeTransport).sent.Load(); got != 0 {
		t.Fatalf("dead channel received %d packets, want 0", got)
	}

	// Kill everything -> Send must error, not panic.
	for _, f := range fakes {
		f.(*fakeTransport).connected.Store(false)
	}
	if err := m.Send([]byte{0x45, 0, 0, 0}); err == nil {
		t.Fatal("expected error when no channel is connected")
	}
}

// TestMuxReceiveMerge: packets from any channel reach the single upstream cb.
func TestMuxReceiveMerge(t *testing.T) {
	fakes := newFakes(3)
	m := NewMultiplexTransport(fakes)
	_ = m.Start()

	var got atomic.Int32
	m.Receive(func([]byte) { got.Add(1) })

	for _, f := range fakes {
		f.(*fakeTransport).deliver([]byte{1, 2, 3})
	}
	if got.Load() != 3 {
		t.Fatalf("upstream callback saw %d packets, want 3", got.Load())
	}
}

// TestMuxStatsAggregate: Stats sums across channels and reports connectivity.
func TestMuxStatsAggregate(t *testing.T) {
	fakes := newFakes(2)
	m := NewMultiplexTransport(fakes)
	_ = m.Start()

	fakes[0].(*fakeTransport).sent.Store(5)
	fakes[1].(*fakeTransport).sent.Store(7)

	s := m.Stats()
	if s.PacketsSent != 12 {
		t.Fatalf("aggregate PacketsSent = %d, want 12", s.PacketsSent)
	}
	if !s.Connected {
		t.Fatal("expected Connected=true when channels are up")
	}
	if !m.IsConnected() {
		t.Fatal("IsConnected should be true")
	}
}
