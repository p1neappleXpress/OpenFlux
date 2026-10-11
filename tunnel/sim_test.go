package tunnel

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// A Telemost-like carrier in memory, to run the real TCP of the tunnel over
// the real striped link without the SFU: every lane sends on a 120 Hz clock,
// at most perTick packets a tick, loses packets the way the SFU does (short
// outages in multiples of 40 ms plus a little random loss) and delivers the
// rest in order after a one-way delay.
//
// SIM=1 go test ./tunnel -run TestSimTCP -v
// knobs: SIM_LANES, SIM_SECS, SIM_OUTAGES (per second per lane), SIM_LOSS
// (random loss %), SIM_DELAY_MS (one way), SIM_PER_TICK.

type simLink struct {
	outagesPerSec float64
	randomLoss    float64
	delay         time.Duration
	perTick       int
	allowance     float64       // packets/s the "SFU" forwards per lane, 0 = all
	ramp          time.Duration // the allowance grows from a fifth to full over this
	start         time.Time
}

type simLane struct {
	link *simLink
	peer *simLane
	rnd  *rand.Rand

	mu       sync.Mutex
	cb       func([]byte)
	src      func(max int) [][]byte
	own      [][]byte
	outUntil time.Time
	bucket   float64
	bucketAt time.Time

	wire       chan simPkt
	done       chan struct{}
	sent, lost atomic.Uint64
}

type simPkt struct {
	at time.Time
	b  []byte
}

func newSimLanePair(link *simLink, seed int64) (*simLane, *simLane) {
	a := &simLane{link: link, rnd: rand.New(rand.NewSource(seed)), wire: make(chan simPkt, 1<<16), done: make(chan struct{})}
	b := &simLane{link: link, rnd: rand.New(rand.NewSource(seed + 1000)), wire: make(chan simPkt, 1<<16), done: make(chan struct{})}
	a.peer, b.peer = b, a
	return a, b
}

func (l *simLane) Start() error {
	go l.clock()
	go l.deliverLoop()
	return nil
}

func (l *simLane) Stop() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *simLane) Send(b []byte) error {
	l.mu.Lock()
	l.own = append(l.own, append([]byte(nil), b...))
	l.mu.Unlock()
	return nil
}

func (l *simLane) Receive(cb func([]byte)) { l.mu.Lock(); l.cb = cb; l.mu.Unlock() }
func (l *simLane) IsConnected() bool       { return true }
func (l *simLane) Stats() transport.TransportStats {
	return transport.TransportStats{Connected: true}
}
func (l *simLane) SetFrameSource(src func(max int) [][]byte) { l.mu.Lock(); l.src = src; l.mu.Unlock() }

func (l *simLane) clock() {
	t := time.NewTicker(time.Second / 120)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			l.mu.Lock()
			frames := l.own
			l.own = nil
			src := l.src
			l.mu.Unlock()
			if src != nil && len(frames) < l.link.perTick {
				frames = append(frames, src(l.link.perTick-len(frames))...)
			}
			for _, f := range frames {
				l.sent.Add(1)
				if l.dropped(now) {
					l.lost.Add(1)
					continue
				}
				l.wire <- simPkt{at: now.Add(l.link.delay), b: f}
			}
		}
	}
}

// dropped: over the allowance (a token bucket), or an outage that starts at
// random (Poisson) and lasts 40, 80 or 120 ms, or random loss.
func (l *simLane) dropped(now time.Time) bool {
	if a := l.link.allowance; a > 0 {
		if el := now.Sub(l.link.start); el < l.link.ramp {
			a *= 0.2 + 0.8*float64(el)/float64(l.link.ramp)
		}
		if !l.bucketAt.IsZero() {
			l.bucket += a * now.Sub(l.bucketAt).Seconds()
		}
		l.bucketAt = now
		if l.bucket > a*0.03 {
			l.bucket = a * 0.03
		}
		if l.bucket < 1 {
			return true
		}
		l.bucket--
	}
	if now.Before(l.outUntil) {
		return true
	}
	if l.rnd.Float64() < l.link.outagesPerSec/1440 {
		l.outUntil = now.Add(time.Duration(40*(1+l.rnd.Intn(3))) * time.Millisecond)
		return true
	}
	return l.rnd.Float64() < l.link.randomLoss
}

func (l *simLane) deliverLoop() {
	for {
		select {
		case <-l.done:
			return
		case p := <-l.wire:
			if d := time.Until(p.at); d > 0 {
				time.Sleep(d)
			}
			l.peer.mu.Lock()
			cb := l.peer.cb
			l.peer.mu.Unlock()
			if cb != nil {
				cb(p.b)
			}
		}
	}
}

func envF(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return v
	}
	return def
}

