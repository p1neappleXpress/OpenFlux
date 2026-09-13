package fluxcore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// BPM maps a 0..100 health score to the "heartbeat" metaphor of the Flux Heart
// screen (spec §14). Healthy = calm/slow; stressed = fast. It is a *metaphor*
// for system activity, never a medical figure — the spec is explicit about this.
func BPM(score float64) int {
	bpm := 60 + (100-score)*0.7 // 100 => 60, 0 => 130
	if bpm < 50 {
		bpm = 50
	}
	if bpm > 140 {
		bpm = 140
	}
	return int(bpm + 0.5)
}

// Snapshot is the whole system state the UI renders in one frame.
type Snapshot struct {
	Time     time.Time           `json:"time"`
	BPM      int                 `json:"bpm"`
	Status   string              `json:"status"` // "alive" | "stress" | "offline"
	Score    float64             `json:"score"`  // best route score
	Weather  Forecast            `json:"weather"`
	Pools    map[string][]string `json:"pools"`
	Routes   []RouteView         `json:"routes"`
	Events   []Event             `json:"events"`
	Stats    *BrainStats         `json:"stats,omitempty"`    // present when a brain is attached
	Profiles []DNAProfile        `json:"profiles,omitempty"` // Route DNA reputation from memory
}

// BuildSnapshot assembles the current state from a registry and event bus.
func BuildSnapshot(reg *Registry, bus *Bus) Snapshot {
	return buildSnapshot(reg, bus, nil)
}

// buildSnapshot is the full builder; brain may be nil.
func buildSnapshot(reg *Registry, bus *Bus, brain *Brain) Snapshot {
	routes := reg.Snapshot()

	// The headline (BPM, status, weather) reflects the ACTIVE route — the path
	// traffic actually uses — not the best route overall. A healthy standby in
	// reserve must not mask a struggling active path: that struggle is exactly
	// the "network under stress" moment the Flux Heart screen exists to show.
	// Only if nothing is active do we fall back to the best connected route.
	var head RouteView
	var score float64
	status := "offline"
	found := false
	for _, v := range routes {
		if v.State == "active" && v.Metrics.Connected {
			head, found = v, true
			break
		}
	}
	if !found {
		for _, v := range routes {
			if v.Metrics.Connected && (!found || v.Score > head.Score) {
				head, found = v, true
			}
		}
	}
	if found {
		score = head.Score
		if score >= 60 {
			status = "alive"
		} else {
			status = "stress"
		}
	}

	var hist []float64
	if found {
		if rt, ok := reg.Get(head.ID); ok {
			rt.mu.Lock()
			hist = append([]float64(nil), rt.history...)
			rt.mu.Unlock()
		}
	}

	var events []Event
	if bus != nil {
		events = bus.Recent()
	}

	snap := Snapshot{
		Time:    time.Now(),
		BPM:     BPM(score),
		Status:  status,
		Score:   round1(score),
		Weather: WeatherFrom(score, hist),
		Pools:   reg.Pools(),
		Routes:  routes,
		Events:  events,
	}
	if brain != nil {
		st := brain.Stats()
		snap.Stats = &st
		if brain.Memory() != nil {
			md := brain.Memory().Snapshot()
			for _, p := range md.Profiles {
				snap.Profiles = append(snap.Profiles, p)
			}
		}
	}
	return snap
}

// Server exposes the snapshot over HTTP for the local GUI: a JSON endpoint and
// a Server-Sent-Events stream. SSE keeps it dependency-free (no websocket lib)
// and is trivially consumed from a browser with EventSource.
type Server struct {
	reg   *Registry
	bus   *Bus
	brain *Brain // optional; enriches snapshots with stats + memory
	mux   *http.ServeMux
}

// NewServer wires the endpoints. Register your own static file handler on the
// returned mux (via Mux) to serve the UI from the same origin.
func NewServer(reg *Registry, bus *Bus) *Server {
	s := &Server{reg: reg, bus: bus, mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("/api/stream", s.handleStream)
	return s
}

// SetBrain attaches a brain so snapshots include lifetime stats and Route DNA
// reputation from memory.
func (s *Server) SetBrain(b *Brain) { s.brain = b }

func (s *Server) snapshot() Snapshot { return buildSnapshot(s.reg, s.bus, s.brain) }

// Mux exposes the router so the caller can add a UI file server.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Handler satisfies http.Handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(s.snapshot())
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	writeSnap := func() {
		b, _ := json.Marshal(s.snapshot())
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	writeSnap() // immediate first frame
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			writeSnap()
		}
	}
}
