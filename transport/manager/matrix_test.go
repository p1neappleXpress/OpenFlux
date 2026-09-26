package manager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"openflux/transport"
	"openflux/transport/control"
)

// The matrix tests run a client and an exit Manager, as main.go wires them
// for --negotiate, over every combination of carriers named after the real
// transport types. Carriers are in-process fakes: what is under test is the
// Session and the Manager on top of them, not the services behind them.

var matrixTypes = []struct {
	name     string
	priority int
	cookies  bool // the real transport carries cookies and can hit a check
}{
	{"direct", 100, false},
	{"yandex", 90, true},
	{"vyandex", 80, true},
	{"boards", 70, true},
	{"mailru", 60, true},
	{"cupsonline", 50, true},
	{"oneme", 40, false},
}

var errCaptcha = errors.New("captcha required")

// wire is one fake carrier shared by a client and an exit, like a document
// both attach to. Its exit side can be stuck behind a check until it is
// given a "spravka" cookie, the way a Yandex transport on a flagged IP is.
type wire struct {
	name string

	mu sync.Mutex
	// stuck: the exit side cannot use the carrier.
	stuck bool
	// blackhole: while stuck, the exit side still claims to be connected
	// and accepts sends that go nowhere (a document attached, relay refused).
	blackhole bool
	// cut: the carrier is gone for both sides.
	cut bool

	client, exit *wireEnd
}

type wireEnd struct {
	w      *wire
	isExit bool

	mu      sync.Mutex
	cb      func([]byte)
	started bool
	jar     map[string]string
	notify  func(err error, name, url, reason string)
	done    chan struct{}
}

func newWire(name string) *wire {
	w := &wire{name: name}
	w.client = &wireEnd{w: w}
	w.exit = &wireEnd{w: w, isExit: true}
	return w
}

// blocked reports whether this end cannot currently use the carrier.
// Caller holds w.mu.
func (e *wireEnd) blockedLocked() bool {
	return e.w.cut || (e.isExit && e.w.stuck)
}

func (e *wireEnd) Start() error {
	e.w.mu.Lock()
	down := e.isExit && e.w.stuck && !e.w.blackhole
	e.w.mu.Unlock()
	if down {
		e.report()
		return errCaptcha
	}
	e.mu.Lock()
	if !e.started {
		e.started = true
		e.done = make(chan struct{})
		if e.isExit {
			go e.reportWhileStuck(e.done)
		}
	}
	e.mu.Unlock()
	return nil
}

// reportWhileStuck re-reports the check while the carrier is up but
// stuck, as the real transports do on every failed request.
func (e *wireEnd) reportWhileStuck(done chan struct{}) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		e.w.mu.Lock()
		stuck := e.w.stuck
		e.w.mu.Unlock()
		if stuck {
			e.report()
		}
	}
}

func (e *wireEnd) report() {
	e.mu.Lock()
	n := e.notify
	e.mu.Unlock()
	if n != nil {
		n(errCaptcha, e.w.name, "https://"+e.w.name+".example/check", "smartcaptcha")
	}
}

func (e *wireEnd) Stop() error {
	e.mu.Lock()
	if e.started {
		e.started = false
		close(e.done)
	}
	e.mu.Unlock()
	return nil
}

func (e *wireEnd) IsConnected() bool {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	if e.w.cut {
		return false
	}
	if e.isExit && e.w.stuck && !e.w.blackhole {
		return false
	}
	return started
}

func (e *wireEnd) Send(b []byte) error {
	e.w.mu.Lock()
	cut := e.w.cut
	selfBlocked := e.blockedLocked()
	blackhole := e.w.blackhole
	peer := e.w.client
	if !e.isExit {
		peer = e.w.exit
	}
	peerBlocked := peer.blockedLocked()
	e.w.mu.Unlock()
	if cut {
		return errors.New("carrier cut")
	}
	if selfBlocked {
		if blackhole {
			return nil
		}
		return errCaptcha
	}
	if peerBlocked {
		return nil // the other side is stuck: the message goes nowhere
	}
	peer.mu.Lock()
	cb, started := peer.cb, peer.started
	peer.mu.Unlock()
	if cb != nil && started {
		cb(append([]byte(nil), b...))
	}
	return nil
}

func (e *wireEnd) Receive(cb func([]byte)) { e.mu.Lock(); e.cb = cb; e.mu.Unlock() }
func (e *wireEnd) Stats() transport.TransportStats {
	return transport.TransportStats{Connected: e.IsConnected()}
}

func (e *wireEnd) SetErrorNotifier(f func(err error, name, url, reason string)) {
	e.mu.Lock()
	e.notify = f
	e.mu.Unlock()
}

func (e *wireEnd) FetchCookies() (map[string]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.jar))
	for k, v := range e.jar {
		out[k] = v
	}
	return out, nil
}

func (e *wireEnd) ApplyCookies(jar map[string]string) error {
	e.mu.Lock()
	e.jar = jar
	e.mu.Unlock()
	if jar["spravka"] != "" {
		e.w.mu.Lock()
		e.w.stuck = false
		e.w.mu.Unlock()
	}
	return nil
}

