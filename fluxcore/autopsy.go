package fluxcore

import (
	"fmt"
	"time"
)

// Autopsy is the postmortem the system writes after a routing incident
// (spec §29). It is not blame — it is how a failure becomes a lesson that goes
// into Memory and, eventually, a Seed.
type Autopsy struct {
	Time        time.Time `json:"time"`
	Incident    string    `json:"incident"`
	Cause       string    `json:"cause"`
	FromRoute   string    `json:"fromRoute"`
	ToRoute     string    `json:"toRoute"`
	WhatTried   string    `json:"whatTried"`
	WhatWorked  string    `json:"whatWorked"`
	WhatFailed  string    `json:"whatFailed"`
	Learned     string    `json:"learned"`
	NewStrategy bool      `json:"newStrategy"`
}

// BuildAutopsy assembles a postmortem from a failover: the route we left, the
// route we moved to, and their measured state at the moment of the switch.
func BuildAutopsy(from, to RouteView, now time.Time) Autopsy {
	cause := classifyCause(from)
	worked := fmt.Sprintf("Switched to %s (score %.0f, rtt %v, loss %.0f%%).",
		to.Label, to.Score, from.Metrics.RTT.Round(time.Millisecond), to.Metrics.LossRatio*100)
	learned := fmt.Sprintf(
		"When %s shows %s, %s is a reliable fallback. Reputation updated.",
		from.Label, cause, to.Label)

	return Autopsy{
		Time:      now,
		Incident:  fmt.Sprintf("Active path %s degraded", from.Label),
		Cause:     cause,
		FromRoute: from.ID,
		ToRoute:   to.ID,
		WhatTried: fmt.Sprintf("Held %s while it degraded (score fell to %.0f, degradation %.0f%%).",
			from.Label, from.Score, from.Degradation*100),
		WhatWorked:  worked,
		WhatFailed:  fmt.Sprintf("%s: %s", from.Label, causeDetail(from)),
		Learned:     learned,
		NewStrategy: true,
	}
}

func classifyCause(v RouteView) string {
	switch {
	case !v.Metrics.Connected:
		return "carrier disconnect"
	case v.Metrics.LossRatio > 0.3:
		return "high packet loss"
	case v.Metrics.RTT > targetRTT:
		return "latency spike"
	case v.Breakdown.Stability < 0.4:
		return "instability (jitter)"
	default:
		return "general degradation"
	}
}

func causeDetail(v RouteView) string {
	return fmt.Sprintf("rtt %v, loss %.0f%%, jitter %v, liveness %.2f",
		v.Metrics.RTT.Round(time.Millisecond),
		v.Metrics.LossRatio*100,
		v.Metrics.Jitter.Round(time.Millisecond),
		v.Breakdown.Liveness)
}
