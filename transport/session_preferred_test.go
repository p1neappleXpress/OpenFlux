package transport

import (
	"sync/atomic"
	"testing"
	"time"
)

// preferredWire is a carrier that declares itself preferred, as a direct
// connection to the exit does.
type preferredWire struct {
	startCountingWire
	prefer bool
}

func (w *preferredWire) Preferred() bool { return w.prefer }

// bigWire is a preferredWire that also declares it moves large IP packets.
type bigWire struct{ *preferredWire }

func (w *bigWire) CarrierMTU() int { return 16000 }

// preferredPair links a client and an exit over a "yandex" carrier of priority 100 and a
// "direct" one of priority 50, the way node profiles rank them.
func preferredPair(t *testing.T, prefer bool) (client, exit *Session, cw, ew map[string]*startCountingWire) {
	t.Helper()
	var err error
	params := testParams
	params.MaxPacketSize = 20000
	if client, err = NewSession(params, false); err != nil {
		t.Fatal(err)
	}
	if exit, err = NewSession(params, true); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{client, exit} {
		s.restartMin, s.restartMax = 10*time.Millisecond, 20*time.Millisecond
		s.handshakeTimeout = 3 * time.Second
	}
	cw, ew = map[string]*startCountingWire{}, map[string]*startCountingWire{}
	for _, c := range []struct {
		name string
		prio int
		pref bool
	}{{"yandex", 100, false}, {"direct", 50, prefer}} {
		a, b := &preferredWire{prefer: c.pref}, &preferredWire{prefer: c.pref}
		a.peer, b.peer = &b.negotiationWire, &a.negotiationWire
		cw[c.name], ew[c.name] = &a.startCountingWire, &b.startCountingWire
		var ta, tb Transport = a, b
		if c.name == "direct" { // the direct carrier declares large packets
			ta, tb = &bigWire{a}, &bigWire{b}
		}
		if err := client.AddTransport(c.name, ta, testSessionSecret, testSessionCtx, c.prio); err != nil {
			t.Fatal(err)
		}
		if err := exit.AddTransport(c.name, tb, testSessionSecret, testSessionCtx, c.prio); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = client.Stop(); _ = exit.Stop() })
	return
}

func unblackhole(wires ...*startCountingWire) {
	for _, w := range wires {
		w.mu.Lock()
		w.drop = false
		w.mu.Unlock()
	}
}

// A preferred carrier carries the traffic although the profile ranks the
// other one higher; it hands over when it stops reaching the peer and takes
// the traffic back when it returns.
func TestSessionPreferredCarrierWinsFailsOverAndReturns(t *testing.T) {
	client, exit, cw, ew := preferredPair(t, true)
	fastKeepalive(client, exit)
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	eventually(t, "keepalive support to be detected", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})

	if a := client.ActiveTransport(); a != "direct" {
		t.Fatalf("active transport = %q, want the preferred direct", a)
	}

	blackhole(cw["direct"], ew["direct"])
	eventually(t, "traffic to move to the other carrier", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return client.ActiveTransport() == "yandex"
	})

	unblackhole(cw["direct"], ew["direct"])
	eventually(t, "traffic to return to the preferred carrier", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return client.ActiveTransport() == "direct"
	})
}

// Without the preference the profile's priorities decide, as before.
func TestSessionUnpreferredCarrierKeepsProfilePriority(t *testing.T) {
	client, exit, _, _ := preferredPair(t, false)
	fastKeepalive(client, exit)
	startPair(t, client, exit)
	if a := client.ActiveTransport(); a != "yandex" {
		t.Fatalf("active transport = %q, want the higher-priority yandex", a)
	}
}

// Large packets go over the carrier that declared them; once only a carrier
// that did not is live, the packet is refused with the size that does fit, and
// the MTU offered to the stack follows the live carriers.
func TestSessionLargePacketsFollowTheLiveCarrier(t *testing.T) {
	client, exit, cw, ew := preferredPair(t, true)
	fastKeepalive(client, exit)
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	eventually(t, "keepalive support to be detected", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})

	if m := client.CarrierMTU(); m != 16000 {
		t.Fatalf("CarrierMTU with the direct carrier live = %d, want 16000", m)
	}
	if err := client.Send(testIPv4(9000, 6)); err != nil {
		t.Fatalf("large packet over the declaring carrier: %v", err)
	}
	eventually(t, "the large packet to arrive", func() bool { return got.Load() == 1 })

	blackhole(cw["direct"], ew["direct"])
	var err error
	eventually(t, "the large packet to be refused once only the other carrier is live", func() bool {
		err = client.Send(testIPv4(9000, 6))
		_, refused := err.(*PacketTooLargeError)
		return refused
	})
	if e := err.(*PacketTooLargeError); e.Limit != DefaultCarrierMTU {
		t.Fatalf("refusal limit = %d, want %d", e.Limit, DefaultCarrierMTU)
	}
	if m := client.CarrierMTU(); m != DefaultCarrierMTU {
		t.Fatalf("CarrierMTU with only the plain carrier live = %d, want %d", m, DefaultCarrierMTU)
	}
	if err := client.Send(testIPv4(1400, 6)); err != nil {
		t.Fatalf("an ordinary packet must still go over the other carrier: %v", err)
	}
}

// A session of carriers that declare nothing never refuses a packet by size.
func TestSessionWithoutDeclaredCarriersDoesNotEnforceSize(t *testing.T) {
	client, exit, _, _ := linkedSessionsWith(t, func(int) int { return 100 }, "yandex")
	client.params.MaxPacketSize, exit.params.MaxPacketSize = 20000, 20000
	startPair(t, client, exit)
	if err := client.Send(testIPv4(9000, 6)); err != nil && err != ErrNegotiationPending {
		if _, refused := err.(*PacketTooLargeError); refused {
			t.Fatalf("a session without declared carriers refused a packet by size: %v", err)
		}
	}
}

// While the preferred carrier is live the one the profile ranks higher carries
// no data. (No fast keepalive here: keepalives are traffic on every carrier.)
func TestSessionPreferredCarrierCarriesTheData(t *testing.T) {
	client, exit, cw, _ := preferredPair(t, true)
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)

	before := sentCount(cw["yandex"])
	for i := 0; i < 5; i++ {
		if err := client.Send(testIPv4(40, 6)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "data delivery", func() bool { return got.Load() == 5 })
	if after := sentCount(cw["yandex"]); after != before {
		t.Fatalf("the ranked-higher carrier carried data while the preferred one was live (%d -> %d)", before, after)
	}
}

type streamWire struct{ *bigWire }

func (streamWire) CheapBytes() bool { return true }

// The tunnel runs Reno while the traffic goes over plain streams, CUBIC otherwise.
func TestSessionReportsReliableStreamOnlyForStreamCarriers(t *testing.T) {
	client, exit, _, _ := preferredPair(t, true)
	fastKeepalive(client, exit)
	startPair(t, client, exit)
	if client.OnReliableStream() {
		t.Fatal("carriers that are not streams were reported as streams")
	}

	c2, e2, _, _ := linkedSessionsWith(t, func(int) int { return 100 }, "x")
	_ = e2
	c2.mu.Lock()
	for _, l := range c2.links {
		l.raw = streamWire{&bigWire{&preferredWire{}}}
	}
	c2.ready = true
	for _, l := range c2.links {
		l.started, l.lastHeard = true, time.Now()
		l.raw = streamWire{&bigWire{&preferredWire{}}}
	}
	c2.mu.Unlock()
	if !c2.OnReliableStream() {
		t.Fatal("a live direct stream was not reported")
	}
}
