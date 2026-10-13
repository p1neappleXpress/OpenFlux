package transport

import (
	"math"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// simLink is a carrier with a fixed rate and one-way delay, FIFO.
type simLink struct {
	rate   float64 // bytes/s
	delay  time.Duration
	down   bool
	queue  []simPacket
	budget float64
}

type simPacket struct {
	seq  uint64
	size int
	at   time.Time
}

// runBond offers more than the links can carry for the given time and
// returns the bytes the scheduler put on each link in the last second.
func runBond(t *testing.T, b *bondScheduler, links map[string]*simLink, order []string, dur time.Duration, cut func(now time.Time)) map[string]int {
	t.Helper()
	const step = 10 * time.Millisecond
	const size = 1200
	t0 := time.Unix(1000, 0)
	seq := uint64(0)
	late := map[string]int{}
	var ack control.BondingAck
	var lastAck time.Time
	for now := t0; now.Before(t0.Add(dur)); now = now.Add(step) {
		if cut != nil {
			cut(now)
		}
		// Offer what a TCP window of 200 KB allows, like a bulk upload.
		inflight := 0
		for _, st := range b.linkStats() {
			inflight += st.InflightBytes
		}
		for n := 0; n < (200_000-inflight)/size; n++ {
			seq++
			name := b.pick(order, size, now)
			b.sent(seq, name, size, now)
			links[name].queue = append(links[name].queue, simPacket{seq, size, now})
			if now.After(t0.Add(dur - time.Second)) {
				late[name] += size
			}
		}
		for _, l := range links {
			if l.down {
				continue
			}
			l.budget += l.rate * step.Seconds()
			for len(l.queue) > 0 && l.budget >= float64(l.queue[0].size) && !now.Before(l.queue[0].at.Add(l.delay)) {
				l.budget -= float64(l.queue[0].size)
				ack.Set(l.queue[0].seq)
				l.queue = l.queue[1:]
			}
			if len(l.queue) == 0 {
				l.budget = math.Min(l.budget, l.rate*step.Seconds())
			}
		}
		if now.Sub(lastAck) >= 50*time.Millisecond {
			b.acked(ack, now)
			b.expire(now)
			lastAck = now
		}
	}
	return late
}

func TestBondSplitFollowsRates(t *testing.T) {
	b := newBondScheduler()
	links := map[string]*simLink{
		"mailru": {rate: 300_000, delay: 60 * time.Millisecond},
		"yandex": {rate: 100_000, delay: 150 * time.Millisecond},
	}
	got := runBond(t, b, links, []string{"mailru", "yandex"}, 6*time.Second, nil)
	ratio := float64(got["mailru"]) / float64(got["yandex"])
	if ratio < 2.55 || ratio > 3.45 {
		t.Fatalf("split %d:%d (%.2f), want about 3:1", got["mailru"], got["yandex"], ratio)
	}
	st := b.linkStats()
	if st["mailru"].RateBps < 200_000 || st["yandex"].RateBps < 60_000 {
		t.Fatalf("learned rates %+v", st)
	}
}

func TestBondMovesOffADeadLink(t *testing.T) {
	b := newBondScheduler()
	links := map[string]*simLink{
		"mailru": {rate: 200_000, delay: 60 * time.Millisecond},
		"yandex": {rate: 200_000, delay: 60 * time.Millisecond},
	}
	t0 := time.Unix(1000, 0)
	got := runBond(t, b, links, []string{"mailru", "yandex"}, 8*time.Second, func(now time.Time) {
		links["yandex"].down = now.After(t0.Add(3 * time.Second))
	})
	if share := float64(got["yandex"]) / float64(got["mailru"]+got["yandex"]); share > 0.05 {
		t.Fatalf("a dead link still gets %.0f%% of the traffic", share*100)
	}
	if st := b.linkStats()["yandex"]; st.InflightBytes > 64*1024 {
		t.Fatalf("dead link keeps %d bytes in flight", st.InflightBytes)
	}
}

func TestBondTriesANewLink(t *testing.T) {
	b := newBondScheduler()
	now := time.Unix(1000, 0)
	for i := uint64(1); i <= 100; i++ {
		b.sent(i, "mailru", 1200, now)
	}
	if got := b.pick([]string{"mailru", "boards"}, 1200, now); got != "boards" {
		t.Fatalf("picked %q over an idle new link", got)
	}
}

// At low load every packet takes the quickest carrier: splitting then would
// only reorder packets for nothing.
func TestBondLowLoadStaysOnTheQuickestLink(t *testing.T) {
	b := newBondScheduler()
	now := time.Unix(1000, 0)
	// Teach the scheduler the round trips: mailru 60 ms, yandex 200 ms.
	var ack control.BondingAck
	for i := uint64(1); i <= 20; i++ {
		name := "mailru"
		d := 60 * time.Millisecond
		if i%2 == 0 {
			name, d = "yandex", 200*time.Millisecond
		}
		b.sent(i, name, 1200, now)
		ack.Set(i)
		b.acked(ack, now.Add(d))
		ack = control.BondingAck{}
	}
	// A trickle: one packet at a time, acked before the next.
	for i := uint64(100); i < 200; i++ {
		now = now.Add(100 * time.Millisecond)
		if got := b.pick([]string{"yandex", "mailru"}, 1200, now); got != "mailru" {
			t.Fatalf("packet %d went to %q at low load", i, got)
		}
		b.sent(i, "mailru", 1200, now)
		ack.Set(i)
		b.acked(ack, now.Add(60*time.Millisecond))
	}
}
