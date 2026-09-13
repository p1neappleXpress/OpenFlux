package fluxcore

import (
	"math"
	"time"
)

// Scoring targets. A metric at its target contributes 0 to that factor; at
// zero it contributes 1. These are deliberately explicit constants, not magic
// numbers buried in a formula — the whole point is that the score is readable.
const (
	targetRTT     = 500 * time.Millisecond // RTT at/above this => latency factor 0
	targetJitter  = 150 * time.Millisecond // jitter at/above this => stability 0
	freshFull     = 2 * time.Second        // seen within this => liveness 1
	freshZero     = 30 * time.Second       // not seen for this => liveness 0
)

// Weights sum to 1. Loss and latency dominate because they hurt the user most.
var scoreWeights = [4]float64{
	0.35, // latency
	0.30, // loss
	0.20, // stability (jitter)
	0.15, // liveness
}

// Breakdown is the explainable decomposition of a health score. Every route
// decision can point at this and say *why* — the seed of the epistemic state
// described in the spec (§22). Nothing "believes"; everything shows its work.
type Breakdown struct {
	Latency   float64    `json:"latency"`   // 0..1
	Loss      float64    `json:"loss"`      // 0..1
	Stability float64    `json:"stability"` // 0..1
	Liveness  float64    `json:"liveness"`  // 0..1
	Weights   [4]float64 `json:"weights"`
	Score     float64    `json:"score"` // 0..100
}

// HealthScore reduces Metrics to a 0..100 score plus the breakdown that
// produced it. `now` is passed for deterministic liveness in tests.
func HealthScore(m Metrics, now time.Time) (float64, Breakdown) {
	b := Breakdown{Weights: scoreWeights}

	b.Latency = clamp01(1 - m.RTT.Seconds()/targetRTT.Seconds())
	b.Loss = clamp01(1 - m.LossRatio)
	b.Stability = clamp01(1 - m.Jitter.Seconds()/targetJitter.Seconds())
	b.Liveness = liveness(m, now)

	// A dead carrier can't be healthy no matter what the other numbers say.
	if !m.Connected {
		b.Liveness = 0
	}

	b.Score = 100 * (b.Latency*scoreWeights[0] +
		b.Loss*scoreWeights[1] +
		b.Stability*scoreWeights[2] +
		b.Liveness*scoreWeights[3])

	return b.Score, b
}

func liveness(m Metrics, now time.Time) float64 {
	if m.LastSeen.IsZero() {
		return 0
	}
	age := now.Sub(m.LastSeen)
	if age <= freshFull {
		return 1
	}
	if age >= freshZero {
		return 0
	}
	// Linear decay between freshFull and freshZero.
	span := (freshZero - freshFull).Seconds()
	return clamp01(1 - (age-freshFull).Seconds()/span)
}

// DegradationProbability estimates the chance a route is heading toward
// trouble, from the recent trend of its scores. It is a projection of what we
// have observed, not a claim about the future — honest by construction.
//
// It combines a downward slope (scores falling) with an absolute-health floor
// (already-low scores are inherently risky). Returns 0..1.
func DegradationProbability(scoreHistory []float64) float64 {
	n := len(scoreHistory)
	if n == 0 {
		return 0
	}
	last := scoreHistory[n-1]

	// Absolute risk: a score of 100 => 0 risk, 40 or below => high risk.
	absRisk := clamp01((70 - last) / 30)

	if n < 3 {
		return absRisk * 0.5 // not enough trend yet, be cautious not alarmist
	}

	slope := linregSlope(scoreHistory) // score units per sample
	// A falling slope raises risk; -5/sample or steeper saturates.
	trendRisk := clamp01(-slope / 5)

	// Weighted blend, capped at 1.
	return clamp01(0.55*trendRisk + 0.45*absRisk)
}

// linregSlope returns the least-squares slope of y over index x=0..n-1.
func linregSlope(y []float64) float64 {
	n := float64(len(y))
	var sx, sy, sxx, sxy float64
	for i, v := range y {
		x := float64(i)
		sx += x
		sy += v
		sxx += x * x
		sxy += x * v
	}
	denom := n*sxx - sx*sx
	if math.Abs(denom) < 1e-9 {
		return 0
	}
	return (n*sxy - sx*sy) / denom
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
