//go:build race

package cupsonline

// raceEnabled: the race detector slows the code 5-10x, so tests that
// measure throughput skip themselves under it.
const raceEnabled = true
