package transport

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"openflux/transport/control"
)

// negotiationWire is a fake in-process transport: whatever is Sent goes
// straight to the peer's Receive callback. Used to test Session end-to-end
// without a real carrier.
type negotiationWire struct {
	mu      sync.Mutex
	cb      func([]byte)
	peer    *negotiationWire
	packets [][]byte
	drop    bool
}

func (w *negotiationWire) Start() error            { return nil }
func (w *negotiationWire) Stop() error             { return nil }
func (w *negotiationWire) IsConnected() bool       { return true }
func (w *negotiationWire) Stats() TransportStats   { return TransportStats{} }
func (w *negotiationWire) Receive(cb func([]byte)) { w.mu.Lock(); w.cb = cb; w.mu.Unlock() }
func (w *negotiationWire) Send(p []byte) error {
	p = append([]byte(nil), p...)
	w.mu.Lock()
	w.packets = append(w.packets, p)
	drop := w.drop
	w.mu.Unlock()
	if w.peer != nil && !drop {
		w.peer.mu.Lock()
		cb := w.peer.cb
		w.peer.mu.Unlock()
		if cb != nil {
			cb(p)
		}
	}
	return nil
}

const (
	testSessionSecret = "session test secret 123456"
	testSessionCtx    = "test"
)

// sessionPair builds two Sessions connected by fake transports.
func sessionPair(t *testing.T) (*Session, *Session, *negotiationWire, *negotiationWire) {
	t.Helper()
	a, b := &negotiationWire{}, &negotiationWire{}
	a.peer = b
	b.peer = a

	pa := PeerParameters{
		Capabilities: control.CapabilityIPv4 | control.CapabilityTCP |
			control.CapabilityUDP | control.CapabilityICMPErrors,
		MaxPacketSize: 1500,
	}
	ca, err := NewSession(pa, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.AddTransport("primary", a, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}

	pb := pa
	pb.MaxPacketSize = 1280
	cb, err := NewSession(pb, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.AddTransport("primary", b, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ca.Stop(); _ = cb.Stop() })
	return ca, cb, a, b
}

func testIPv4(size int, proto byte) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	p[9] = proto
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	return p
}

func TestSessionHandshakeAndData(t *testing.T) {
	a, b, _, _ := sessionPair(t)

	ch := make(chan []byte, 1)
	b.Receive(func(p []byte) { ch <- append([]byte(nil), p...) })

	errs := make(chan error, 2)
	go func() { errs <- a.Start() }()
	go func() { errs <- b.Start() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("handshake timeout")
		}
	}

	params, ready := a.PeerParameters()
	if !ready || params.MaxPacketSize != 1280 || params.Capabilities&control.CapabilityUDP == 0 {
		t.Fatalf("bad negotiated params: %+v ready=%v", params, ready)
	}

	p := testIPv4(1280, 17)
	if err := a.Send(p); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if !bytes.Equal(p, got) {
			t.Fatal("packet changed")
		}
	case <-time.After(time.Second):
		t.Fatal("missing UDP")
	}
	if a.Send(testIPv4(1281, 17)) == nil {
		t.Fatal("oversized packet accepted")
	}
	if a.Send(testIPv4(28, 58)) == nil {
		t.Fatal("unknown protocol accepted")
	}
}

func TestSessionControlRoundTrip(t *testing.T) {
	a, b, _, _ := sessionPair(t)
	errs := make(chan error, 2)
	go func() { errs <- a.Start() }()
	go func() { errs <- b.Start() }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	got := make(chan struct {
		sub  control.Subtype
		body []byte
	}, 1)
	b.SetControlHandler(func(sub control.Subtype, payload []byte) {
		got <- struct {
			sub  control.Subtype
			body []byte
		}{sub, append([]byte(nil), payload...)}
	})
	if err := a.SendControl(control.SubtypeCookiesRequest, []byte("please")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.sub != control.SubtypeCookiesRequest || string(m.body) != "please" {
			t.Fatalf("bad control: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("control not delivered")
	}
}

func TestSessionReplayWindow(t *testing.T) {
	a, b, _, _ := sessionPair(t)
	errs := make(chan error, 2)
	go func() { errs <- a.Start() }()
	go func() { errs <- b.Start() }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	count := 0
	b.Receive(func([]byte) { count++ })
	data := func(seq uint64) []byte {
		env := &control.Envelope{
			Kind:  control.KindIPv4,
			Role:  control.RoleClient,
			Local: b.peer,
			Peer:  b.local,
			Data:  &control.DataTail{Sequence: seq},
		}
		raw, _ := env.Encode()
		return append(raw, testIPv4(40, 6)...)
	}
	for _, seq := range []uint64{2, 1, 2, 100, 1, 99, 99, 0} {
		b.receive(b.links["primary"], data(seq))
	}
	if count != 4 {
		t.Fatalf("replay/reordering delivered %d, want 4", count)
	}
}

func TestSessionNoTransportsStartFails(t *testing.T) {
	pa := PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}
	s, err := NewSession(pa, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("Start with no transports should fail")
	}
	_ = s.Stop()
}

func TestSessionHandshakeTimeoutWithoutPeer(t *testing.T) {
	a := &negotiationWire{}
	a.drop = true
	b := &negotiationWire{}
	a.peer = b
	b.peer = a
	// B stays silent.

	pa := PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}
	s, err := NewSession(pa, false)
	if err != nil {
		t.Fatal(err)
	}
	s.handshakeTimeout = 100 * time.Millisecond
	if err := s.AddTransport("primary", a, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("expected handshake timeout")
	}
	if s.IsConnected() {
		t.Fatal("session reported connected after timeout")
	}
}
