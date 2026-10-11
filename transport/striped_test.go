package transport

import (
	"encoding/binary"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestReorderBufferOrderAndSkip(t *testing.T) {
	old := holeDeadline
	holeDeadline = 100 * time.Millisecond
	defer func() { holeDeadline = old }()
	var mu sync.Mutex
	var got []byte
	r := newReorderBuffer(func(b []byte, _ bool) { mu.Lock(); got = append(got, b[0]); mu.Unlock() })
	done := make(chan struct{})
	defer close(done)
	go r.run(done)

	r.push(10, 10, []byte{10})
	r.push(12, 10, []byte{12}) // 11 missing
	r.push(11, 10, []byte{11}) // fills hole
	r.push(14, 10, []byte{14}) // 13 never arrives -> skipped after timeout
	time.Sleep(500 * time.Millisecond)
	r.push(13, 10, []byte{13}) // late, dropped
	r.push(15, 10, []byte{15})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := []byte{10, 11, 12, 14, 15}
	if string(got) != string(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	d, sk, late, _, _ := r.snapshot()
	_ = d
	if d != 5 || sk != 1 || late != 1 {
		t.Fatalf("delivered=%d skipped=%d late=%d", d, sk, late)
	}
}

// lossyLane is one end of an in-memory lane that drops a fraction of what it
// sends and delivers the rest in order after a small delay.
type lossyLane struct {
	peer  *lossyLane
	loss  float64
	delay time.Duration
	rnd   *rand.Rand
	mu    sync.Mutex
	cb    func([]byte)
	once  sync.Once
	wire  chan lossyPkt
}

type lossyPkt struct {
	at time.Time
	b  []byte
}

func (l *lossyLane) Start() error {
	l.once.Do(func() {
		l.wire = make(chan lossyPkt, 1<<16)
		go func() {
			for p := range l.wire {
				time.Sleep(time.Until(p.at))
				l.peer.mu.Lock()
				cb := l.peer.cb
				l.peer.mu.Unlock()
				if cb != nil {
					cb(p.b)
				}
			}
		}()
	})
	return nil
}
func (l *lossyLane) Stop() error { return nil }
func (l *lossyLane) Send(b []byte) error {
	l.mu.Lock()
	drop := l.rnd.Float64() < l.loss
	l.mu.Unlock()
	if drop {
		return nil
	}
	l.wire <- lossyPkt{at: time.Now().Add(l.delay), b: append([]byte(nil), b...)}
	return nil
}
func (l *lossyLane) Receive(cb func([]byte)) { l.mu.Lock(); l.cb = cb; l.mu.Unlock() }
func (l *lossyLane) IsConnected() bool       { return true }
func (l *lossyLane) Stats() TransportStats   { return TransportStats{Connected: true} }

func TestStripedARQRecoversLoss(t *testing.T) {
	old := codelTarget
	codelTarget = time.Minute // this is about retransmission, not queue management
	defer func() { codelTarget = old }()
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
	if len(got) != frames {
		t.Fatalf("delivered %d of %d with 25%% loss per lane; %+v", len(got), frames, A.snd.snapshot())
	}
	st := A.snd.snapshot()
	_, _, late, _, _ := B.ro.snapshot()
	t.Logf("delivered %d/%d, rtx=%d (by timeout %d), duplicates at the receiver=%d, acks sent=%d", len(got), frames, st.rtx, st.rtxTimeout, late, B.ackSent.Load())
}

// The sender reads an ack right: what it lists as missing is resent once a
// later piece of the same lane is acked, the rest is done with.
func TestStripedAckMarksLossByLaneOrder(t *testing.T) {
	var q sender
	q.init()
	now := time.Now()
	for i := 0; i < 6; i++ {
		q.enqueue([]byte{byte(i)})
	}
	first := q.seq + 1
	if out := q.pull(0, 3, now); len(out) != 3 { // seqs first..first+2 on lane 0
		t.Fatalf("pulled %d", len(out))
	}
	if out := q.pull(1, 3, now); len(out) != 3 { // first+3..first+5 on lane 1
		t.Fatalf("pulled %d", len(out))
	}
	// The receiver has first, first+2 (lane 0) and first+3 (lane 1): it
	// misses first+1 (lane 0, a later lane-0 piece arrived: lost) and
	// first+4, first+5 (lane 1, nothing later arrived: maybe in flight).
	r := newReorderBuffer(func([]byte, bool) {})
	for _, d := range []uint32{0, 2, 3} {
		r.push(first+d, first, []byte{0})
	}
	ack := r.ackFrame()
	if n := ack[9]; n != 1 {
		t.Fatalf("ack lists %d missing runs, want 1: % x", n, ack)
	}
	if !q.onAck(ack, now.Add(time.Millisecond)) {
		t.Fatal("no retransmission queued")
	}
	if len(q.rtxQ) != 1 || q.rtxQ[0] != first+1 {
		t.Fatalf("rtxQ=%v want [%d]", q.rtxQ, first+1)
	}
	if q.low != first+1 {
		t.Fatalf("low=%d want %d", q.low, first+1)
	}
	// Past the timeout the lane-1 tail is resent too.
	q.detectLoss(now.Add(time.Second))
	if len(q.rtxQ) != 3 {
		t.Fatalf("rtxQ=%v after the timeout", q.rtxQ)
	}
	// A stale ack from before a restart is ignored.
	stale := append([]byte(nil), ack...)
	binary.BigEndian.PutUint32(stale[1:], first+1_000_000)
	binary.BigEndian.PutUint32(stale[5:], first+1_000_005)
	if q.onAck(stale, now) || q.low != first+1 {
		t.Fatal("stale ack applied")
	}
}

// A receiver that sees seqs from a restarted sender starts over instead of
// dropping them as late.
func TestReorderBufferResyncsOnRestart(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	r := newReorderBuffer(func(b []byte, _ bool) { mu.Lock(); got = append(got, b[0]); mu.Unlock() })
	r.push(1_000_000, 1_000_000, []byte{1})
	r.push(1_000_001, 1_000_000, []byte{2})
	r.push(5, 5, []byte{3}) // the sender restarted at a random seq
	r.push(6, 5, []byte{4})
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if string(got) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("got %v", got)
	}
}

// A receiver whose first arrival is not the sender's first piece still waits
// for the earlier ones (the low mark in every piece tells it where to start).
func TestReorderBufferStartsAtSenderLow(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	r := newReorderBuffer(func(b []byte, _ bool) { mu.Lock(); got = append(got, b[0]); mu.Unlock() })
	r.push(101, 100, []byte{2}) // 100 was lost on the way
	r.push(102, 100, []byte{3})
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(got) != 0 {
		t.Fatalf("delivered %v before the first piece", got)
	}
	mu.Unlock()
	r.push(100, 100, []byte{1}) // its retransmission
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if string(got) != string([]byte{1, 2, 3}) {
		t.Fatalf("got %v", got)
	}
}
