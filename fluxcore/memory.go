package fluxcore

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// DNAProfile is what the brain remembers about one route across its whole life
// (spec §31, §30 "technical memory"): not a single reading, but an aggregate
// reputation it can consult next time it sees the same Route DNA.
type DNAProfile struct {
	DNA         RouteDNA  `json:"dna"`
	Samples     int64     `json:"samples"`
	AvgScore    float64   `json:"avgScore"`
	BestScore   float64   `json:"bestScore"`
	WorstScore  float64   `json:"worstScore"`
	Failovers   int64     `json:"failovers"`   // times we switched AWAY from this route
	Adoptions   int64     `json:"adoptions"`   // times this route became active
	Quarantines int64     `json:"quarantines"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
}

// Seed is a validated strategy worth keeping and replaying (spec §33, §74).
type Seed struct {
	ID         string    `json:"id"`
	Topic      string    `json:"topic"`
	DNA        RouteDNA  `json:"dna"`
	Confidence float64   `json:"confidence"`
	Evidence   int64     `json:"evidence"` // number of confirming observations
	Created    time.Time `json:"created"`
}

// MemoryData is the on-disk shape.
type MemoryData struct {
	Profiles map[string]DNAProfile `json:"profiles"` // keyed by DNA ID
	Events   []Event               `json:"events"`   // recent episodic memory
	Seeds    []Seed                `json:"seeds"`
	Updated  time.Time             `json:"updated"`
}

// Memory is a small, file-backed store. It is deliberately simple (one JSON
// file) — durable enough to carry experience across restarts (spec §75) without
// pulling in a database.
type Memory struct {
	mu        sync.Mutex
	path      string
	data      MemoryData
	maxEvents int
	now       func() time.Time
}

// OpenMemory loads memory from path, or starts fresh if it does not exist.
func OpenMemory(path string) (*Memory, error) {
	return openMemory(path, time.Now)
}

func openMemory(path string, now func() time.Time) (*Memory, error) {
	m := &Memory{
		path:      path,
		data:      MemoryData{Profiles: map[string]DNAProfile{}},
		maxEvents: 500,
		now:       now,
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil // fresh memory is not an error
		}
		return m, err
	}
	if err := json.Unmarshal(b, &m.data); err != nil {
		return m, err
	}
	if m.data.Profiles == nil {
		m.data.Profiles = map[string]DNAProfile{}
	}
	return m, nil
}

// RecordRoute folds one route observation into that DNA's lifetime profile.
func (m *Memory) RecordRoute(v RouteView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := v.ID
	p, ok := m.data.Profiles[id]
	if !ok {
		p = DNAProfile{DNA: v.DNA, FirstSeen: m.now(), BestScore: v.Score, WorstScore: v.Score}
	}
	// Running average without storing every sample.
	p.AvgScore = (p.AvgScore*float64(p.Samples) + v.Score) / float64(p.Samples+1)
	p.Samples++
	if v.Score > p.BestScore {
		p.BestScore = v.Score
	}
	if v.Score < p.WorstScore || p.Samples == 1 {
		p.WorstScore = v.Score
	}
	p.LastSeen = m.now()
	m.data.Profiles[id] = p
}

// NoteDecision updates the counters that make a route's reputation (how often it
// was adopted, failed over from, quarantined).
func (m *Memory) NoteDecision(d Decision) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch d.Action {
	case Failover:
		if d.From != "" {
			p := m.data.Profiles[d.From]
			p.Failovers++
			m.data.Profiles[d.From] = p
		}
		fallthrough
	case Adopt:
		if d.To != "" {
			p := m.data.Profiles[d.To]
			p.Adoptions++
			m.data.Profiles[d.To] = p
		}
	case Quarantine:
		if d.To != "" {
			p := m.data.Profiles[d.To]
			p.Quarantines++
			m.data.Profiles[d.To] = p
		}
	}
}

// RecordEvent appends to episodic memory, bounded.
func (m *Memory) RecordEvent(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data.Events = append(m.data.Events, e)
	if len(m.data.Events) > m.maxEvents {
		m.data.Events = m.data.Events[len(m.data.Events)-m.maxEvents:]
	}
}

// Profile returns the stored profile for a DNA ID.
func (m *Memory) Profile(id string) (DNAProfile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.data.Profiles[id]
	return p, ok
}

// Snapshot returns a copy of the whole store (for the API / UI).
func (m *Memory) Snapshot() MemoryData {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := MemoryData{
		Profiles: make(map[string]DNAProfile, len(m.data.Profiles)),
		Events:   append([]Event(nil), m.data.Events...),
		Seeds:    append([]Seed(nil), m.data.Seeds...),
		Updated:  m.data.Updated,
	}
	for k, v := range m.data.Profiles {
		out.Profiles[k] = v
	}
	return out
}

// Save writes memory to disk atomically (temp file + rename), so a crash mid-
// write never corrupts the store (spec §76 "recover signed state" in spirit).
func (m *Memory) Save() error {
	m.mu.Lock()
	m.data.Updated = m.now()
	b, err := json.MarshalIndent(m.data, "", "  ")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if m.path == "" {
		return nil
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
