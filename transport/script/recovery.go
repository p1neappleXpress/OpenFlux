package script

import "time"

// Recovery decides what to do when an installed transport keeps failing to
// carry traffic. A script transport that stops working usually means the
// service it rides changed under it (a new Mail.ru/Yandex deploy), and the
// fix is a newer .flux the author already published - so the first move is to
// pull an update, not to give up. The ladder climbs:
//
//	recheck  - ask the author's update.json for a newer package now, out of
//	           band, instead of waiting for the daily check; apply it if the
//	           core says no question is needed (official key, same wire).
//	rollback - the current package is the problem (a bad update, or one that
//	           no longer connects); swap back to the <id>.flux.prev kept by the
//	           last apply.
//	fallback - nothing scripted works; hand the carrier to the native
//	           transport of the same service, which ships frozen in the binary.
//
// Like everything else in this package the ladder emits machine codes, never
// user text (the apps own the wording), and holds no I/O: it is a pure
// decision over a stream of connect/fail events, driven the same way on the
// client app and on an exit node so both recover identically.
type RecoveryAction int

const (
	// RecoverNone: stay the course, the streak has not earned a step yet.
	RecoverNone RecoveryAction = iota
	// RecoverRecheck: check for and apply an auto-OK update right now.
	RecoverRecheck
	// RecoverRollback: return to the previous package (<id>.flux.prev).
	RecoverRollback
	// RecoverFallback: give the carrier to the native transport.
	RecoverFallback
)

// Code is the machine code an action reports; "" for RecoverNone.
func (a RecoveryAction) Code() string {
	switch a {
	case RecoverRecheck:
		return "recheck"
	case RecoverRollback:
		return "rollback"
	case RecoverFallback:
		return "fallback"
	default:
		return ""
	}
}

// RecoveryPolicy is the ladder's thresholds. A step fires when the number of
// consecutive connect failures in the current streak first reaches its count;
// each step fires at most once per streak, so climbing is one rung at a time.
// Zero fields take DefaultRecoveryPolicy's value, so a caller can set only
// what it wants to change.
type RecoveryPolicy struct {
	// RecheckAfter/RollbackAfter/FallbackAfter are failure counts; they must
	// be strictly increasing for the ladder to climb rung by rung. A step set
	// to 0 after Normalize keeps its default; a step smaller than the one
	// before it is raised to one past it so a misconfiguration can never make
	// a later, heavier action fire before a lighter one.
	RecheckAfter  int
	RollbackAfter int
	FallbackAfter int

	// StableFor is how long a connection must hold before the run counts as
	// recovered: a flap that connects for an instant and dies again must not
	// reset the ladder, or a broken package would re-download on every flap.
	StableFor time.Duration

	// Window starts a fresh streak when a failure arrives this long after the
	// previous event: trouble now is unrelated to trouble hours ago, and the
	// ladder should start again from the bottom rather than jump a rung.
	Window time.Duration
}

// DefaultRecoveryPolicy is tuned for a doc transport whose own reconnect loop
// retries every few seconds: a couple of failures is noise, a handful is a
// real outage worth pulling an update for, and persistence past that means the
// package itself is the problem.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{
		RecheckAfter:  2,
		RollbackAfter: 4,
		FallbackAfter: 6,
		StableFor:     60 * time.Second,
		Window:        10 * time.Minute,
	}
}

// Normalize fills zero fields from the default and guarantees the three
// thresholds strictly increase.
func (p RecoveryPolicy) Normalize() RecoveryPolicy {
	d := DefaultRecoveryPolicy()
	if p.RecheckAfter <= 0 {
		p.RecheckAfter = d.RecheckAfter
	}
	if p.RollbackAfter <= 0 {
		p.RollbackAfter = d.RollbackAfter
	}
	if p.FallbackAfter <= 0 {
		p.FallbackAfter = d.FallbackAfter
	}
	if p.StableFor <= 0 {
		p.StableFor = d.StableFor
	}
	if p.Window <= 0 {
		p.Window = d.Window
	}
	if p.RollbackAfter <= p.RecheckAfter {
		p.RollbackAfter = p.RecheckAfter + 1
	}
	if p.FallbackAfter <= p.RollbackAfter {
		p.FallbackAfter = p.RollbackAfter + 1
	}
	return p
}

// recoveryRung is how far up the ladder a streak has already climbed, so a
// step does not fire twice for the same streak.
type recoveryRung int

const (
	rungNormal recoveryRung = iota
	rungRechecked
	rungRolledBack
	rungFellBack
)

// RecoveryLadder tracks one transport's connect health and returns the next
// action to take. It is not safe for concurrent use: drive it from the one
// goroutine that owns the transport's supervision (the Session supervisor on
// each side), exactly as goja runtimes are driven from a single loop.
type RecoveryLadder struct {
	p RecoveryPolicy

	fails       int
	rung        recoveryRung
	lastEvent   time.Time // last OnConnected/OnFailed, zero before any
	connectedAt time.Time // when the live connection began, zero when down
}

// NewRecoveryLadder builds a ladder with the policy normalized.
func NewRecoveryLadder(p RecoveryPolicy) *RecoveryLadder {
	return &RecoveryLadder{p: p.Normalize()}
}

// OnConnected records that the transport reached a working connected state.
// It never asks for an action - good news only ever calms the ladder - but
// the reset waits until the connection has proven it can hold StableFor (see
// OnFailed), so a flap does not wipe a streak mid-climb.
func (l *RecoveryLadder) OnConnected(now time.Time) RecoveryAction {
	l.connectedAt = now
	l.lastEvent = now
	return RecoverNone
}

// OnFailed records a failed or lost connection and returns the next action.
// A failure is: Start() returned an error, the script reached a dead state, or
// a reconnect was abandoned - whatever the supervisor already treats as "this
// carrier is not up".
func (l *RecoveryLadder) OnFailed(now time.Time) RecoveryAction {
	// A run that held StableFor and only now dropped was healthy: the streak
	// that may have climbed the ladder is over, this is a fresh problem.
	if !l.connectedAt.IsZero() && now.Sub(l.connectedAt) >= l.p.StableFor {
		l.reset()
	} else if !l.lastEvent.IsZero() && now.Sub(l.lastEvent) > l.p.Window {
		// The previous trouble was long ago; do not carry its count forward.
		l.reset()
	}

	l.connectedAt = time.Time{}
	l.lastEvent = now
	l.fails++

	// Climb at most one rung per failure: test the heaviest first, each
	// guarded by the rung already reached, so a step never fires twice and a
	// later step cannot precede an earlier one.
	switch {
	case l.rung < rungFellBack && l.fails >= l.p.FallbackAfter:
		l.rung = rungFellBack
		return RecoverFallback
	case l.rung < rungRolledBack && l.fails >= l.p.RollbackAfter:
		l.rung = rungRolledBack
		return RecoverRollback
	case l.rung < rungRechecked && l.fails >= l.p.RecheckAfter:
		l.rung = rungRechecked
		return RecoverRecheck
	default:
		return RecoverNone
	}
}

func (l *RecoveryLadder) reset() {
	l.fails = 0
	l.rung = rungNormal
}

// Fails is the current consecutive-failure count, for diagnostics.
func (l *RecoveryLadder) Fails() int { return l.fails }
