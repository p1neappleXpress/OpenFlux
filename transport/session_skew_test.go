package transport

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openflux/transport/control"
)

// delayedWire delivers to its peer after a fixed delay, in order, like a
// carrier with that much latency.
type delayedWire struct {
	negotiationWire
	delay time.Duration

	once sync.Once
	q    chan delayed
}

type delayed struct {
	due time.Time
	p   []byte
}

func (w *delayedWire) Send(p []byte) error {
	w.once.Do(func() {
		w.q = make(chan delayed, 1<<16)
		go func() {
			for d := range w.q {
				time.Sleep(time.Until(d.due))
				_ = w.negotiationWire.Send(d.p)
			}
		}()
	})
	w.q <- delayed{time.Now().Add(w.delay), append([]byte(nil), p...)}
	return nil
}

// Carriers of equal priority share flows between them. When one is much
// slower, its packets arrive long after later-numbered ones came through
// the fast one, and every one of them must still be accepted.
func TestSessionEqualPriorityCarriersWithLatencySkew(t *testing.T) {
	params := PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP | control.CapabilityUDP,
		MaxPacketSize: 1500,
	}
	client, _ := NewSession(params, false)
	exit, _ := NewSession(params, true)
	t.Cleanup(func() { _ = client.Stop(); _ = exit.Stop() })
	for _, c := range []struct {
		name  string
		delay time.Duration
	}{{"yandex", 0}, {"vyandex", 150 * time.Millisecond}} {
		cw := &delayedWire{delay: c.delay}
		ew := &delayedWire{delay: c.delay}
		cw.peer, ew.peer = &ew.negotiationWire, &cw.negotiationWire
		if err := client.AddTransport(c.name, cw, testSessionSecret, testSessionCtx, 50); err != nil {
			t.Fatal(err)
		}
		if err := exit.AddTransport(c.name, ew, testSessionSecret, testSessionCtx, 50); err != nil {
			t.Fatal(err)
		}
	}
	var got atomic.Int64
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	eventually(t, "both carriers heard", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return len(client.liveLinksLocked()) == 2
	})

	const total = 3000
	sent := 0
	for i := 0; i < total; i++ {
		p := testIPv4(40, 17)
		copy(p[12:], []byte{10, 10, 10, 2})
		copy(p[16:], []byte{8, 8, 8, 8})
		binary.BigEndian.PutUint16(p[20:], uint16(10000+i)) // one flow per packet
		binary.BigEndian.PutUint16(p[22:], 53)
		if client.Send(p) == nil {
			sent++
		}
		if i%100 == 99 {
			time.Sleep(2 * time.Millisecond) // ~50k packets/s, within the batch queue
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for got.Load() < int64(sent) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := got.Load(); n < int64(sent) {
		t.Fatalf("delivered %d of %d packets", n, sent)
	}
	if sent < total*9/10 {
		t.Fatalf("only %d of %d packets could be sent", sent, total)
	}
}

func TestReplayWindowRing(t *testing.T) {
	s := &Session{}
	for seq := uint64(1); seq <= 10000; seq++ {
		if !s.acceptSequenceLocked(seq) {
			t.Fatalf("fresh seq %d rejected", seq)
		}
	}
	for _, c := range []struct {
		seq  uint64
		want bool
	}{
		{10000, false},                        // newest, again
		{10000 - replayWindowSize + 1, false}, // oldest still in the window, again
		{10000 - replayWindowSize, false},     // out of the window
		{0, false},
		{10000 + replayWindowSize - 1, true}, // jump: its slot held seq 10000-1
		{10000 - 1, false},                   // same slot as the jump target
		{10000 + 5, true},                    // skipped over by the jump, fresh
		{10000 + 5, false},
	} {
		if got := s.acceptSequenceLocked(c.seq); got != c.want {
			t.Fatalf("seq %d accepted=%v, want %v", c.seq, got, c.want)
		}
	}
}
