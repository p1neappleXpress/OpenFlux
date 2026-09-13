package fluxcore

import (
	"testing"
	"time"
)

// mkview builds a RouteView for selector tests without a full sampler.
func mkview(id, state string, score float64, connected bool, degradation float64, lastSeen time.Time) RouteView {
	return RouteView{
		ID:          id,
		Label:       id,
		DNA:         RouteDNA{Transport: id},
		State:       state,
		Score:       score,
		Degradation: degradation,
		Metrics:     Metrics{Connected: connected, LastSeen: lastSeen, RTT: 50 * time.Millisecond},
	}
}

func TestSelectorAdoptsWhenNoActive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	views := []RouteView{
		mkview("a", "standby", 90, true, 0, now),
		mkview("b", "standby", 70, true, 0, now),
	}
	d := DefaultSelector().Decide(views, time.Time{}, now)
	if d.Action != Adopt || d.To != "a" {
		t.Fatalf("should adopt best connected route, got %+v", d)
	}
}

func TestSelectorFailsOverWhenActiveDown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	views := []RouteView{
		mkview("a", "active", 40, false, 0, now), // active carrier down
		mkview("b", "standby", 80, true, 0, now),
	}
	// Even with no dwell elapsed, a down active must fail over immediately.
	d := DefaultSelector().Decide(views, now, now)
	if d.Action != Failover || d.To != "b" {
		t.Fatalf("down active must fail over immediately, got %+v", d)
	}
}

func TestSelectorFailsOverOnPain(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	past := now.Add(-30 * time.Second) // dwell satisfied
	views := []RouteView{
		mkview("a", "active", 40, true, 0.2, now),
		mkview("b", "standby", 90, true, 0, now),
	}
	d := DefaultSelector().Decide(views, past, now)
	if d.Action != Failover || d.To != "b" || d.Predictive {
		t.Fatalf("low active + better standby should fail over (not predictive), got %+v", d)
	}
}

func TestSelectorHysteresisHolds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	recent := now.Add(-2 * time.Second) // within 6s dwell
	views := []RouteView{
		mkview("a", "active", 40, true, 0.2, now),
		mkview("b", "standby", 90, true, 0, now),
	}
	d := DefaultSelector().Decide(views, recent, now)
	if d.Action != Hold {
		t.Fatalf("within dwell the brain must hold to avoid flapping, got %+v", d)
	}
}

func TestSelectorPredictivePreempt(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	past := now.Add(-30 * time.Second)
	// Active still scores OK (65) but its forecast is dire (0.9).
	views := []RouteView{
		mkview("a", "active", 65, true, 0.9, now),
		mkview("b", "standby", 90, true, 0, now),
	}
	d := DefaultSelector().Decide(views, past, now)
	if d.Action != Failover || !d.Predictive {
		t.Fatalf("a dire forecast should trigger a predictive pre-empt, got %+v", d)
	}
}

func TestSelectorHoldsWithoutBetterOption(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	past := now.Add(-30 * time.Second)
	views := []RouteView{
		mkview("a", "active", 40, true, 0.5, now),
		mkview("b", "standby", 44, true, 0, now), // only +4, below margin
	}
	d := DefaultSelector().Decide(views, past, now)
	if d.Action != Hold {
		t.Fatalf("a candidate that isn't clearly better should not trigger a switch, got %+v", d)
	}
}

func TestSelectorQuarantineAndRecover(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sel := DefaultSelector()

	// A standby that has been silent past the grace window.
	down := []RouteView{mkview("x", "standby", 10, false, 0, now.Add(-30*time.Second))}
	qs := sel.PoolSweep(down, now)
	if len(qs) != 1 || qs[0].Action != Quarantine {
		t.Fatalf("silent standby should be quarantined, got %+v", qs)
	}

	// A brand-new unknown route (never seen) must NOT be quarantined.
	fresh := []RouteView{mkview("y", "unknown", 0, false, 0, time.Time{})}
	if got := sel.PoolSweep(fresh, now); len(got) != 0 {
		t.Fatalf("never-probed route must not be quarantined, got %+v", got)
	}

	// A quarantined route that healed should recover.
	healed := []RouteView{mkview("z", "quarantined", 80, true, 0, now)}
	rs := sel.PoolSweep(healed, now)
	if len(rs) != 1 || rs[0].Action != Recover {
		t.Fatalf("healed route should recover, got %+v", rs)
	}
}

