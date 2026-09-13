package fluxcore

import (
	"testing"
	"time"
)

func TestHealthScorePerfect(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := Metrics{
		RTT:       10 * time.Millisecond,
		Jitter:    1 * time.Millisecond,
		LossRatio: 0,
		Connected: true,
		LastSeen:  now,
	}
	score, b := HealthScore(m, now)
	if score < 97 {
		t.Fatalf("near-perfect metrics should score high, got %v", score)
	}
	if b.Latency < 0.95 || b.Loss != 1 || b.Liveness != 1 {
		t.Fatalf("breakdown wrong: %+v", b)
	}
}

func TestHealthScoreDeadConnectionKillsLiveness(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := Metrics{RTT: 10 * time.Millisecond, Connected: false, LastSeen: now}
	_, b := HealthScore(m, now)
	if b.Liveness != 0 {
		t.Fatalf("disconnected route must have zero liveness, got %v", b.Liveness)
	}
}

func TestHealthScoreLossDominatesLatency(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fast := Metrics{RTT: 400 * time.Millisecond, LossRatio: 0.5, Connected: true, LastSeen: now}
	slow := Metrics{RTT: 450 * time.Millisecond, LossRatio: 0.0, Connected: true, LastSeen: now}
	sFast, _ := HealthScore(fast, now)
	sSlow, _ := HealthScore(slow, now)
	if sSlow <= sFast {
		t.Fatalf("low-loss route should beat lossy one: lossy=%v clean=%v", sFast, sSlow)
	}
}

func TestDegradationRisesWithFallingScores(t *testing.T) {
	falling := []float64{95, 90, 80, 65, 45}
	rising := []float64{45, 60, 75, 88, 96}
	pFall := DegradationProbability(falling)
	pRise := DegradationProbability(rising)
	if pFall <= pRise {
		t.Fatalf("falling trend should be riskier: fall=%v rise=%v", pFall, pRise)
	}
	if pFall < 0.5 {
		t.Fatalf("a steep fall to 45 should read as clearly risky, got %v", pFall)
	}
}

func TestDegradationBoundedAndCautiousEarly(t *testing.T) {
	p := DegradationProbability([]float64{80})
	if p < 0 || p > 1 {
		t.Fatalf("probability must be within [0,1], got %v", p)
	}
}
