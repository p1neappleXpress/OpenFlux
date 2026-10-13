package transport

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// delayWire is one end of an in-memory carrier with a one-way delay: what
// is sent arrives, in order, delay later. Frames over 900 bytes (data
// packets in these tests) are counted, and down drops everything sent.
type delayWire struct {
	mu    sync.Mutex
	cb    func([]byte)
	out   chan wireFrame
	delay time.Duration
	down  atomic.Bool
	data  atomic.Int64
}

type wireFrame struct {
	p  []byte
	at time.Time
}

func delayPair(delay time.Duration) (*delayWire, *delayWire) {
	a := &delayWire{out: make(chan wireFrame, 8192), delay: delay}
	b := &delayWire{out: make(chan wireFrame, 8192), delay: delay}
	pump := func(from, to *delayWire) {
		for d := range from.out {
			time.Sleep(time.Until(d.at.Add(from.delay)))
			to.mu.Lock()
			cb := to.cb
			to.mu.Unlock()
			if cb != nil {
				cb(d.p)
			}
		}
	}
	go pump(a, b)
	go pump(b, a)
	return a, b
}

func (w *delayWire) Start() error          { return nil }
func (w *delayWire) Stop() error           { return nil }
func (w *delayWire) IsConnected() bool     { return true }
func (w *delayWire) Stats() TransportStats { return TransportStats{} }
func (w *delayWire) Receive(cb func([]byte)) {
	w.mu.Lock()
	w.cb = cb
	w.mu.Unlock()
}
func (w *delayWire) Send(p []byte) error {
	if w.down.Load() {
		return nil
	}
	if len(p) > 900 {
		w.data.Add(1)
	}
	w.out <- wireFrame{append([]byte(nil), p...), time.Now()}
	return nil
}

// bondPeers starts a client and an exit joined by two carriers of equal
// priority, "a" (fast) and "b" (slow).
func bondPeers(t *testing.T, clientBonds, exitBonds bool) (cl, ex *Session, a, b [2]*delayWire) {
	t.Helper()
	params := PeerParameters{Capabilities: control.CapabilityIPv4 | control.CapabilityTCP | control.CapabilityUDP, MaxPacketSize: MaxNegotiatedPacket}
	var err error
	if cl, err = NewSession(params, false); err != nil {
		t.Fatal(err)
	}
	if ex, err = NewSession(params, true); err != nil {
		t.Fatal(err)
	}
	cl.SetBonding(clientBonds)
	ex.SetBonding(exitBonds)
	a[0], a[1] = delayPair(10 * time.Millisecond)
	b[0], b[1] = delayPair(40 * time.Millisecond)
	for _, x := range []struct {
		s    *Session
		a, b *delayWire
	}{{cl, a[0], b[0]}, {ex, a[1], b[1]}} {
		if err := x.s.AddTransport("a", x.a, compatSecret, "ctx", 100); err != nil {
			t.Fatal(err)
		}
		if err := x.s.AddTransport("b", x.b, compatSecret, "ctx", 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := ex.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Stop(); ex.Stop() })
	return
}

