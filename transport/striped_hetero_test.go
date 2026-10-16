package transport

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// Lanes of very different speed: without a per-lane window the slow lane is
// handed as many pieces as the fast one and holds the whole stream back; with
// the delay-based window it takes what it can carry and the fast lanes the rest.
// queueLane is a lossless carrier with a fast non-blocking Send, a queue and
// a limited service rate (a chat, a document, a room): what it cannot carry
// yet waits, and the wait is its latency.
type queueLane struct {
	peer  *queueLane
	rate  float64       // bytes/s
	half  time.Duration // one-way delay
	q     chan []byte
	cb    atomic.Pointer[func([]byte)]
	alive atomic.Bool
}

func (l *queueLane) Start() error {
	l.alive.Store(true)
	go func() {
		for b := range l.q {
			time.Sleep(time.Duration(float64(len(b)) / l.rate * float64(time.Second)))
			cp := b
			go func() {
				time.Sleep(l.half)
				if f := l.peer.cb.Load(); f != nil {
					(*f)(cp)
				}
			}()
		}
	}()
	return nil
}
func (l *queueLane) Stop() error             { l.alive.Store(false); return nil }
func (l *queueLane) IsConnected() bool       { return l.alive.Load() }
func (l *queueLane) Stats() TransportStats   { return TransportStats{Connected: l.alive.Load()} }
func (l *queueLane) Receive(cb func([]byte)) { l.cb.Store(&cb) }
func (l *queueLane) Send(b []byte) error {
	select {
	case l.q <- append([]byte(nil), b...):
		return nil
	default:
		return nil // the carrier's queue overflowed: a loss
	}
}

func newQueuePair(rate float64, rtt time.Duration) (*queueLane, *queueLane) {
	a := &queueLane{rate: rate, half: rtt / 2, q: make(chan []byte, 4096)}
	b := &queueLane{rate: rate, half: rtt / 2, q: make(chan []byte, 4096)}
	a.peer, b.peer = b, a
	return a, b
}

func TestStripedHeteroLanes(t *testing.T) {
	if raceEnabled {
		t.Skip("a throughput measurement: meaningless under the race detector")
	}
	saved := stripePieceBytes
	stripePieceBytes = 32 * 1024
	defer func() { stripePieceBytes = saved }()

	run := func(window bool) float64 {
		// two fast lanes (1.2 MB/s each) and one slow (60 KB/s), 100 ms rtt
		rates := []float64{1.2 * (1 << 20), 1.2 * (1 << 20), 60 * 1024}
		var aL, bL []Transport
		for _, r := range rates {
			a, b := newQueuePair(r, 100*time.Millisecond)
			aL, bL = append(aL, a), append(bL, b)
		}
		A, B := NewStripedTransport(aL), NewStripedTransport(bL)
		for _, s := range []*StripedTransport{A, B} {
			s.SetOptions(StripeOptions{PieceBytes: 32 * 1024, NoDups: true, DeadAfter: 5 * time.Second,
				InitRTO: time.Second, MinRTO: time.Second, MaxRTO: 8 * time.Second, HoleDeadline: 20 * time.Second,
				LaneWindow: window})
		}
		var got atomic.Uint64
		B.Receive(func(p []byte) { got.Add(uint64(len(p))) })
		if err := A.Start(); err != nil {
			t.Fatal(err)
		}
		if err := B.Start(); err != nil {
			t.Fatal(err)
		}
		defer A.Stop()
		defer B.Stop()
		stop := make(chan struct{})
		go func() {
			frame := make([]byte, 32*1024)
			for seq := uint32(0); ; seq++ {
				select {
				case <-stop:
					return
				default:
				}
				binary.BigEndian.PutUint32(frame, seq)
				_ = A.Send(frame)
			}
		}()
		time.Sleep(3 * time.Second)
		start, base := time.Now(), got.Load()
		time.Sleep(8 * time.Second)
		rate := float64(got.Load()-base) / time.Since(start).Seconds() / (1 << 20)
		close(stop)
		return rate
	}
	off, on := run(false), run(true)
	t.Logf("hetero lanes (1.2+1.2+0.06 MB/s): window off %.2f MB/s, on %.2f MB/s", off, on)
	// the lanes add up to ~2.46 MB/s; unwindowed, the slow lane stalls them
	if on < 1.5 || on < 2*off {
		t.Errorf("the lane window should let lanes of different speed add up: off %.2f, on %.2f MB/s", off, on)
	}
}