// simServerStack is the far end: a netstack at 10.10.10.1 on the tunnel.
func simServerStack(t *testing.T, trans transport.Transport) *stack.Stack {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcpProtocol(), udp.NewProtocol},
	})
	TuneTCP(s)
	ep := NewTunnelLinkEndpoint()
	ep.onOutgoingPacket = func(b []byte) { _ = trans.Send(b) }
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{10, 10, 10, 1}), PrefixLen: 24},
	}, stack.AddressProperties{})
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})
	trans.Receive(func(b []byte) { ep.InjectInbound(b) })
	return s
}

func TestSimTCP(t *testing.T) {
	if os.Getenv("SIM") == "" {
		t.Skip("SIM=1 runs the carrier simulation")
	}
	link := &simLink{
		outagesPerSec: envF("SIM_OUTAGES", 2),
		randomLoss:    envF("SIM_LOSS", 2) / 100,
		delay:         time.Duration(envF("SIM_DELAY_MS", 45)) * time.Millisecond,
		perTick:       int(envF("SIM_PER_TICK", 12)),
		allowance:     envF("SIM_ALLOW", 0),
		ramp:          time.Duration(envF("SIM_RAMP_S", 0) * float64(time.Second)),
		start:         time.Now(),
	}
	lanes := int(envF("SIM_LANES", 1))
	secs := int(envF("SIM_SECS", 20))
	var aL, bL []transport.Transport
	var all []*simLane
	for i := 0; i < lanes; i++ {
		a, b := newSimLanePair(link, int64(i+1))
		aL, bL = append(aL, a), append(bL, b)
		all = append(all, a, b)
	}
	sa, sb := transport.NewStripedTransport(aL), transport.NewStripedTransport(bL)
	ca := transport.NewCodecTransport(sa, transport.CodecBatched, true)
	cb := transport.NewCodecTransport(sb, transport.CodecBatched, false)
	client := NewTCPTunnelMode(ca, false, ExitModeL4)
	server := simServerStack(t, cb)
	if err := ca.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cb.Start(); err != nil {
		t.Fatal(err)
	}
	defer ca.Stop()
	defer cb.Stop()

	ln, err := gonet.ListenTCP(server, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 10, 10, 1}), Port: 5201}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		rand.Read(buf)
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	}()

	var conn io.ReadCloser
	for i := 0; i < 50; i++ { // the codec may need a moment to settle
		c, err := client.DialTCP("10.10.10.1:5201")
		if err == nil {
			conn = c
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.Close()

	var got atomic.Int64
	go func() {
		buf := make([]byte, 256<<10)
		for {
			n, err := conn.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	start := time.Now()
	var last, lateHalf int64
	var series string
	for i := 1; i <= secs; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i) * time.Second)))
		g := got.Load()
		if os.Getenv("SIM_INFO") != "" {
			for _, te := range server.RegisteredEndpoints() {
				if ep, ok := te.(tcpip.Endpoint); ok {
					var info tcpip.TCPInfoOption
					if ep.GetSockOpt(&info) == nil && info.SndCwnd > 0 {
						t.Logf("t=%ds %dKB/s cwnd=%d ssthresh=%d rtt=%s rttvar=%s rto=%s cc=%d reorder=%v",
							i, (g-last)/1000, info.SndCwnd, info.SndSsthresh, info.RTT.Round(time.Millisecond),
							info.RTTVar.Round(time.Millisecond), info.RTO.Round(time.Millisecond), info.CcState, info.ReorderSeen)
					}
				}
			}
		}
		series += fmt.Sprintf(" %d", (g-last)/1000)
		if i > secs/2 {
			lateHalf += g - last
		}
		last = g
	}
	total := got.Load()
	var sent, lost uint64
	for _, l := range all {
		sent += l.sent.Load()
		lost += l.lost.Load()
	}
	ts := server.Stats().TCP
	t.Logf("lanes=%d loss: carrier %.1f%% (outages %.1f/s/lane, random %.1f%%, one-way %s, allowance %.0f/s ramp %s)",
		lanes, 100*float64(lost)/float64(max(sent, 1)), link.outagesPerSec, link.randomLoss*100, link.delay, link.allowance, link.ramp)
	t.Logf("KB/s per second:%s", series)
	t.Logf("avg %.0f KB/s, second half %.0f KB/s", float64(total)/1000/float64(secs), float64(lateHalf)/1000/float64(secs-secs/2))
	t.Logf("server TCP: segs=%d retrans=%d fastRetx=%d timeouts=%d sackRec=%d tlpRec=%d spuriousRTO=%d dsack=%d",
		ts.SegmentsSent.Value(), ts.Retransmits.Value(), ts.FastRetransmit.Value(), ts.Timeouts.Value(),
		ts.SACKRecovery.Value(), ts.TLPRecovery.Value(), ts.SpuriousRTORecovery.Value(), ts.SegmentsAckedWithDSACK.Value())
}
