package transport

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// Compatibility between the layerings, codecs and KDF contexts that peers
// of different builds use. "old" stacks are built exactly as the core did
// before (Encrypted(Batched|Compressed(carrier)) with a fixed context);
// "new" ones as it does now.

// asyncWire is one end of an in-memory carrier that delivers in order on
// its own goroutine, like a real one (no re-entrant Send -> Receive).
type asyncWire struct {
	mu   sync.Mutex
	cb   func([]byte)
	out  chan []byte
	down atomic.Bool
}

func asyncPair() (*asyncWire, *asyncWire) {
	a := &asyncWire{out: make(chan []byte, 4096)}
	b := &asyncWire{out: make(chan []byte, 4096)}
	pump := func(from, to *asyncWire) {
		for p := range from.out {
			to.mu.Lock()
			cb := to.cb
			to.mu.Unlock()
			if cb != nil {
				cb(p)
			}
		}
	}
	go pump(a, b)
	go pump(b, a)
	return a, b
}

func (w *asyncWire) Start() error          { return nil }
func (w *asyncWire) Stop() error           { return nil }
func (w *asyncWire) IsConnected() bool     { return true }
func (w *asyncWire) Stats() TransportStats { return TransportStats{} }
func (w *asyncWire) Receive(cb func([]byte)) {
	w.mu.Lock()
	w.cb = cb
	w.mu.Unlock()
}
func (w *asyncWire) Send(p []byte) error {
	if !w.down.Load() {
		w.out <- append([]byte(nil), p...)
	}
	return nil
}

type peer interface {
	Start() error
	Stop() error
	Send([]byte) error
	Receive(func([]byte))
}

const compatSecret = "compat test secret 0123456789"

func oldClassic(t *testing.T, w Transport, codec, context string, exit bool) peer {
	t.Helper()
	var inner Transport = w
	if codec == CodecLegacy {
		inner = NewCompressedTransport(inner)
	} else {
		inner = NewBatchedTransport(inner)
	}
	enc, err := NewEncryptedTransport(inner, compatSecret, context, exit)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func newClassic(t *testing.T, w Transport, codec, context string, alternates []string, exit bool) peer {
	t.Helper()
	enc, err := NewEncryptedTransport(NewCodecTransport(w, codec, !exit), compatSecret, context, exit)
	if err != nil {
		t.Fatal(err)
	}
	enc.SetAlternateContexts(alternates)
	return enc
}

func compatSession(t *testing.T, w Transport, context string, alternates []string, exit, classic bool, codec string) *Session {
	t.Helper()
	s, err := NewSession(PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP | control.CapabilityUDP,
		MaxPacketSize: MaxNegotiatedPacket,
	}, exit)
	if err != nil {
		t.Fatal(err)
	}
	if classic {
		s.SetClassic(codec)
	}
	s.SetAlternateContexts(alternates)
	if err := s.AddTransport("carrier", w, compatSecret, context, 100); err != nil {
		t.Fatal(err)
	}
	return s
}

