package fluxcore

import (
	"sync"
	"time"
)

// RouteState is where a route sits in the Shadow Network (spec §9).
type RouteState int

const (
	StateUnknown     RouteState = iota // discovered, not yet measured
	StateStandby                       // healthy, held in reserve, probed periodically
	StateActive                        // currently carrying traffic
	StateQuarantined                   // misbehaving, pulled from the pool (spec §38)
)

func (s RouteState) String() string {
	switch s {
	case StateStandby:
		return "standby"
	case StateActive:
		return "active"
	case StateQuarantined:
		return "quarantined"
	default:
		return "unknown"
	}
}

// Route is one path plus its live measurements and short score history.
type Route struct {
	mu sync.Mutex

	DNA     RouteDNA
	state   RouteState
	sampler *Sampler
	history []float64 // recent scores, for trend / degradation prediction
	maxHist int

	createdAt time.Time
	now       func() time.Time
}

// NewRoute creates a route in the Unknown pool.
func NewRoute(dna RouteDNA) *Route { return newRoute(dna, time.Now) }

func newRoute(dna RouteDNA, now func() time.Time) *Route {
	return &Route{
		DNA:       dna,
		state:     StateUnknown,
		sampler:   newSampler(now),
		maxHist:   64,
		createdAt: now(),
		now:       now,
	}
}

// Sampler exposes the route's sampler so the adapter can feed observations.
func (r *Route) Sampler() *Sampler { return r.sampler }

// State / SetState guard the pool membership.
func (r *Route) State() RouteState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}
func (r *Route) SetState(s RouteState) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

// Tick samples the current health, appends it to history and returns the view.
// The brain calls this on a fixed cadence; UI reads the returned view.
func (r *Route) Tick() RouteView {
	m := r.sampler.Snapshot()
	score, bd := HealthScore(m, r.now())

	r.mu.Lock()
	r.history = append(r.history, score)
	if len(r.history) > r.maxHist {
		r.history = r.history[len(r.history)-r.maxHist:]
	}
	hist := append([]float64(nil), r.history...)
	state := r.state
	r.mu.Unlock()

	return RouteView{
		ID:          r.DNA.ID(),
		Label:       r.DNA.Label(),
		DNA:         r.DNA,
		State:       state.String(),
		Score:       round1(score),
		Breakdown:   bd,
		Metrics:     m,
		Degradation: round2(DegradationProbability(hist)),
	}
}

// RouteView is the immutable, serialisable snapshot the API and UI consume.
type RouteView struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	DNA         RouteDNA  `json:"dna"`
	State       string    `json:"state"`
	Score       float64   `json:"score"`       // 0..100
	Breakdown   Breakdown `json:"breakdown"`   // why the score is what it is
	Metrics     Metrics   `json:"metrics"`     // raw smoothed telemetry
	Degradation float64   `json:"degradation"` // 0..1 projected risk
}

func round1(x float64) float64 { return float64(int(x*10+0.5)) / 10 }
func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }
