// Command fluxdemo runs the full Flux GUI over the real Multi-Transport Engine,
// driven by *simulated* carriers, so you can open the real interface and watch
// FluxBrain measure several transports at once and pick the best — no gVisor, no
// VPS, no root.
//
//	cd fluxcore && go run ./cmd/fluxdemo
//	open http://127.0.0.1:8787
//
// Engine, Sampler, MetricsProbe, HealthScore, Selector, Brain, Memory, Control
// API and the embedded UI are the exact code the real client uses. Only the
// carriers are simulated (SimCarrier) — and even they are driven through the
// real probe path, so the RTT/loss you see are measured, not injected.
package main

import (
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"fluxcore"
	"fluxcore/ui"
)

const addr = "127.0.0.1:8787"

type carrierInfo struct {
	dna     fluxcore.RouteDNA
	carrier *fluxcore.SimCarrier
}

func main() {
	reg := fluxcore.NewRegistry()
	bus := fluxcore.NewBus(80)
	mem, _ := fluxcore.OpenMemory(filepath.Join(os.TempDir(), "fluxdemo-memory.json"))
	brain := fluxcore.NewBrain(reg, bus, fluxcore.DefaultSelector(), mem)
	eng := fluxcore.NewEngine(reg, bus, brain)

	// Three transports, one Turkish exit (spec: one EXIT served by many carriers).
	defs := []struct {
		transport string
		rtt       time.Duration
	}{
		{"yandex", 60 * time.Millisecond},
		{"max", 45 * time.Millisecond},
		{"direct", 30 * time.Millisecond},
	}
	var carriers []carrierInfo
	for i, d := range defs {
		dna := fluxcore.RouteDNA{Transport: d.transport, Exit: "tr"}
		sim := fluxcore.NewSimCarrier(d.transport, d.rtt)
		rt := eng.Add(dna, sim)
		if i == 0 {
			rt.SetState(fluxcore.StateActive)
		} else {
			rt.SetState(fluxcore.StateStandby)
		}
		carriers = append(carriers, carrierInfo{dna, sim})
		bus.Publish(fluxcore.Event{Kind: fluxcore.EvRouteDiscovered, Route: dna.ID(),
			Message: "Flux discovered a path: " + dna.Label()})
	}

	eng.SetInbound(func([]byte) {}) // demo has no tunnel to hand data to
	if err := eng.Start(); err != nil {
		log.Fatal(err)
	}
	go conditions(brain, carriers, mem)

	srv := fluxcore.NewServer(reg, bus)
	srv.SetBrain(brain)
	srv.Mux().Handle("/", ui.Handler())

	log.Printf("Flux GUI demo (Multi-Transport) -> http://%s  (Ctrl-C to stop)", addr)
	log.Fatal(http.ListenAndServe(addr, srv.Handler()))
}

// conditions choreographs the simulated weather so the demo is alive: it storms
// whichever carrier is currently active (forcing a failover), and occasionally
// takes a standby fully offline for a while (forcing a quarantine, then a
// recovery). Everything the Brain does in response is real.
func conditions(brain *fluxcore.Brain, carriers []carrierInfo, mem *fluxcore.Memory) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	byID := map[string]*fluxcore.SimCarrier{}
	for _, c := range carriers {
		byID[c.dna.ID()] = c.carrier
	}

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()

	var storming *fluxcore.SimCarrier
	stormLeft := 0
	var downed *fluxcore.SimCarrier
	downLeft := 0
	cooldown := 8
	sinceSave := 0

	for range tick.C {
		sinceSave++
		if sinceSave >= 10 {
			_ = mem.Save()
			sinceSave = 0
		}

		// Storm scheduler: hit the active carrier.
		if stormLeft > 0 {
			stormLeft--
			if stormLeft == 0 && storming != nil {
				storming.SetStorm(false)
				storming = nil
				cooldown = 12 + rng.Intn(8)
			}
		} else if cooldown > 0 {
			cooldown--
		} else {
			if c := byID[brain.ActiveID()]; c != nil {
				storming = c
				c.SetStorm(true)
				stormLeft = 6
			}
		}

		// Outage scheduler: rarely, drop a standby entirely.
		if downLeft > 0 {
			downLeft--
			if downLeft == 0 && downed != nil {
				downed.SetDown(false)
				downed = nil
			}
		} else if rng.Float64() < 0.03 {
			// pick a non-active carrier
			for _, c := range carriers {
				if c.dna.ID() != brain.ActiveID() && c.carrier != storming {
					downed = c.carrier
					c.carrier.SetDown(true)
					downLeft = 25 + rng.Intn(15) // long enough to be quarantined then recovered
					break
				}
			}
		}
	}
}
