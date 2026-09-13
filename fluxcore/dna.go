package fluxcore

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// RouteDNA is the identity of a route: the combination of carrier, relay, exit
// and configuration that together produce one path to the internet (spec §31).
// Two routes with the same DNA are the same path; the brain accumulates a
// performance profile per DNA over time.
type RouteDNA struct {
	Transport string            `json:"transport"` // "yandex" | "oneme" | ...
	Relay     string            `json:"relay"`     // relay identifier (optional)
	Exit      string            `json:"exit"`      // exit node identifier (optional)
	Params    map[string]string `json:"params,omitempty"`
}

// ID returns a short, stable identifier for this DNA. Order-independent over
// Params so the same route always hashes the same.
func (d RouteDNA) ID() string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%s|%s|", d.Transport, d.Relay, d.Exit)
	keys := make([]string, 0, len(d.Params))
	for k := range d.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s;", k, d.Params[k])
	}
	return fmt.Sprintf("%08x", h.Sum32())
}

// Label is a human-friendly name for UI and logs, e.g. "yandex→exit-fra".
func (d RouteDNA) Label() string {
	parts := []string{d.Transport}
	if d.Relay != "" {
		parts = append(parts, d.Relay)
	}
	if d.Exit != "" {
		parts = append(parts, d.Exit)
	}
	return strings.Join(parts, "→")
}
