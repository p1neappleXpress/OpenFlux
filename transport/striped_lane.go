package transport

import (
	"math"
	"os"
	"time"
)

// laneState is what the sender knows about one lane: whether what it sends
// arrives at all, how fast it may send, and its recent loss for the stats.
//
// The SFU forwards a lane only up to an allowance and drops the rest, and the
// allowance differs from lane to lane (two lanes of one stripe, same SFU, same
// rate: 6% and 64% lost) and with the rate (one lane between two phones: 5%
// lost at 1000 pieces/s, 8% at 1200, 20% at the full clock of ~1400). Every
// loss costs a retransmission and, worse for the tunnel's TCP, the round trip
// it waits for it. So each lane is paced. Its loss is averaged over the
// windows (paceWindow, and at least paceMinSamples pieces) - the SFU loses
// 5-15% in bursts even far below the allowance, too noisy to act on one
// window - and only a window in which the rate held the lane back says
// anything about the rate:
//
//   - losing much (paceBackoffLoss): back off by paceBackoff;
//   - losing little (paceGrowLoss): grow, quickly back up to the last rate
//     that lost little (good), gently above it;
//   - nothing arrived at all, or the SFU's ramp: hold. When a stream starts
//     after a pause the SFU drops nearly everything for some seconds whatever
//     the rate (paceRamp), and a whole carrier that stalls is not a rate
//     problem either; backing off then only costs the regrowth after it.
//
// The data the lanes cannot take waits in the send queue, where CoDel turns
// it into the signal TCP reads.
type laneState struct {
	started              bool
	sent, lost           uint64 // this second, for the stats
	loss                 float64
	unanswered           int // pieces sent since the last delivery
	deliveredAt, probeAt time.Time
	statAt               time.Time

	rate      float64 // pieces/s the lane may send
	good      float64 // the last rate that lost little
	lossAvg   float64
	tokens    float64
	tokenAt   time.Time
	wSent     int // the pacing window: pieces sent, found lost, delivered
	wLost     int
	wGot      int
	wLimited  bool // the lane had more to send than its rate allowed
	wAt       time.Time
	busyAt    time.Time // the last window that carried traffic
	limitedAt time.Time // the last window the rate held the lane back
	rampUntil time.Time

	deadAfter time.Duration // 0: deadAfterDefault
	noPace    bool          // StripeOptions.NoPacing

	// The delay-based window (StripeOptions.LaneWindow), for lanes that lose
	// nothing but queue: how many pieces may be sent and not yet acked, grown
	// while the lane's round trip stays near its best and cut once the
	// carrier's queue shows up in it. A slow lane then holds few pieces and a
	// fast one many, so lanes of very different speed add up.
	winOn     bool
	win       float64
	slowStart bool
	baseRTT   time.Duration
	baseAt    time.Time
	lrtt      time.Duration
	cutAt     time.Time
}

const (
	laneWinMin  = 8
	laneWinMax  = 4096
	laneWinInit = 24
)

// winRoom is how many more pieces the lane may take now, given how many it
// has in flight; a large number when the window is off.
func (l *laneState) winRoom(inflight int) int {
	if !l.winOn {
		return math.MaxInt32
	}
	if l.win == 0 {
		l.win, l.slowStart = laneWinInit, true
	}
	if r := int(l.win) - inflight; r > 0 {
		return r
	}
	return 0
}

// winSample feeds the window one round trip of the lane (a piece sent once
// and acked).
func (l *laneState) winSample(d time.Duration, now time.Time) {
	if !l.winOn {
		return
	}
	if l.win == 0 {
		l.win, l.slowStart = laneWinInit, true
	}
	if l.baseRTT == 0 || d < l.baseRTT || now.Sub(l.baseAt) > 20*time.Second {
		l.baseRTT, l.baseAt = d, now
	}
	if l.lrtt == 0 {
		l.lrtt = d
	} else {
		l.lrtt = (7*l.lrtt + d) / 8
	}
	grow := l.baseRTT*2 + 80*time.Millisecond
	switch {
	case l.lrtt <= grow && l.slowStart:
		l.win++ // doubles per round trip
	case l.lrtt <= l.baseRTT+l.baseRTT/2+60*time.Millisecond:
		l.slowStart = false
		l.win += 1 / l.win // one piece per round trip
	case l.lrtt > l.baseRTT*3+150*time.Millisecond:
		l.slowStart = false
		if now.Sub(l.cutAt) > l.baseRTT {
			l.cutAt = now
			l.win *= 0.90
		}
	default:
		l.slowStart = false
	}
	l.win = math.Min(math.Max(l.win, laneWinMin), laneWinMax)
}

// pacingOn: STRIPE_PACE=0 lets every lane send all its clock allows.
var pacingOn = os.Getenv("STRIPE_PACE") != "0"

// paceFixed (STRIPE_PACE_FIXED, pieces/s) holds every lane at one rate, for
// measuring what a lane loses at a given rate.
var paceFixed = float64(envIntOr("STRIPE_PACE_FIXED", 0))

