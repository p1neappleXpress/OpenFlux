package fluxcore

import (
	"testing"
	"time"
)

func TestSnapshotIncludesStatsAndProfilesWithBrain(t *testing.T) {
	c := newClock()
	reg := newRegistry(c.now)
	bus := NewBus(10)
	mem, _ := openMemory(t.TempDir()+"/m.json", c.now)
	brain := newBrain(reg, bus, DefaultSelector(), mem, c.now)

	r := reg.Add(RouteDNA{Transport: "yandex", Exit: "fra"})
	r.SetState(StateActive)
	r.Sampler().ObserveRTT(20 * time.Millisecond)
	c.add(5 * time.Second)
	brain.Tick()

	s := buildSnapshot(reg, bus, brain)
	if s.Stats == nil {
		t.Fatal("snapshot with a brain must include stats")
	}
	if s.Stats.RoutesKnown != 1 {
		t.Fatalf("expected 1 route known, got %d", s.Stats.RoutesKnown)
	}
	if s.Stats.UptimeSec < 5 {
		t.Fatalf("uptime should reflect elapsed time, got %d", s.Stats.UptimeSec)
	}
	if len(s.Profiles) != 1 {
		t.Fatalf("memory should surface one DNA profile, got %d", len(s.Profiles))
	}

	// A brain-less snapshot must omit stats (back-compat).
	if plain := BuildSnapshot(reg, bus); plain.Stats != nil {
		t.Fatal("BuildSnapshot without a brain must not include stats")
	}
}
