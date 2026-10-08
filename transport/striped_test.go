package transport

import (
	"encoding/binary"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestReorderBufferOrderAndSkip(t *testing.T) {
	old := arqEnabled
	arqEnabled = false
	defer func() { arqEnabled = old }()
	var mu sync.Mutex
	var got []byte
	r := newReorderBuffer(func(b []byte, _ bool) { mu.Lock(); got = append(got, b[0]); mu.Unlock() })
	done := make(chan struct{})
	defer close(done)
	go r.run(done)

	r.push(10, []byte{10})
	r.push(12, []byte{12}) // 11 missing
	r.push(11, []byte{11}) // fills hole
	r.push(14, []byte{14}) // 13 never arrives -> skipped after timeout
	time.Sleep(500 * time.Millisecond)
	r.push(13, []byte{13}) // late, dropped
	r.push(15, []byte{15})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := []byte{10, 11, 12, 14, 15}
	if string(got) != string(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	d, sk, late, _, _ := r.snapshot()
	if d != 5 || sk != 1 || late != 1 {
		t.Fatalf("delivered=%d skipped=%d late=%d", d, sk, late)
	}
}

// lossyLane is one end of an in-memory lane that drops a fraction of what it
// sends and delivers the rest after a small delay.
type lossyLane struct {
	peer  *lossyLane
	loss  float64
	delay time.Duration
	rnd   *rand.Rand
	mu    sync.Mutex
	cb    func([]byte)
}

func (l *lossyLane) Start() error { return nil }
func (l *lossyLane) Stop() error  { return nil }
func (l *lossyLane) Send(b []byte) error {
	l.mu.Lock()
	drop := l.rnd.Float64() < l.loss
	l.mu.Unlock()
	if drop {
		return nil
	}
	cp := append([]byte(nil), b...)
	time.AfterFunc(l.delay, func() {
		l.peer.mu.Lock()
		cb := l.peer.cb
		l.peer.mu.Unlock()
		if cb != nil {
			cb(cp)
		}
	})
	return nil
}
func (l *lossyLane) Receive(cb func([]byte)) { l.mu.Lock(); l.cb = cb; l.mu.Unlock() }
func (l *lossyLane) IsConnected() bool       { return true }
func (l *lossyLane) Stats() TransportStats   { return TransportStats{Connected: true} }

func TestStripedARQRecoversLoss(t *testing.T) {
	const lanes, frames = 3, 3000
	var aLanes, bLanes []Transport
	for i := 0; i < lanes; i++ {
		a := &lossyLane{loss: 0.25, delay: 30 * time.Millisecond, rnd: rand.New(rand.NewSource(int64(i + 1)))}
		b := &lossyLane{loss: 0.25, delay: 30 * time.Millisecond, rnd: rand.New(rand.NewSource(int64(i + 100)))}
		a.peer, b.peer = b, a
		aLanes, bLanes = append(aLanes, a), append(bLanes, b)
	}
	A, B := NewStripedTransport(aLanes), NewStripedTransport(bLanes)
	var mu sync.Mutex
	var got []uint32
	B.Receive(func(p []byte) { mu.Lock(); got = append(got, binary.BigEndian.Uint32(p)); mu.Unlock() })
	if err := A.Start(); err != nil {
		t.Fatal(err)
	}
	if err := B.Start(); err != nil {
		t.Fatal(err)
	}
	defer A.Stop()
	defer B.Stop()
	for i := 0; i < frames; i++ {
		p := make([]byte, 100)
		binary.BigEndian.PutUint32(p, uint32(i))
		if err := A.Send(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= frames {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("out of order at %d: %d after %d", i, got[i], got[i-1])
		}
	}
	if len(got) < frames*99/100 {
		t.Fatalf("delivered %d of %d with 25%% loss per lane; rtx=%d nack=%d", len(got), frames, A.rtxSent.Load(), B.nackSent.Load())
	}
	t.Logf("delivered %d/%d, nacks=%d rtx=%d", len(got), frames, B.nackSent.Load(), A.rtxSent.Load())
}

// With ARQ off, FEC alone must hide 15% random loss from the receiver.
func TestStripedFECAlone(t *testing.T) {
	old := arqEnabled
	arqEnabled = false
	defer func() { arqEnabled = old }()
	const lanes, frames = 3, 4000
	var aLanes, bLanes []Transport
	for i := 0; i < lanes; i++ {
		a := &lossyLane{loss: 0.15, delay: 20 * time.Millisecond, rnd: rand.New(rand.NewSource(int64(i + 11)))}
		b := &lossyLane{loss: 0.15, delay: 20 * time.Millisecond, rnd: rand.New(rand.NewSource(int64(i + 111)))}
		a.peer, b.peer = b, a
		aLanes, bLanes = append(aLanes, a), append(bLanes, b)
	}
	A, B := NewStripedTransport(aLanes), NewStripedTransport(bLanes)
	var mu sync.Mutex
	var got []uint32
	B.Receive(func(p []byte) {
		if p[len(p)-1] != byte(binary.BigEndian.Uint32(p)) {
			t.Errorf("corrupt reassembly: len=%d", len(p))
		}
		mu.Lock()
		got = append(got, binary.BigEndian.Uint32(p))
		mu.Unlock()
	})
	if err := A.Start(); err != nil {
		t.Fatal(err)
	}
	if err := B.Start(); err != nil {
		t.Fatal(err)
	}
	defer A.Stop()
	defer B.Stop()
	for i := 0; i < frames; i++ {
		n := 200
		if i%4 == 0 {
			n = 3000 // three pieces
		}
		p := make([]byte, n)
		binary.BigEndian.PutUint32(p, uint32(i))
		p[n-1] = byte(i)
		if err := A.Send(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Microsecond)
	}
	time.Sleep(2 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("out of order at %d", i)
		}
	}
	t.Logf("delivered %d/%d, fec recovered=%d parity=%d", len(got), frames, B.fecRecovered.Load(), A.fecParitySent.Load())
	if len(got) < frames*97/100 {
		t.Fatalf("FEC alone delivered only %d of %d at 15%% loss", len(got), frames)
	}
}