// roundTrip starts both ends, has the client send an IPv4 packet every
// 50ms and the exit echo each one, and reports how long the first echo
// took (or fails after limit).
func roundTrip(t *testing.T, client, exit peer, limit time.Duration, while ...func()) time.Duration {
	t.Helper()
	exit.Receive(func(p []byte) { _ = exit.Send(p) })
	got := make(chan struct{}, 1)
	client.Receive(func(p []byte) {
		if len(p) >= 20 && p[0]>>4 == 4 {
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})
	if err := exit.Start(); err != nil {
		t.Fatalf("exit start: %v", err)
	}
	began := time.Now()
	if err := client.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer client.Stop()
	defer exit.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(limit)
	for {
		_ = client.Send(testIPv4(60, 6))
		select {
		case <-got:
			d := time.Since(began)
			for _, f := range while {
				f()
			}
			return d
		case <-deadline:
			t.Fatalf("no echo within %v", limit)
			return 0
		case <-tick.C:
		}
	}
}

func TestCodecFallbackAgainstOldPeers(t *testing.T) {
	cases := []struct {
		name         string
		client, exit func(*asyncWire, *asyncWire) (peer, peer)
	}{
		{"new batched client -> old legacy exit", func(a, b *asyncWire) (peer, peer) {
			return newClassic(t, a, CodecBatched, "ctx", nil, false), oldClassic(t, b, CodecLegacy, "ctx", true)
		}, nil},
		{"new legacy client -> old batched exit", func(a, b *asyncWire) (peer, peer) {
			return newClassic(t, a, CodecLegacy, "ctx", nil, false), oldClassic(t, b, CodecBatched, "ctx", true)
		}, nil},
		{"old legacy client -> new batched exit", func(a, b *asyncWire) (peer, peer) {
			return oldClassic(t, a, CodecLegacy, "ctx", false), newClassic(t, b, CodecBatched, "ctx", nil, true)
		}, nil},
		{"old batched client -> new legacy exit", func(a, b *asyncWire) (peer, peer) {
			return oldClassic(t, a, CodecBatched, "ctx", false), newClassic(t, b, CodecLegacy, "ctx", nil, true)
		}, nil},
		{"new legacy client -> new batched exit", func(a, b *asyncWire) (peer, peer) {
			return newClassic(t, a, CodecLegacy, "ctx", nil, false), newClassic(t, b, CodecBatched, "ctx", nil, true)
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := asyncPair()
			cl, ex := c.client(a, b)
			d := roundTrip(t, cl, ex, 8*time.Second)
			t.Logf("first echo after %v", d.Round(10*time.Millisecond))
		})
	}
}

// An iOS-fork peer's empty-batch probe makes the codec send batched.
func TestCodecFollowsBatchProbe(t *testing.T) {
	w := &fakeTransport{}
	c := NewCodecTransport(w, CodecLegacy, false)
	c.Receive(func([]byte) {})
	deliver := func(p []byte) {
		w.mu.Lock()
		cb := w.cb
		w.mu.Unlock()
		cb(p)
	}
	deliver([]byte{0x00}) // a Volga carrier keepalive: ignored
	if c.heard.Load() {
		t.Fatal("a lone 0x00 counted as the peer's codec")
	}
	deliver(encodeBatch(nil))
	if c.Current() != CodecBatched {
		t.Fatalf("after a batch probe the codec sends %s", c.Current())
	}
}

func TestContextFallback(t *testing.T) {
	t.Run("new client rotates to an old exit's context", func(t *testing.T) {
		a, b := asyncPair()
		cl := newClassic(t, a, CodecBatched, "https://docs/d", []string{ContextPlaceholder, "rooms"}, false)
		ex := oldClassic(t, b, CodecBatched, "rooms", true)
		d := roundTrip(t, cl, ex, 15*time.Second)
		t.Logf("first echo after %v", d.Round(10*time.Millisecond))
	})
	t.Run("new exit answers an old client under its context", func(t *testing.T) {
		a, b := asyncPair()
		cl := oldClassic(t, a, CodecBatched, "rooms", false)
		ex := newClassic(t, b, CodecBatched, ContextPlaceholder, []string{"rooms"}, true)
		d := roundTrip(t, cl, ex, 5*time.Second)
		if d > 2*time.Second {
			t.Fatalf("exit took %v to answer under the client's context", d)
		}
	})
	t.Run("Session over mismatched contexts", func(t *testing.T) {
		a, b := asyncPair()
		cl := compatSession(t, a, "https://docs/d", []string{ContextPlaceholder}, false, false, "")
		ex := compatSession(t, b, ContextPlaceholder, []string{"https://docs/d"}, true, false, "")
		roundTrip(t, cl, ex, 5*time.Second)
	})
}

func TestSessionClassicCompat(t *testing.T) {
	t.Run("new client falls back to an old classic exit", func(t *testing.T) {
		a, b := asyncPair()
		cl := compatSession(t, a, "ctx", nil, false, true, CodecBatched)
		ex := oldClassic(t, b, CodecLegacy, "ctx", true)
		d := roundTrip(t, cl, ex, 12*time.Second, func() {
			if m := cl.Mode(); m != "classic" {
				t.Errorf("client mode %q", m)
			}
		})
		t.Logf("first echo after %v (classic)", d.Round(10*time.Millisecond))
	})
	t.Run("old classic client served by a classic-configured exit", func(t *testing.T) {
		a, b := asyncPair()
		cl := oldClassic(t, a, CodecBatched, "ctx", false)
		ex := compatSession(t, b, "ctx", nil, true, true, CodecBatched)
		roundTrip(t, cl, ex, 5*time.Second, func() {
			if m := ex.Mode(); m != "classic" {
				t.Errorf("exit mode %q", m)
			}
		})
	})
	t.Run("new classic profile upgrades to a Session", func(t *testing.T) {
		a, b := asyncPair()
		cl := compatSession(t, a, "ctx", nil, false, true, CodecLegacy)
		ex := compatSession(t, b, "ctx", nil, true, true, CodecBatched)
		roundTrip(t, cl, ex, 5*time.Second, func() {
			deadline := time.Now().Add(3 * time.Second)
			for cl.Mode() != "session" && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if m := cl.Mode(); m != "session" {
				t.Errorf("client mode %q", m)
			}
		})
	})
	t.Run("old Session client with a classic-configured exit", func(t *testing.T) {
		a, b := asyncPair()
		cl := compatSession(t, a, "ctx", nil, false, false, "")
		ex := compatSession(t, b, "ctx", nil, true, true, CodecBatched)
		roundTrip(t, cl, ex, 5*time.Second)
	})
	t.Run("a Session-only exit does not serve classic clients", func(t *testing.T) {
		a, b := asyncPair()
		cl := oldClassic(t, a, CodecBatched, "ctx", false)
		ex := compatSession(t, b, "ctx", nil, true, false, "")
		var got atomic.Int64
		ex.Receive(func([]byte) { got.Add(1) })
		_ = ex.Start()
		_ = cl.Start()
		defer cl.Stop()
		defer ex.Stop()
		for i := 0; i < 20; i++ {
			_ = cl.Send(testIPv4(60, 6))
			time.Sleep(20 * time.Millisecond)
		}
		if got.Load() != 0 {
			t.Fatalf("strict exit delivered %d classic packets", got.Load())
		}
	})
}

// A Session exit prefers its Session client over a classic one: classic
// frames on the carrier (a replayed capture, another device) must not
// take the replies away from it.
func TestSessionClassicYieldsToSession(t *testing.T) {
	a, b := asyncPair()
	cl := compatSession(t, a, "ctx", nil, false, false, "")
	ex := compatSession(t, b, "ctx", nil, true, true, CodecBatched)
	roundTrip(t, cl, ex, 5*time.Second, func() {
		// A classic frame arrives on the exit's carrier.
		intruder := oldClassic(t, &asyncWire{out: a.out}, CodecBatched, "ctx", false)
		_ = intruder.Start()
		_ = intruder.Send(testIPv4(60, 6))
		time.Sleep(200 * time.Millisecond)
		if m := ex.Mode(); m != "session" {
			t.Errorf("exit switched to %q while its Session client was active", m)
		}
	})
}

func TestKDFContextsRule(t *testing.T) {
	cases := []struct {
		name, explicit, url string
		specs               []ContextSource
		want                string
	}{
		{"explicit", "x", "https://u", nil, "x"},
		{"--url", "", "https://u", []ContextSource{{Type: "mailru", URL: "https://m"}}, "https://u"},
		{"highest priority", "", ContextPlaceholder, []ContextSource{
			{Type: "mailru", URL: "https://m", Priority: 10}, {Type: "vyandex", URL: "https://v", Priority: 100}}, "https://v"},
		{"classic cups rooms as --url", "", "rooms", []ContextSource{{Type: "cupsonline", URL: "rooms"}}, ContextPlaceholder},
		{"direct address", "", ContextPlaceholder, []ContextSource{{Type: "direct", URL: "1.2.3.4:9443"}}, ContextPlaceholder},
	}
	for _, c := range cases {
		got, alts := KDFContexts(c.explicit, c.url, c.specs)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		for _, a := range alts {
			if a == got {
				t.Errorf("%s: primary repeated among alternates", c.name)
			}
		}
	}
	_, alts := KDFContexts("", "rooms", []ContextSource{{Type: "cupsonline", URL: "rooms"}})
	if !bytes.Contains([]byte(joinStrings(alts)), []byte("rooms")) {
		t.Errorf("old classic cups context missing from alternates %q", alts)
	}
}

func joinStrings(s []string) string {
	var b bytes.Buffer
	for _, x := range s {
		b.WriteString(x)
		b.WriteByte('|')
	}
	return b.String()
}
