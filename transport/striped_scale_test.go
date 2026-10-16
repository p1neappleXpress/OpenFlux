package transport

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// delayLane simulates a lossless bidirectional link with a fixed round-trip
// delay, like an HTTP-based collaborative editor (Avito, Mail.ru, etc.).
// Send blocks for half the RTT (the request-ack cycle); the peer's callback
// fires after the other half.
type delayLane struct {
	peer  *delayLane
	half  time.Duration // one-way delay = RTT/2
	mu    sync.Mutex
	cb    func([]byte)
	alive atomic.Bool
}

func (l *delayLane) Start() error   { l.alive.Store(true); return nil }
func (l *delayLane) Stop() error    { l.alive.Store(false); return nil }
func (l *delayLane) IsConnected() bool { return l.alive.Load() }
func (l *delayLane) Stats() TransportStats { return TransportStats{Connected: l.alive.Load()} }

func (l *delayLane) Receive(cb func([]byte)) {
	l.mu.Lock()
	l.cb = cb
	l.mu.Unlock()
}

func (l *delayLane) Send(b []byte) error {
	time.Sleep(l.half)
	cp := append([]byte(nil), b...)
	go func() {
		time.Sleep(l.half)
		l.peer.mu.Lock()
		cb := l.peer.cb
		l.peer.mu.Unlock()
		if cb != nil {
			cb(cp)
		}
	}()
	return nil
}

func newDelayPair(rtt time.Duration) (*delayLane, *delayLane) {
	a := &delayLane{half: rtt / 2}
	b := &delayLane{half: rtt / 2}
	a.peer, b.peer = b, a
	return a, b
}

func TestStripedThroughputScalesWithLanes(t *testing.T) {
	if raceEnabled {
		// Also: it swaps the package-wide stripePieceBytes under stripes
		// whose goroutines are still winding down.
		t.Skip("a throughput measurement: meaningless under the race detector")
	}
	const (
		rtt       = 130 * time.Millisecond
		frameSize = 32 * 1024
		duration  = 4 * time.Second
	)

	// WebSocket-based transports carry much larger pieces than Telemost's
	// 1312-byte SFU fragments. Match the piece size to what fits in one
	// lane op (e.g. 64 KB for ShareDB's 1 MB maxPayload).
	saved := stripePieceBytes
	stripePieceBytes = 64 * 1024
	defer func() { stripePieceBytes = saved }()

	for _, nLanes := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("%d-lanes", nLanes), func(t *testing.T) {
			var aLanes, bLanes []Transport
			for i := 0; i < nLanes; i++ {
				a, b := newDelayPair(rtt)
				aLanes = append(aLanes, a)
				bLanes = append(bLanes, b)
			}
			A := NewStripedTransport(aLanes)
			B := NewStripedTransport(bLanes)

			var received atomic.Uint64
			B.Receive(func(p []byte) {
				received.Add(uint64(len(p)))
			})

			if err := A.Start(); err != nil {
				t.Fatal(err)
			}
			if err := B.Start(); err != nil {
				t.Fatal(err)
			}
			defer A.Stop()
			defer B.Stop()

			stop := make(chan struct{})
			var sent atomic.Uint64
			go func() {
				seq := uint32(0)
				frame := make([]byte, frameSize)
				for {
					select {
					case <-stop:
						return
					default:
					}
					binary.BigEndian.PutUint32(frame, seq)
					seq++
					if err := A.Send(frame); err != nil {
						continue
					}
					sent.Add(uint64(frameSize))
				}
			}()

			time.Sleep(duration)
			close(stop)
			time.Sleep(time.Second) // drain

			s := sent.Load()
			r := received.Load()
			mbps := float64(r) / duration.Seconds() / 1024 / 1024
			t.Logf("lanes=%d sent=%.1f MB received=%.1f MB throughput=%.2f MB/s",
				nLanes,
				float64(s)/1024/1024,
				float64(r)/1024/1024,
				mbps,
			)
			if nLanes > 1 && mbps < 0.1*float64(nLanes) {
				t.Errorf("throughput %.2f MB/s is less than 0.1 MB/s per lane", mbps)
			}
		})
	}
}
