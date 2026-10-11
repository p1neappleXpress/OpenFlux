package transport

import "time"

// laneState is what the sender knows about one lane: whether what it sends
// arrives at all, and its recent loss for the stats.
type laneState struct {
	started              bool
	sent, lost           uint64 // this second
	loss                 float64
	unanswered           int // pieces sent since the last delivery
	deliveredAt, probeAt time.Time
	statAt               time.Time
}

func (l *laneState) sentN(n int, now time.Time) {
	if !l.started {
		l.started, l.statAt = true, now
	}
	l.sent += uint64(n)
	l.unanswered += n
}

// got records that the far end received a piece this lane sent.
func (l *laneState) got(now time.Time) { l.unanswered, l.deliveredAt = 0, now }

func (l *laneState) lostOne() { l.lost++ }

// dead: the lane keeps sending and nothing it sends arrives - the SFU is not
// forwarding it (a subscription that never started or broke). Such a lane
// only gets a probe now and then until something arrives again.
func (l *laneState) dead(now time.Time) bool {
	return l.started && l.unanswered >= 20 && now.Sub(l.deliveredAt) > 600*time.Millisecond
}

// tick folds the last second into the loss figure.
func (l *laneState) tick(now time.Time) {
	if !l.started || now.Sub(l.statAt) < time.Second {
		return
	}
	if l.sent >= 20 {
		l.loss = float64(l.lost) / float64(l.sent)
	}
	l.sent, l.lost, l.statAt = 0, 0, now
}
