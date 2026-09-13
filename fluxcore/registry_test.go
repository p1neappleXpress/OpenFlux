package fluxcore

import (
	"testing"
	"time"
)

func TestRegistryAddIsIdempotent(t *testing.T) {
	reg := NewRegistry()
	dna := RouteDNA{Transport: "yandex", Exit: "fra"}
	a := reg.Add(dna)
	b := reg.Add(dna)
	if a != b {
		t.Fatal("adding the same DNA twice must return the same route")
	}
	if reg.Len() != 1 {
		t.Fatalf("expected 1 route, got %d", reg.Len())
	}
}

func TestRegistryBestPicksHealthiestConnected(t *testing.T) {
	c := newClock()
	reg := newRegistry(c.now)

	good := reg.Add(RouteDNA{Transport: "yandex", Exit: "good"})
	bad := reg.Add(RouteDNA{Transport: "yandex", Exit: "bad"})
	down := reg.Add(RouteDNA{Transport: "oneme", Exit: "down"})

	// good: fast, no loss, fresh, connected
	good.Sampler().SetConnected(true)
	good.Sampler().ObserveRTT(20 * time.Millisecond)
	good.SetState(StateActive)

	// bad: connected but lossy and slow
	bad.Sampler().SetConnected(true)
	bad.Sampler().ObserveRTT(480 * time.Millisecond)
	for i := 0; i < 6; i++ {
		bad.Sampler().ObserveLoss()
	}

	// down: not connected (no successful probes) -> must never be chosen,
	// even though with zero RTT its raw factors would look great.
	down.Sampler().SetConnected(false)
	down.Sampler().ObserveLoss()

	_, best, ok := reg.Best()
	if !ok {
		t.Fatal("expected a best route")
	}
	if best.DNA.Exit != "good" {
		t.Fatalf("healthiest connected route should win, got %q", best.DNA.Exit)
	}
}

func TestRegistryPoolsGroupByState(t *testing.T) {
	reg := NewRegistry()
	a := reg.Add(RouteDNA{Transport: "yandex", Exit: "a"})
	b := reg.Add(RouteDNA{Transport: "yandex", Exit: "b"})
	a.SetState(StateActive)
	b.SetState(StateStandby)
	a.Sampler().SetConnected(true)
	b.Sampler().SetConnected(true)

	pools := reg.Pools()
	if len(pools["active"]) != 1 || len(pools["standby"]) != 1 {
		t.Fatalf("unexpected pools: %+v", pools)
	}
}

func TestSnapshotBuildsAndBPMReacts(t *testing.T) {
	c := newClock()
	reg := newRegistry(c.now)
	bus := NewBus(10)
	rt := reg.Add(RouteDNA{Transport: "yandex", Exit: "fra"})
	rt.SetState(StateActive)
	rt.Sampler().SetConnected(true)
	rt.Sampler().ObserveRTT(20 * time.Millisecond)

	snap := BuildSnapshot(reg, bus)
	if snap.Status != "alive" {
		t.Fatalf("healthy system should be alive, got %q", snap.Status)
	}
	if snap.BPM < 50 || snap.BPM > 80 {
		t.Fatalf("healthy BPM should be calm, got %d", snap.BPM)
	}
}

// The headline must track the ACTIVE path, so a struggling active route reads
// as stress even when a healthy standby is held in reserve.
func TestSnapshotHeadlineTracksActiveNotBest(t *testing.T) {
	c := newClock()
	reg := newRegistry(c.now)
	bus := NewBus(10)

	active := reg.Add(RouteDNA{Transport: "yandex", Exit: "fra"})
	standby := reg.Add(RouteDNA{Transport: "oneme", Exit: "ams"})
	active.SetState(StateActive)
	standby.SetState(StateStandby)

	// Active path is in a storm: slow + very lossy.
	active.Sampler().SetConnected(true)
	active.Sampler().ObserveRTT(480 * time.Millisecond)
	for i := 0; i < 8; i++ {
		active.Sampler().ObserveLoss()
	}
	// Standby is a healthy reserve.
	standby.Sampler().SetConnected(true)
	standby.Sampler().ObserveRTT(30 * time.Millisecond)

	snap := BuildSnapshot(reg, bus)
	if snap.Status != "stress" {
		t.Fatalf("a struggling active path must read as stress, got %q (score %v)", snap.Status, snap.Score)
	}
	if snap.BPM <= 80 {
		t.Fatalf("stressed system should have an elevated BPM, got %d", snap.BPM)
	}
}
