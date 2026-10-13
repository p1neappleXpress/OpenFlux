package transport

import (
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// Bond scheduler constants (see bondScheduler).
const (
	bondRateStart  = 256 * 1024 // bytes/s a link is assumed to carry until measured
	bondRateFloor  = 16 * 1024  // a measured link never goes under this
	bondWindow     = 250 * time.Millisecond
	bondRateWins   = 8 // the rate is the best of this many windows (2 s)
	bondMinRTTAge  = 10 * time.Second
	bondLostAfter  = 2 * time.Second        // at most
	bondLostMin    = time.Second            // at least
	bondRTTInitial = 200 * time.Millisecond // until acks tell
)

// BondLinkStats is what the scheduler knows about one carrier.
type BondLinkStats struct {
	RateBps       float64       // bytes per second it delivers when busy
	InflightBytes int           // sent, not yet acknowledged
	RTT           time.Duration // send to ack, EWMA
	MinRTT        time.Duration // lowest send to ack lately: the carrier's own delay
}

type bondLink struct {
	BondLinkStats
	winStart time.Time
	winBytes int
	winBusy  bool // the link had data in flight during the window
	// wins: delivered bytes/s of the last busy windows. The rate is their
	// maximum: a window in which little was sent says the link was idle,
	// not slow.
	wins   [bondRateWins]float64
	winIdx int
	minAt  time.Time
	// lost: packets on it went unacknowledged. It gets one packet at a
	// time, a probe, until an ack shows it delivers again.
	lost bool
}

type bondSent struct {
	link string
	size int
	at   time.Time
}

// bondScheduler splits a bonded Session's packets over its carriers so
// that each packet arrives as early as it can, which also keeps them close
// to their order: TCP inside the tunnel takes packets that overtake each
// other for losses and slows down.
//
// A packet goes to the carrier where it is due first: half the carrier's
// own round trip (MinRTT) plus the time to deliver what is queued on it
// (in flight / rate). At low load that is always the quickest carrier; the
// others join only once its queue makes them quicker, which is when their
// speeds add up.
//
// Rates come from the peer's acks (carriers queue internally, so the
// sending side cannot see their speed): every packet is remembered with
// its carrier until acked. A carrier that stops delivering keeps its bytes
// in flight and so stops getting new ones; after a few round trips they
// count as lost and it gets single probe packets until one is acked.
type bondScheduler struct {
	mu     sync.Mutex
	links  map[string]*bondLink
	flight map[uint64]bondSent
}

func newBondScheduler() *bondScheduler {
	return &bondScheduler{links: make(map[string]*bondLink), flight: make(map[uint64]bondSent)}
}

func (b *bondScheduler) linkLocked(name string, now time.Time) *bondLink {
	l := b.links[name]
	if l == nil {
		l = &bondLink{BondLinkStats: BondLinkStats{RateBps: bondRateStart, RTT: bondRTTInitial, MinRTT: bondRTTInitial}, winStart: now}
		b.links[name] = l
	}
	return l
}

// pick returns the carrier, of links (in priority order), to send a packet
// of size bytes on.
func (b *bondScheduler) pick(links []string, size int, now time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	best, bestAt := "", 0.0
	for _, name := range links {
		l := b.linkLocked(name, now)
		if l.lost && l.InflightBytes > 0 {
			continue
		}
		at := l.MinRTT.Seconds()/2 + float64(l.InflightBytes+size)/l.RateBps
		if best == "" || at < bestAt {
			best, bestAt = name, at
		}
	}
	if best == "" && len(links) > 0 {
		best = links[0] // every link is being probed: keep sending
	}
	return best
}

// isLost reports whether link's packets went unacknowledged lately.
func (b *bondScheduler) isLost(link string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.links[link]
	return l != nil && l.lost
}

// sent records packet seq as gone out on link.
func (b *bondScheduler) sent(seq uint64, link string, size int, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.linkLocked(link, now)
	l.InflightBytes += size
	l.winBusy = true
	b.flight[seq] = bondSent{link, size, now}
}

// acked credits every packet the peer reports to its carrier.
func (b *bondScheduler) acked(a control.BondingAck, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := uint64(0); i < control.BondingAckSpan && i < a.Newest; i++ {
		seq := a.Newest - i
		if !a.Has(seq) {
			continue
		}
		p, ok := b.flight[seq]
		if !ok {
			continue
		}
		delete(b.flight, seq)
		l := b.linkLocked(p.link, now)
		l.InflightBytes -= p.size
		l.winBytes += p.size
		l.lost = false
		rtt := now.Sub(p.at)
		l.RTT += (rtt - l.RTT) / 8
		if rtt < l.MinRTT || now.Sub(l.minAt) > bondMinRTTAge {
			l.MinRTT, l.minAt = rtt, now
		}
	}
	b.rollLocked(now)
}

// expire gives up on packets unacknowledged for four of their carrier's
// round trips (bondLostMin to bondLostAfter): a carrier that died stops
// taking new packets that soon.
func (b *bondScheduler) expire(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for seq, p := range b.flight {
		l := b.linkLocked(p.link, now)
		if now.Sub(p.at) < min(max(4*l.RTT, bondLostMin), bondLostAfter) {
			continue
		}
		delete(b.flight, seq)
		l.InflightBytes -= p.size
		l.lost = true
	}
	b.rollLocked(now)
}

// rollLocked closes each link's rate window once it is bondWindow old.
// Only a window with data in flight says anything about the link's speed.
func (b *bondScheduler) rollLocked(now time.Time) {
	for _, l := range b.links {
		el := now.Sub(l.winStart)
		if el < bondWindow {
			continue
		}
		if l.winBusy {
			l.wins[l.winIdx] = float64(l.winBytes) / el.Seconds()
			l.winIdx = (l.winIdx + 1) % bondRateWins
			best := 0.0
			for _, w := range l.wins {
				best = max(best, w)
			}
			l.RateBps = max(best, bondRateFloor)
		}
		l.winStart, l.winBytes, l.winBusy = now, 0, l.InflightBytes > 0
	}
}

// forget drops a carrier that left the Session; its packets in flight count
// as lost.
func (b *bondScheduler) forget(link string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.links, link)
	for seq, p := range b.flight {
		if p.link == link {
			delete(b.flight, seq)
		}
	}
}

func (b *bondScheduler) linkStats() map[string]BondLinkStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]BondLinkStats, len(b.links))
	for name, l := range b.links {
		out[name] = l.BondLinkStats
	}
	return out
}
