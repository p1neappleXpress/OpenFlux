package fluxcore

import (
	"sync"
	"time"
)

// EventKind classifies what happened. These map directly to the human-facing
// language in the spec (§15, §99): the UI turns them into calm sentences.
type EventKind string

const (
	EvRouteDiscovered EventKind = "route.discovered"
	EvRouteDegrading  EventKind = "route.degrading"
	EvRouteRecovered  EventKind = "route.recovered"
	EvFailoverStart   EventKind = "failover.start"
	EvFailoverDone    EventKind = "failover.done"
	EvExperiment      EventKind = "experiment"
	EvGuardianVeto    EventKind = "guardian.veto"
	EvInfo            EventKind = "info"
)

// Event is one structured thing that happened. Structured — not a log line —
// because Memory, Autopsy and the UI all consume the same stream (spec §29).
type Event struct {
	Time    time.Time         `json:"time"`
	Kind    EventKind         `json:"kind"`
	Route   string            `json:"route,omitempty"` // DNA ID, if route-scoped
	Message string            `json:"message"`         // human-friendly
	Fields  map[string]string `json:"fields,omitempty"`
}

// Human turns a kind into the product's voice (§99): never "Connection failed",
// always "Flux is finding another path…".
func Human(kind EventKind) string {
	switch kind {
	case EvRouteDegrading:
		return "A path is getting weaker. Flux is watching it."
	case EvFailoverStart:
		return "The path disappeared. Flux is finding another one…"
	case EvFailoverDone:
		return "Flux found another way."
	case EvRouteRecovered:
		return "Connection stable."
	case EvRouteDiscovered:
		return "Flux discovered a new path."
	default:
		return string(kind)
	}
}

// Bus is a tiny in-process pub/sub with a bounded replay buffer, so a UI that
// connects late still sees recent history.
type Bus struct {
	mu       sync.RWMutex
	subs     map[int]chan Event
	nextID   int
	recent   []Event
	maxReplay int
}

// NewBus creates a bus that keeps the last `replay` events for late joiners.
func NewBus(replay int) *Bus {
	if replay <= 0 {
		replay = 100
	}
	return &Bus{subs: map[int]chan Event{}, maxReplay: replay}
}

// Publish sends an event to all subscribers and records it for replay.
// Slow subscribers are skipped (non-blocking) rather than stalling the brain.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	b.recent = append(b.recent, e)
	if len(b.recent) > b.maxReplay {
		b.recent = b.recent[len(b.recent)-b.maxReplay:]
	}
	subs := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default: // drop for a subscriber that can't keep up
		}
	}
}

// Subscribe returns a channel of future events plus an unsubscribe func.
func (b *Bus) Subscribe(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = 32
	}
	ch := make(chan Event, buf)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// Recent returns a copy of the replay buffer (oldest first).
func (b *Bus) Recent() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Event(nil), b.recent...)
}
