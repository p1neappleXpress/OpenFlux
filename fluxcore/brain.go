package fluxcore

import (
	"time"
)

// Brain is FluxBrain: it ties the registry, selector, event bus and memory
// together into one decision loop. The runtime calls Tick on a fixed cadence;
// Tick observes, decides, applies, remembers, and reports — the OBSERVE →
// ANALYZE → DECIDE → LEARN cycle of spec §19, minus the sandbox experiments
// that come in Phase 4.
type Brain struct {
	reg *Registry
	bus *Bus
	sel *Selector
	mem *Memory // optional; may be nil

	lastSwitch time.Time
	startTime  time.Time
	now        func() time.Time

	// Lifetime counters (spec §18 statistics).
	failovers   int64
	adoptions   int64
	recoveries  int64
	quarantines int64
}

// BrainStats is the aggregate the Statistics screen shows.
type BrainStats struct {
	UptimeSec   int64 `json:"uptimeSec"`
	Failovers   int64 `json:"failovers"`
	Adoptions   int64 `json:"adoptions"`
	Recoveries  int64 `json:"recoveries"`
	Quarantines int64 `json:"quarantines"`
	RoutesKnown int   `json:"routesKnown"`
}

// Stats returns the lifetime counters.
func (b *Brain) Stats() BrainStats {
	return BrainStats{
		UptimeSec:   int64(b.now().Sub(b.startTime).Seconds()),
		Failovers:   b.failovers,
		Adoptions:   b.adoptions,
		Recoveries:  b.recoveries,
		Quarantines: b.quarantines,
		RoutesKnown: b.reg.Len(),
	}
}

// NewBrain wires a brain. `mem` may be nil to run without persistence.
func NewBrain(reg *Registry, bus *Bus, sel *Selector, mem *Memory) *Brain {
	return newBrain(reg, bus, sel, mem, time.Now)
}

func newBrain(reg *Registry, bus *Bus, sel *Selector, mem *Memory, now func() time.Time) *Brain {
	if sel == nil {
		sel = DefaultSelector()
	}
	return &Brain{reg: reg, bus: bus, sel: sel, mem: mem, now: now, startTime: now()}
}

// BrainState is what one Tick decided, for the UI and logs.
type BrainState struct {
	ActiveID string     `json:"activeId"`
	Decision Decision   `json:"decision"`
	Sweeps   []Decision `json:"sweeps,omitempty"`
	Autopsy  *Autopsy   `json:"autopsy,omitempty"`
}

// Tick runs one full decision cycle and returns what it did.
func (b *Brain) Tick() BrainState {
	now := b.now()
	views := b.reg.Snapshot()

	dec := b.sel.Decide(views, b.lastSwitch, now)
	st := BrainState{Decision: dec}

	switch dec.Action {
	case Adopt:
		b.setState(dec.To, StateActive)
		b.lastSwitch = now
		b.adoptions++
		b.emit(Event{Kind: EvFailoverDone, Route: dec.To,
			Message: "Flux selected a path: " + label(views, dec.To)})
		b.note(dec)

	case Failover:
		// Bracket the switch with the product's human voice (spec §99).
		b.emit(Event{Kind: EvFailoverStart, Route: dec.From, Message: Human(EvFailoverStart)})
		b.setState(dec.From, StateStandby)
		b.setState(dec.To, StateActive)
		b.lastSwitch = now
		b.failovers++
		b.emit(Event{Kind: EvFailoverDone, Route: dec.To,
			Message: Human(EvFailoverDone) + " (" + label(views, dec.To) + ")"})
		// Write the postmortem and file the lesson.
		if from, okF := viewByID(views, dec.From); okF {
			if to, okT := viewByID(views, dec.To); okT {
				a := BuildAutopsy(from, to, now)
				st.Autopsy = &a
			}
		}
		b.note(dec)
	}

	// Immune sweep: quarantine dead routes, recover healed ones.
	sweeps := b.sel.PoolSweep(views, now)
	for _, s := range sweeps {
		switch s.Action {
		case Quarantine:
			b.setState(s.To, StateQuarantined)
			b.quarantines++
			b.emit(Event{Kind: EvGuardianVeto, Route: s.To, Message: "Quarantined: " + s.Reason})
		case Recover:
			b.setState(s.To, StateStandby)
			b.recoveries++
			b.emit(Event{Kind: EvRouteRecovered, Route: s.To, Message: "Recovered: " + s.Reason})
		}
		b.note(s)
	}
	st.Sweeps = sweeps

	// Learn: fold this tick's observations into long-term memory.
	if b.mem != nil {
		for _, v := range views {
			b.mem.RecordRoute(v)
		}
	}

	st.ActiveID = b.currentActiveID()
	return st
}

// ActiveID returns the DNA ID of the currently active route ("" if none).
func (b *Brain) ActiveID() string { return b.currentActiveID() }

// Memory exposes the store so the runtime can Save() it periodically.
func (b *Brain) Memory() *Memory { return b.mem }

// LastSwitch reports when the active route last changed.
func (b *Brain) LastSwitch() time.Time { return b.lastSwitch }

// --- internals ---

func (b *Brain) setState(id string, s RouteState) {
	if rt, ok := b.reg.Get(id); ok {
		rt.SetState(s)
	}
}

func (b *Brain) emit(e Event) {
	if b.bus != nil {
		b.bus.Publish(e)
	}
	if b.mem != nil {
		b.mem.RecordEvent(e)
	}
}

func (b *Brain) note(d Decision) {
	if b.mem != nil {
		b.mem.NoteDecision(d)
	}
}

func (b *Brain) currentActiveID() string {
	for _, v := range b.reg.Snapshot() {
		if v.State == "active" {
			return v.ID
		}
	}
	return ""
}

func label(views []RouteView, id string) string {
	if v, ok := viewByID(views, id); ok {
		return v.Label
	}
	return id
}

func viewByID(views []RouteView, id string) (RouteView, bool) {
	for _, v := range views {
		if v.ID == id {
			return v, true
		}
	}
	return RouteView{}, false
}
