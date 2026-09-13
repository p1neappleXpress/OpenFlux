package fluxcore

import (
	"math"
	"testing"
	"time"
)

func TestEwmaConvergesToConstant(t *testing.T) {
	e := NewEwma(0.3)
	for i := 0; i < 100; i++ {
		e.Update(42)
	}
	if math.Abs(e.Value()-42) > 1e-6 {
		t.Fatalf("expected convergence to 42, got %v", e.Value())
	}
}

func TestEwmaFirstSampleIsExact(t *testing.T) {
	e := NewEwma(0.1)
	if got := e.Update(7); got != 7 {
		t.Fatalf("first sample should be exact, got %v", got)
	}
	if !e.Ready() {
		t.Fatal("Ready should be true after a sample")
	}
}

func TestEwmaBadAlphaFallsBack(t *testing.T) {
	if NewEwma(0).alpha != 0.2 || NewEwma(5).alpha != 0.2 {
		t.Fatal("out-of-range alpha should fall back to 0.2")
	}
}

func TestSamplerRTTandJitter(t *testing.T) {
	c := newClock()
	s := newSampler(c.now)
	s.ObserveRTT(100 * time.Millisecond)
	s.ObserveRTT(100 * time.Millisecond)
	snap := s.Snapshot()
	if snap.RTT < 90*time.Millisecond || snap.RTT > 110*time.Millisecond {
		t.Fatalf("RTT should be ~100ms, got %v", snap.RTT)
	}
	if !snap.Connected {
		t.Fatal("a successful RTT should mark connected")
	}
	// Introduce variation -> jitter should rise above zero.
	s.ObserveRTT(300 * time.Millisecond)
	if s.Snapshot().Jitter <= 0 {
		t.Fatal("jitter should be positive after RTT changes")
	}
}

func TestSamplerLossRisesAndFalls(t *testing.T) {
	c := newClock()
	s := newSampler(c.now)
	for i := 0; i < 10; i++ {
		s.ObserveLoss()
	}
	high := s.Snapshot().LossRatio
	if high < 0.5 {
		t.Fatalf("loss ratio should be high after repeated losses, got %v", high)
	}
	for i := 0; i < 30; i++ {
		s.ObserveRTT(50 * time.Millisecond) // successes push loss down
	}
	if low := s.Snapshot().LossRatio; low >= high {
		t.Fatalf("loss ratio should fall after successes: high=%v low=%v", high, low)
	}
}

func TestSamplerThroughputWindow(t *testing.T) {
	c := newClock()
	s := newSampler(c.now)
	s.ObserveBytes(1000)
	c.add(1 * time.Second) // close the window
	s.ObserveBytes(1000)   // triggers rate computation over ~1s
	if tp := s.Snapshot().Throughput; tp <= 0 {
		t.Fatalf("throughput should be positive after a full window, got %v", tp)
	}
}