func waitBonding(t *testing.T, s ...*Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, x := range s {
		for !x.Bonding() {
			if time.Now().After(deadline) {
				t.Fatal("bonding not agreed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// numbered is a 1000-byte packet of protocol proto from source port port,
// carrying n.
func numbered(n uint32, proto byte, port uint16) []byte {
	p := testIPv4(1000, proto)
	rand.Read(p[44:]) // incompressible, so the carriers' frames show the split
	binary.BigEndian.PutUint16(p[20:], port)
	binary.BigEndian.PutUint32(p[40:], n)
	return p
}

// sendNumbered sends count numbered packets (packet i from port(i)),
// calling at(i) before each and pausing pause every 4, and collects what
// the other side delivers until count arrived or wait passed.
func sendNumbered(t *testing.T, from, to *Session, count int, proto byte, port func(i int) uint16, pause time.Duration, wait time.Duration, at func(i int)) ([]uint32, time.Time) {
	t.Helper()
	var mu sync.Mutex
	var got []uint32
	var last time.Time
	done := make(chan struct{})
	to.Receive(func(p []byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, binary.BigEndian.Uint32(p[40:]))
		last = time.Now()
		if len(got) == count {
			close(done)
		}
	})
	for i := 1; i <= count; i++ {
		if at != nil {
			at(i)
		}
		if err := from.Send(numbered(uint32(i), proto, port(i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if i%4 == 0 {
			time.Sleep(pause)
		}
	}
	select {
	case <-done:
	case <-time.After(wait):
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]uint32(nil), got...), last
}

func onePort(int) uint16 { return 40000 }

func shareOK(a, b int64, pct int64) bool { return min(a, b)*100 >= (a+b)*pct }

// A TCP connection rides one carrier: split over documents its packets
// overtake each other and TCP slows down.
func TestBondingKeepsATCPConnectionOnOneCarrier(t *testing.T) {
	cl, ex, a, b := bondPeers(t, true, true)
	waitBonding(t, cl, ex)
	got, _ := sendNumbered(t, cl, ex, 2000, 6, onePort, time.Millisecond, 10*time.Second, nil)
	if len(got) != 2000 {
		t.Fatalf("%d of 2000 arrived", len(got))
	}
	for i, n := range got {
		if n != uint32(i+1) {
			t.Fatalf("out of order at %d: %d", i, n)
		}
	}
	if na, nb := a[0].data.Load(), b[0].data.Load(); shareOK(na, nb, 5) {
		t.Fatalf("one connection split over both carriers (a=%d b=%d frames)", na, nb)
	}
}

// Several TCP connections spread over the carriers.
func TestBondingSpreadsTCPConnections(t *testing.T) {
	cl, ex, a, b := bondPeers(t, true, true)
	waitBonding(t, cl, ex)
	got, _ := sendNumbered(t, cl, ex, 2000, 6, func(i int) uint16 { return uint16(40000 + i%8) }, time.Millisecond, 10*time.Second, nil)
	if len(got) != 2000 {
		t.Fatalf("%d of 2000 arrived", len(got))
	}
	if na, nb := a[0].data.Load(), b[0].data.Load(); !shareOK(na, nb, 15) {
		t.Fatalf("connections not spread: a=%d b=%d frames", na, nb)
	}
	if st := cl.BondStats(); len(st) != 2 {
		t.Fatalf("bond stats %v", st)
	}
}

// UDP is split packet by packet: SRT and QUIC reorder in their own buffers.
func TestBondingSplitsUDP(t *testing.T) {
	cl, ex, a, b := bondPeers(t, true, true)
	waitBonding(t, cl, ex)
	got, _ := sendNumbered(t, cl, ex, 2000, 17, onePort, time.Millisecond, 10*time.Second, nil)
	if len(got) != 2000 {
		t.Fatalf("%d of 2000 arrived", len(got))
	}
	if na, nb := a[0].data.Load(), b[0].data.Load(); !shareOK(na, nb, 15) {
		t.Fatalf("UDP not split: a=%d b=%d frames", na, nb)
	}
}

func TestBondingNotWithAnOldExit(t *testing.T) {
	cl, ex, a, b := bondPeers(t, true, false)
	got, _ := sendNumbered(t, cl, ex, 500, 17, onePort, time.Millisecond, 10*time.Second, nil)
	if len(got) != 500 {
		t.Fatalf("%d of 500 arrived", len(got))
	}
	if cl.Bonding() || ex.Bonding() {
		t.Fatal("bonding on with an exit that does not bond")
	}
	// Routing by flow: the flow stays on one carrier, but for the first
	// frames, sent while the other one had not been heard from yet.
	if na, nb := a[0].data.Load(), b[0].data.Load(); shareOK(na, nb, 20) {
		t.Fatalf("one flow split over both carriers (a=%d b=%d) without bonding", na, nb)
	}
}

// The carrier of a TCP connection dies: the connection moves to the other
// one once its packets there count as lost.
func TestBondingMovesAConnectionOffADeadCarrier(t *testing.T) {
	cl, ex, a, b := bondPeers(t, true, true)
	waitBonding(t, cl, ex)
	var killed time.Time
	got, last := sendNumbered(t, cl, ex, 1600, 6, onePort, 10*time.Millisecond, 6*time.Second, func(i int) {
		if i != 400 {
			return
		}
		dead := b
		if a[0].data.Load() > b[0].data.Load() {
			dead = a
		}
		dead[0].down.Store(true)
		dead[1].down.Store(true)
		killed = time.Now()
	})
	t.Logf("%d of 1600 arrived; the last %v after the carrier died", len(got), last.Sub(killed).Round(time.Millisecond))
	if n := len(got); n > 0 {
		t.Logf("tail %v", got[max(0, n-6):])
	}
	if len(got) == 0 || got[len(got)-1] != 1600 {
		t.Fatal("the connection did not carry on after its carrier died")
	}
	if len(got) < 1200 {
		t.Fatalf("only %d of 1600 arrived: the move took too long", len(got))
	}
}