const (
	paceWindow      = 200 * time.Millisecond
	paceMinSamples  = 20          // a window waits for this many pieces...
	paceMaxWindow   = time.Second // ...up to this long
	paceStartRate   = 1200.0
	paceMinRate     = 300.0
	paceMaxRate     = 1440.0 // ~16 Mbit/s of lane packets, what the SFU gives one subscriber
	paceBackoffLoss = 0.2
	paceGrowLoss    = 0.1
	paceBackoff     = 0.92
	paceLossWeight  = 0.3  // of a full window (100 pieces) in the average
	paceRecover     = 1.15 // growth per window below the good rate
	paceGrow        = 1.03 // growth per window above it
	paceGoodDecay   = 0.999
	paceBusyRate    = 50.0            // pieces/s that count as traffic
	paceIdle        = 2 * time.Second // a pause this long...
	paceRamp        = 8 * time.Second // ...is followed by the SFU's ramp
)

func (l *laneState) start(now time.Time) {
	l.started, l.statAt = true, now
	l.rate, l.good, l.tokens, l.tokenAt, l.wAt = paceStartRate, paceStartRate, 3, now, now
	if paceFixed > 0 {
		l.rate = paceFixed
	}
}

// allow returns how many pieces the lane may send at this tick.
func (l *laneState) allow(now time.Time) int {
	if !l.started {
		l.start(now)
	}
	if !pacingOn || l.noPace {
		return math.MaxInt32
	}
	l.tokens += l.rate * now.Sub(l.tokenAt).Seconds()
	l.tokenAt = now
	if burst := math.Max(l.rate*0.03, 3); l.tokens > burst {
		l.tokens = burst
	}
	return int(l.tokens)
}

// sentN records a tick's pieces; limited: the rate, not the data, set how
// many.
func (l *laneState) sentN(n int, limited bool, now time.Time) {
	if !l.started {
		l.start(now)
	}
	l.tokens -= float64(n)
	l.sent += uint64(n)
	l.wSent += n
	l.unanswered += n
	if limited {
		l.wLimited = true
	}
}

// got records that the far end received a piece this lane sent.
func (l *laneState) got(now time.Time) {
	l.unanswered, l.deliveredAt = 0, now
	l.wGot++
}

func (l *laneState) lostOne() {
	l.lost++
	l.wLost++
}

// dead: the lane keeps sending and nothing it sends arrives - the SFU is not
// forwarding it (a subscription that never started or broke). Such a lane
// only gets a probe now and then until something arrives again.
func (l *laneState) dead(now time.Time) bool {
	after := l.deadAfter
	if after <= 0 {
		after = deadAfterDefault
	}
	return l.started && l.unanswered >= 20 && now.Sub(l.deliveredAt) > after
}

const deadAfterDefault = 600 * time.Millisecond

// pace adjusts the rate after a window that lost loss of what it sent.
func (l *laneState) pace(loss float64, now time.Time) {
	l.good *= paceGoodDecay
	if paceFixed > 0 || l.wGot == 0 || now.Before(l.rampUntil) {
		return // an outage or the SFU's ramp: no rate fixes either
	}
	l.lossAvg += math.Min(float64(l.wSent)/100, 1) * paceLossWeight * (loss - l.lossAvg)
	if !l.wLimited {
		return // the lane sent less than its rate: nothing learned about it
	}
	switch {
	case l.lossAvg > paceBackoffLoss:
		l.rate = math.Max(l.rate*paceBackoff, paceMinRate)
	case l.lossAvg < paceGrowLoss:
		l.good = math.Max(l.good, l.rate)
		if l.rate < l.good*0.95 {
			l.rate = math.Min(l.rate*paceRecover, l.good)
		} else {
			l.rate *= paceGrow
		}
		l.rate = math.Min(l.rate, paceMaxRate)
	}
}

// full: past the SFU's ramp, not losing heavily, and held back by its rate
// within the last second - the lane carries what the SFU lets it, at
// whatever rate that is. (Pacing keeps a lane's loss around
// paceBackoffLoss, so the bound sits above it.)
func (l *laneState) full(now time.Time) bool {
	return l.started && !now.Before(l.rampUntil) && l.lossAvg < 1.5*paceBackoffLoss && now.Sub(l.limitedAt) < time.Second
}

// tick closes the pacing window and folds the last second into the loss
// figure.
func (l *laneState) tick(now time.Time) {
	if !l.started {
		return
	}
	if el := now.Sub(l.wAt); el >= paceWindow && (l.wSent >= paceMinSamples || el >= paceMaxWindow) {
		if float64(l.wSent)/el.Seconds() >= paceBusyRate {
			if l.busyAt.IsZero() || now.Sub(l.busyAt) > paceIdle+el {
				l.rampUntil = now.Add(paceRamp)
			}
			l.busyAt = now
		}
		if l.wLimited {
			l.limitedAt = now
		}
		if l.wSent > 0 {
			l.pace(float64(l.wLost)/float64(l.wSent), now)
		}
		l.wSent, l.wLost, l.wGot, l.wLimited, l.wAt = 0, 0, 0, false, now
	}
	if now.Sub(l.statAt) >= time.Second {
		if l.sent >= 20 {
			l.loss = float64(l.lost) / float64(l.sent)
		}
		l.sent, l.lost, l.statAt = 0, 0, now
	}
}
