package fluxcore

import (
	"fmt"
	"time"
)

// Action is what the selector decided to do this tick.
type Action string

const (
	Hold       Action = "hold"       // keep the current active route
	Adopt      Action = "adopt"      // no active route yet; make the best one active
	Failover   Action = "failover"   // switch active away from a failing/worse route
	Quarantine Action = "quarantine" // pull a persistently-bad route out of the pool
	Recover    Action = "recover"    // return a healed route from quarantine to standby
)

// Decision is one explainable routing choice. Like the health score, it carries
// its reasoning so the UI, Memory and a future Council can all see *why*.
type Decision struct {
	Action      Action  `json:"action"`
	From        string  `json:"from,omitempty"` // route ID (old active), when relevant
	To          string  `json:"to,omitempty"`   // route ID (new active / affected)
	Reason      string  `json:"reason"`
	ActiveScore float64 `json:"activeScore"`
	CandScore   float64 `json:"candScore"`
	Predictive  bool    `json:"predictive"` // true if driven by degradation forecast, not yet-felt pain
}

// Selector is the routing brain's policy. Every threshold is an explicit,
// tunable field — no magic buried in the logic.
type Selector struct {
	FailoverBelow   float64       // active score under this is "in pain"
	MinMargin       float64       // a candidate must beat active by this much to justify a switch
	Dwell           time.Duration // minimum time between voluntary switches (anti-flapping)
	PredictAbove    float64       // active degradation prob over this triggers a pre-emptive switch
	QuarantineGrace time.Duration // how long a route may be down before quarantine
	RecoverAbove    float64       // a quarantined route scoring above this (and connected) may return
}

// DefaultSelector returns balanced defaults: quick to escape real pain, slow to
// flap, willing to act on a strong forecast.
func DefaultSelector() *Selector {
	return &Selector{
		FailoverBelow:   55,
		MinMargin:       12,
		Dwell:           6 * time.Second,
		PredictAbove:    0.80,
		QuarantineGrace: 20 * time.Second,
		RecoverAbove:    70,
	}
}

// Decide chooses the primary routing action for this tick. `lastSwitch` is when
// the active route last changed, used for anti-flap dwell. It returns a Hold
// decision when nothing should change.
func (s *Selector) Decide(views []RouteView, lastSwitch, now time.Time) Decision {
	active, hasActive := pickActive(views)
	cand, hasCand := pickBestCandidate(views, active.ID)

	// 1) No usable active route: adopt the best connected candidate immediately.
	if !hasActive || !active.Metrics.Connected {
		if hasCand {
			reason := "no active path"
			if hasActive && !active.Metrics.Connected {
				reason = "active carrier is down"
			}
			return Decision{
				Action: choose(hasActive, Failover, Adopt), From: active.ID, To: cand.ID,
				Reason: reason + fmt.Sprintf(" → adopt %s (score %.0f)", cand.Label, cand.Score),
				ActiveScore: active.Score, CandScore: cand.Score,
			}
		}
		return Decision{Action: Hold, To: active.ID, Reason: "no connected route available", ActiveScore: active.Score}
	}

	// 2) Active is alive. Consider a voluntary switch only after dwell.
	if now.Sub(lastSwitch) < s.Dwell {
		return Decision{Action: Hold, To: active.ID, Reason: "holding (anti-flap dwell)", ActiveScore: active.Score}
	}

	if !hasCand {
		return Decision{Action: Hold, To: active.ID, Reason: "no better path in reserve", ActiveScore: active.Score}
	}

	betterBy := cand.Score - active.Score
	inPain := active.Score < s.FailoverBelow
	forecastBad := active.Degradation > s.PredictAbove

	if (inPain || forecastBad) && betterBy >= s.MinMargin {
		reason := fmt.Sprintf("active %.0f < %.0f, %s scores %.0f (+%.0f)",
			active.Score, s.FailoverBelow, cand.Label, cand.Score, betterBy)
		if forecastBad && !inPain {
			reason = fmt.Sprintf("forecast: %s degradation %.0f%% — pre-empt to %s (%.0f)",
				active.Label, active.Degradation*100, cand.Label, cand.Score)
		}
		return Decision{
			Action: Failover, From: active.ID, To: cand.ID, Reason: reason,
			ActiveScore: active.Score, CandScore: cand.Score, Predictive: forecastBad && !inPain,
		}
	}

	return Decision{Action: Hold, To: active.ID, Reason: "active healthy enough", ActiveScore: active.Score}
}

// PoolSweep returns quarantine/recover decisions for non-active routes: a route
// that has been unreachable past the grace period is quarantined; a quarantined
// route that has healed is recovered to standby. This is the immune response
// (spec §38) — keep bad nodes out of the rotation without losing them.
func (s *Selector) PoolSweep(views []RouteView, now time.Time) []Decision {
	var out []Decision
	for _, v := range views {
		switch v.State {
		case "standby", "unknown":
			// Only quarantine a route that was alive and went silent — never a
			// brand-new route that simply hasn't been probed yet.
			if !v.Metrics.Connected && !v.Metrics.LastSeen.IsZero() &&
				staleFor(v, now) >= s.QuarantineGrace {
				out = append(out, Decision{
					Action: Quarantine, To: v.ID,
					Reason: fmt.Sprintf("%s unreachable > %s", v.Label, s.QuarantineGrace),
				})
			}
		case "quarantined":
			if v.Metrics.Connected && v.Score >= s.RecoverAbove {
				out = append(out, Decision{
					Action: Recover, To: v.ID,
					Reason: fmt.Sprintf("%s healed (score %.0f)", v.Label, v.Score),
				})
			}
		}
	}
	return out
}

// --- helpers ---

func pickActive(views []RouteView) (RouteView, bool) {
	for _, v := range views {
		if v.State == "active" {
			return v, true
		}
	}
	return RouteView{}, false
}

// pickBestCandidate returns the highest-scoring connected route that is eligible
// to become active (standby or unknown), excluding the current active and
// anything quarantined.
func pickBestCandidate(views []RouteView, activeID string) (RouteView, bool) {
	var best RouteView
	found := false
	for _, v := range views {
		if v.ID == activeID || v.State == "quarantined" || v.State == "active" {
			continue
		}
		if !v.Metrics.Connected {
			continue
		}
		if !found || v.Score > best.Score {
			best, found = v, true
		}
	}
	return best, found
}

func staleFor(v RouteView, now time.Time) time.Duration {
	if v.Metrics.LastSeen.IsZero() {
		return 1<<62 - 1 // effectively "forever" if never seen
	}
	return now.Sub(v.Metrics.LastSeen)
}

func choose(cond bool, a, b Action) Action {
	if cond {
		return a
	}
	return b
}
