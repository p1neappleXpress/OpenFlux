package fluxcore

import (
	"sort"
	"sync"
	"time"
)

// Registry holds every known route and answers the two questions FluxBrain
// asks constantly: "what's the state of the world?" and "which route should be
// active?". Phase 1 only observes; Phase 2 adds the switching on top.
type Registry struct {
	mu     sync.RWMutex
	routes map[string]*Route // keyed by DNA ID
	order  []string          // insertion order, for stable output
	now    func() time.Time
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return newRegistry(time.Now) }

func newRegistry(now func() time.Time) *Registry {
	return &Registry{routes: map[string]*Route{}, now: now}
}

// Add registers a route (idempotent by DNA). Returns the live route so the
// caller can attach a sampler-feeding adapter.
func (r *Registry) Add(dna RouteDNA) *Route {
	id := dna.ID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt, ok := r.routes[id]; ok {
		return rt
	}
	rt := newRoute(dna, r.now)
	r.routes[id] = rt
	r.order = append(r.order, id)
	return rt
}

// Get returns a route by DNA ID.
func (r *Registry) Get(id string) (*Route, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.routes[id]
	return rt, ok
}

// Best returns the healthiest route that is currently connected, or nil.
// This is the selector Phase 2 will act on; in Phase 1 it just reports.
func (r *Registry) Best() (*Route, RouteView, bool) {
	views := r.Snapshot()
	var best RouteView
	var bestRoute *Route
	found := false
	for _, v := range views {
		if !v.Metrics.Connected {
			continue
		}
		if !found || v.Score > best.Score {
			best, found = v, true
			if rt, ok := r.Get(v.ID); ok {
				bestRoute = rt
			}
		}
	}
	return bestRoute, best, found
}

// Snapshot ticks every route and returns their views, sorted best-first.
func (r *Registry) Snapshot() []RouteView {
	r.mu.RLock()
	routes := make([]*Route, 0, len(r.order))
	for _, id := range r.order {
		routes = append(routes, r.routes[id])
	}
	r.mu.RUnlock()

	views := make([]RouteView, 0, len(routes))
	for _, rt := range routes {
		views = append(views, rt.Tick())
	}
	sort.SliceStable(views, func(i, j int) bool {
		// Active first, then by score.
		if (views[i].State == "active") != (views[j].State == "active") {
			return views[i].State == "active"
		}
		return views[i].Score > views[j].Score
	})
	return views
}

// Pools groups route IDs by state, mirroring the Shadow Network view (§9).
func (r *Registry) Pools() map[string][]string {
	out := map[string][]string{
		"active": {}, "standby": {}, "unknown": {}, "quarantined": {},
	}
	for _, v := range r.Snapshot() {
		out[v.State] = append(out[v.State], v.ID)
	}
	return out
}

// Len returns the number of registered routes.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.routes)
}