// pair is a client and an exit Manager over a set of wires.
type pair struct {
	client, exit *Manager
	wires        map[string]*wire
	got          [2]chan string // payload markers received: [0] client, [1] exit
	asked        chan [3]string // AuthRequired reaching the client
}

type pairOpts struct {
	types []int // indexes into matrixTypes
	// stuck names a carrier whose exit side starts stuck; blackhole makes
	// it claim to be connected meanwhile.
	stuck     string
	blackhole bool
}

func startPair(t *testing.T, o pairOpts) *pair {
	t.Helper()
	const secret, ctx = "matrix-secret-long-enough", "http://#"
	params := transport.PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP | control.CapabilityUDP,
		MaxPacketSize: 1500,
	}
	p := &pair{
		wires: make(map[string]*wire),
		got:   [2]chan string{make(chan string, 64), make(chan string, 64)},
		asked: make(chan [3]string, 16),
	}
	for _, i := range o.types {
		w := newWire(matrixTypes[i].name)
		if w.name == o.stuck {
			w.stuck, w.blackhole = true, o.blackhole
		}
		p.wires[w.name] = w
	}
	for side := 0; side < 2; side++ {
		exit := side == 1
		sess, err := transport.NewSession(params, exit)
		if err != nil {
			t.Fatal(err)
		}
		m := New(sess, nil, secret, ctx)
		for _, i := range o.types {
			ty := matrixTypes[i]
			end := p.wires[ty.name].client
			if exit {
				end = p.wires[ty.name].exit
			}
			var prov CookieProvider
			if ty.cookies {
				prov = end
			}
			if err := sess.AddTransport(ty.name, end, secret, ctx, ty.priority); err != nil {
				t.Fatal(err)
			}
			if err := m.Add(ty.name, ty.name, end, ty.priority, prov); err != nil {
				t.Fatal(err)
			}
		}
		sess.SetControlHandler(m.DispatchControl)
		got := p.got[side]
		m.Receive(func(pkt []byte) {
			select {
			case got <- string(pkt[28:]):
			default:
			}
		})
		t.Cleanup(func() { _ = m.Stop() })
		if exit {
			p.exit = m
		} else {
			p.client = m
			m.SetRemoteAuthNotifier(func(name, url, reason string) {
				p.asked <- [3]string{name, url, reason}
			})
		}
	}
	if err := p.exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Start(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	waitUntil(t, 10*time.Second, "both sides connected", func() bool {
		return p.client.IsConnected() && p.exit.IsConnected()
	})
	return p
}

// udp builds an IPv4/UDP packet carrying marker.
func udp(marker string) []byte {
	pkt := make([]byte, 28+len(marker))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt)))
	pkt[8], pkt[9] = 64, 17
	copy(pkt[12:], []byte{10, 10, 10, 2})
	copy(pkt[16:], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(pkt[20:], 53000)
	binary.BigEndian.PutUint16(pkt[22:], 53)
	binary.BigEndian.PutUint16(pkt[24:], uint16(8+len(marker)))
	copy(pkt[28:], marker)
	return pkt
}

// roundTrip sends a marker each way, retrying every 200ms, and returns how
// long it took until both arrived.
func (p *pair) roundTrip(t *testing.T, within time.Duration, tag string) time.Duration {
	t.Helper()
	start := time.Now()
	for dir, from := range []*Manager{p.client, p.exit} {
		marker := fmt.Sprintf("%s-%d", tag, dir)
		inbox := p.got[1-dir]
		deadline := time.After(within)
		tick := time.NewTicker(200 * time.Millisecond)
	wait:
		for {
			_ = from.Send(udp(marker))
			for {
				select {
				case m := <-inbox:
					if m == marker {
						break wait
					}
					continue
				case <-tick.C:
				case <-deadline:
					tick.Stop()
					t.Fatalf("%s: %s did not reach the %s within %v (client active=%q, exit active=%q)",
						tag, marker, []string{"exit", "client"}[dir], within,
						p.client.Session().ActiveTransport(), p.exit.Session().ActiveTransport())
				}
				break
			}
		}
		tick.Stop()
	}
	return time.Since(start)
}

func (w *wire) set(f func(w *wire)) { w.mu.Lock(); f(w); w.mu.Unlock() }

func waitUntil(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func subsetName(types []int) string {
	s := ""
	for i, ix := range types {
		if i > 0 {
			s += "+"
		}
		s += matrixTypes[ix].name
	}
	return s
}

// Every non-empty combination of carriers negotiates, carries data both
// ways, and keeps carrying it through each carrier in turn as the ones
// above it are cut.
func TestMatrixEveryCombination(t *testing.T) {
	n := len(matrixTypes)
	for mask := 1; mask < 1<<n; mask++ {
		var types []int
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				types = append(types, i)
			}
		}
		t.Run(subsetName(types), func(t *testing.T) {
			t.Parallel()
			p := startPair(t, pairOpts{types: types})
			p.roundTrip(t, 3*time.Second, "up")
			for k, ix := range types[:len(types)-1] {
				p.wires[matrixTypes[ix].name].set(func(w *wire) { w.cut = true })
				next := matrixTypes[types[k+1]].name
				p.roundTrip(t, 3*time.Second, "after-cut-"+matrixTypes[ix].name)
				if a := p.client.Session().ActiveTransport(); a != next {
					t.Fatalf("client routes via %q after cutting %s, want %q", a, matrixTypes[ix].name, next)
				}
			}
		})
	}
}