func TestMemoryRoundTripAndAggregate(t *testing.T) {
	path := t.TempDir() + "/mem.json"
	m, err := OpenMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	v := mkview("r1", "active", 80, true, 0, time.Now())
	m.RecordRoute(v)
	v.Score = 60
	m.RecordRoute(v)
	m.NoteDecision(Decision{Action: Failover, From: "r1", To: "r2"})
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}

	m2, err := OpenMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := m2.Profile("r1")
	if !ok {
		t.Fatal("profile should persist across reload")
	}
	if p.Samples != 2 {
		t.Fatalf("expected 2 samples, got %d", p.Samples)
	}
	if p.AvgScore < 69 || p.AvgScore > 71 {
		t.Fatalf("avg of 80 and 60 should be ~70, got %v", p.AvgScore)
	}
	if p.BestScore != 80 || p.WorstScore != 60 {
		t.Fatalf("best/worst wrong: %v/%v", p.BestScore, p.WorstScore)
	}
	if p.Failovers != 1 {
		t.Fatalf("failover counter should persist, got %d", p.Failovers)
	}
}

func TestBrainFailsOverEndToEnd(t *testing.T) {
	c := newClock()
	reg := newRegistry(c.now)
	bus := NewBus(50)
	mem, _ := openMemory(t.TempDir()+"/m.json", c.now)
	brain := newBrain(reg, bus, DefaultSelector(), mem, c.now)

	a := reg.Add(RouteDNA{Transport: "yandex", Exit: "fra"})
	b := reg.Add(RouteDNA{Transport: "oneme", Exit: "ams"})
	a.SetState(StateActive)
	b.SetState(StateStandby)

	// Both healthy at first.
	for i := 0; i < 3; i++ {
		a.Sampler().ObserveRTT(20 * time.Millisecond)
		b.Sampler().ObserveRTT(25 * time.Millisecond)
		c.add(1 * time.Second)
		brain.Tick()
	}
	if brain.currentActiveID() != a.DNA.ID() {
		t.Fatalf("healthy A should remain active")
	}

	// Storm A: slow + very lossy. B stays healthy.
	c.add(10 * time.Second) // ensure dwell satisfied
	a.Sampler().ObserveRTT(480 * time.Millisecond)
	for i := 0; i < 8; i++ {
		a.Sampler().ObserveLoss()
	}
	b.Sampler().ObserveRTT(25 * time.Millisecond)

	st := brain.Tick()
	if st.Decision.Action != Failover {
		t.Fatalf("storm on active should trigger failover, got %+v", st.Decision)
	}
	if brain.currentActiveID() != b.DNA.ID() {
		t.Fatalf("after failover, B should be active; active=%s", brain.currentActiveID())
	}
	if a.State() != StateStandby {
		t.Fatalf("old active A should drop to standby, got %s", a.State())
	}
	if st.Autopsy == nil {
		t.Fatal("a failover should produce an autopsy")
	}
	if st.Autopsy.Cause == "" {
		t.Fatal("autopsy should classify a cause")
	}

	// Memory should have recorded the failover on A's profile.
	if p, ok := mem.Profile(a.DNA.ID()); !ok || p.Failovers < 1 {
		t.Fatalf("memory should note the failover away from A")
	}
}

func TestBrainAdoptsWhenNoActive(t *testing.T) {
	c := newClock()
	reg := newRegistry(c.now)
	brain := newBrain(reg, NewBus(10), DefaultSelector(), nil, c.now)

	r := reg.Add(RouteDNA{Transport: "yandex"})
	r.Sampler().ObserveRTT(20 * time.Millisecond) // connected + healthy, but no state set (unknown)

	st := brain.Tick()
	if st.Decision.Action != Adopt || brain.currentActiveID() != r.DNA.ID() {
		t.Fatalf("brain should adopt the only healthy route, got %+v", st.Decision)
	}
}

func TestAutopsyClassifiesLoss(t *testing.T) {
	now := time.Now()
	from := RouteView{ID: "a", Label: "yandex", Score: 30,
		Metrics: Metrics{Connected: true, LossRatio: 0.5, RTT: 200 * time.Millisecond, LastSeen: now}}
	to := RouteView{ID: "b", Label: "oneme", Score: 90, Metrics: Metrics{Connected: true}}
	a := BuildAutopsy(from, to, now)
	if a.Cause != "high packet loss" {
		t.Fatalf("expected loss classification, got %q", a.Cause)
	}
	if !a.NewStrategy || a.ToRoute != "b" {
		t.Fatalf("autopsy fields wrong: %+v", a)
	}
}
