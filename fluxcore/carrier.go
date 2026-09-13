package fluxcore

import (
	"errors"
	"sync"
	"time"
)

// Carrier is one live covert channel to an exit — the fluxcore-side contract a
// real OpenFlux transport (Yandex, MAX, …) satisfies through a thin adapter.
// It is deliberately the minimum the engine needs: move bytes, receive bytes,
// and honestly report whether it is up.
type Carrier interface {
	Name() string
	Start() error
	Stop() error
	Send([]byte) error       // satisfies Link; the engine frames data/pings before this
	SetInbound(func([]byte)) // the carrier calls this with each received frame
	Connected() bool
}

// Engine is the Multi-Transport Engine: it runs *several* carriers at once, each
// as its own Route with its own MetricsProbe, continuously probes all of them so
// standby routes have fresh (not stale) scores, lets the Brain pick the active
// one, and routes application traffic through whichever carrier is active.
//
// To the tunnel above it, an Engine looks like a single transport (Send +
// SetInbound); underneath it manages N and switches between them. One exit can
// be served by many carriers; many exits are just more routes.
type Engine struct {
	reg   *Registry
	bus   *Bus
	brain *Brain

	mu      sync.RWMutex
	routes  []*boundRoute
	byID    map[string]*boundRoute
	inbound func([]byte)

	pingEvery time.Duration
	pingTO    time.Duration
	stop      chan struct{}
	started   bool
}

type boundRoute struct {
	route   *Route
	carrier Carrier
	probe   *MetricsProbe
}

// NewEngine builds an engine over an existing registry/bus/brain.
func NewEngine(reg *Registry, bus *Bus, brain *Brain) *Engine {
	return &Engine{
		reg: reg, bus: bus, brain: brain,
		byID:      map[string]*boundRoute{},
		pingEvery: 700 * time.Millisecond,
		pingTO:    1800 * time.Millisecond,
		stop:      make(chan struct{}),
	}
}

// SetProbeTiming tunes how often each route is pinged and when an unanswered
// ping counts as loss.
func (e *Engine) SetProbeTiming(every, timeout time.Duration) {
	e.mu.Lock()
	e.pingEvery, e.pingTO = every, timeout
	e.mu.Unlock()
}

// Add binds a carrier to a Route (identified by its DNA) and wires the carrier's
// inbound through this route's MetricsProbe: pong frames become RTT samples,
// data frames flow up to the engine's inbound sink. Returns the Route so the
// caller can set its initial pool state.
func (e *Engine) Add(dna RouteDNA, c Carrier) *Route {
	rt := e.reg.Add(dna)
	p := NewMetricsProbe(c, rt.Sampler())
	br := &boundRoute{route: rt, carrier: c, probe: p}

	c.SetInbound(func(raw []byte) {
		payload, isData := p.Inbound(raw)
		if isData {
			e.mu.RLock()
			cb := e.inbound
			e.mu.RUnlock()
			if cb != nil {
				cb(payload)
			}
		}
	})

	e.mu.Lock()
	e.routes = append(e.routes, br)
	e.byID[dna.ID()] = br
	e.mu.Unlock()
	return rt
}

// SetInbound registers the sink for application data arriving on the active
// carrier (in the real client, this hands bytes to the gVisor tunnel).
func (e *Engine) SetInbound(cb func([]byte)) {
	e.mu.Lock()
	e.inbound = cb
	e.mu.Unlock()
}

// Start starts every carrier and launches the probe+decision loop.
func (e *Engine) Start() error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return nil
	}
	e.started = true
	routes := append([]*boundRoute(nil), e.routes...)
	e.mu.Unlock()

	for _, br := range routes {
		if err := br.carrier.Start(); err != nil {
			return err
		}
	}
	go e.loop()
	return nil
}

// Stop halts the loop and stops every carrier.
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.started {
		e.mu.Unlock()
		return
	}
	e.started = false
	close(e.stop)
	routes := append([]*boundRoute(nil), e.routes...)
	e.mu.Unlock()
	for _, br := range routes {
		_ = br.carrier.Stop()
	}
}

// Send routes application data through the currently active carrier. This is
// the method the tunnel calls; the engine hides which physical transport is
// carrying the traffic right now.
func (e *Engine) Send(data []byte) error {
	id := e.brain.ActiveID()
	e.mu.RLock()
	br := e.byID[id]
	e.mu.RUnlock()
	if br == nil {
		return errors.New("flux: no active route")
	}
	return br.probe.SendData(data)
}

// Connected reports whether there is a usable active carrier.
func (e *Engine) Connected() bool {
	id := e.brain.ActiveID()
	e.mu.RLock()
	br := e.byID[id]
	e.mu.RUnlock()
	return br != nil && br.carrier.Connected()
}

// Tick runs one probe+decision cycle by hand (used in tests). The running
// engine calls this on its own ticker.
func (e *Engine) Tick() {
	e.mu.RLock()
	routes := append([]*boundRoute(nil), e.routes...)
	to := e.pingTO
	e.mu.RUnlock()

	for _, br := range routes {
		br.route.Sampler().SetConnected(br.carrier.Connected())
		if br.carrier.Connected() {
			_ = br.probe.Ping()
		}
		br.probe.SweepTimeouts(to)
	}
	e.brain.Tick()
}

func (e *Engine) loop() {
	e.mu.RLock()
	every := e.pingEvery
	e.mu.RUnlock()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.Tick()
		}
	}
}