// A carrier stuck on a check at the exit is brought up through any other
// carrier: the exit's AuthRequired travels over it, the client's cookies
// come back over it, and then the stuck carrier carries data on its own.
// Every ordered pair, the stuck one above and below the working one, with
// the stuck side refusing to start or claiming to be connected.
func TestMatrixAuthRelay(t *testing.T) {
	for si, st := range matrixTypes {
		if !st.cookies {
			continue
		}
		for ri := range matrixTypes {
			if ri == si {
				continue
			}
			for _, blackhole := range []bool{false, true} {
				mode := "down"
				if blackhole {
					mode = "blackhole"
				}
				types := []int{si, ri}
				if ri < si {
					types = []int{ri, si}
				}
				name := fmt.Sprintf("%s-stuck/via-%s/%s", st.name, matrixTypes[ri].name, mode)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					relay := matrixTypes[ri].name
					start := time.Now()
					p := startPair(t, pairOpts{types: types, stuck: st.name, blackhole: blackhole})

					// Data must flow over the working carrier right away,
					// even while the stuck one is ranked above it.
					took := p.roundTrip(t, 3*time.Second, "before")
					t.Logf("data flowing %v after connect", took.Round(time.Millisecond))

					var asked [3]string
					select {
					case asked = <-p.asked:
					case <-time.After(45 * time.Second):
						t.Fatalf("AuthRequired for %s never reached the client", st.name)
					}
					t.Logf("AuthRequired reached the client %v after start", time.Since(start).Round(time.Millisecond))
					if asked[0] != st.name {
						t.Fatalf("client asked about %q, want %q", asked[0], st.name)
					}

					if err := p.client.OfferCookies(st.name, map[string]string{"spravka": "passed"}); err != nil {
						t.Fatalf("offer cookies: %v", err)
					}
					waitUntil(t, 10*time.Second, "cookies on the exit's "+st.name, func() bool {
						jar, _ := p.exit.FetchCookiesFor(st.name)
						return jar["spravka"] == "passed"
					})

					// Now the relay goes away and the unstuck carrier has
					// to carry everything by itself.
					p.wires[relay].set(func(w *wire) { w.cut = true })
					took = p.roundTrip(t, 45*time.Second, "unstuck")
					t.Logf("data over the unstuck %s %v after the relay was cut", st.name, took.Round(time.Millisecond))
				})
			}
		}
	}
}

// A carrier that hits a check in the middle of a working session, the way
// Yandex does after a while: the exit's report must get through the other
// carrier at once and data must move off the stuck one on both sides,
// without waiting for its keepalives to time out.
func TestMatrixStuckMidSession(t *testing.T) {
	for si, st := range matrixTypes {
		if !st.cookies {
			continue
		}
		for ri := si + 1; ri < len(matrixTypes); ri++ {
			relay := matrixTypes[ri].name
			t.Run(fmt.Sprintf("%s-stuck/via-%s", st.name, relay), func(t *testing.T) {
				t.Parallel()
				p := startPair(t, pairOpts{types: []int{si, ri}})
				p.roundTrip(t, 3*time.Second, "healthy")
				if a := p.client.Session().ActiveTransport(); a != st.name {
					t.Fatalf("healthy session routes via %q, want %q", a, st.name)
				}

				stuckAt := time.Now()
				p.wires[st.name].set(func(w *wire) { w.stuck, w.blackhole = true, true })
				select {
				case asked := <-p.asked:
					if asked[0] != st.name {
						t.Fatalf("client asked about %q, want %q", asked[0], st.name)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("AuthRequired for %s did not reach the client", st.name)
				}
				t.Logf("AuthRequired reached the client %v after the check", time.Since(stuckAt).Round(time.Millisecond))
				took := p.roundTrip(t, 3*time.Second, "stuck")
				t.Logf("data around the stuck carrier %v later", took.Round(time.Millisecond))

				if err := p.client.OfferCookies(st.name, map[string]string{"spravka": "passed"}); err != nil {
					t.Fatal(err)
				}
				waitUntil(t, 5*time.Second, "cookies on the exit", func() bool {
					jar, _ := p.exit.FetchCookiesFor(st.name)
					return jar["spravka"] == "passed"
				})
				p.wires[relay].set(func(w *wire) { w.cut = true })
				took = p.roundTrip(t, 5*time.Second, "unstuck")
				t.Logf("data over the unstuck %s %v after the relay was cut", st.name, took.Round(time.Millisecond))
			})
		}
	}
}
