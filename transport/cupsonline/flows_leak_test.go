package cupsonline

import (
	"testing"
	"time"
)

// Before this fix, t.flows only ever grew: Send() added an entry per new
// TCP flow and nothing ever deleted one -- not on flow close, not in
// Stop(), not on any timer. Every distinct client connection an exit node
// running --transport=cupsonline saw over its lifetime stayed in this map
// forever, unlike tunnel/l3/conntrack.go's analogous table.
func TestSweepFlowsRemovesOnlyIdleEntries(t *testing.T) {
	ct := &CupsonlineTransport{flows: make(map[flowKey]*flowSender)}

	now := time.Now()
	stale := &flowSender{}
	stale.lastSeen.Store(now.Add(-flowIdleTimeout - time.Second).UnixNano())
	fresh := &flowSender{}
	fresh.lastSeen.Store(now.Add(-time.Second).UnixNano())

	staleKey := flowKey{srcPort: 1}
	freshKey := flowKey{srcPort: 2}
	ct.flows[staleKey] = stale
	ct.flows[freshKey] = fresh

	ct.sweepFlows(now)

	if _, ok := ct.flows[staleKey]; ok {
		t.Error("stale flow entry was not swept")
	}
	if _, ok := ct.flows[freshKey]; !ok {
		t.Error("fresh flow entry was incorrectly swept")
	}
	if len(ct.flows) != 1 {
		t.Errorf("len(flows) = %d, want 1", len(ct.flows))
	}
}

// Regression guard for the unbounded-growth shape of the bug: many flows
// that all go idle must all be reclaimed, not just leave the map size
// permanently elevated.
func TestSweepFlowsReclaimsManyIdleEntries(t *testing.T) {
	ct := &CupsonlineTransport{flows: make(map[flowKey]*flowSender)}
	now := time.Now()

	for i := 0; i < 500; i++ {
		fs := &flowSender{}
		fs.lastSeen.Store(now.Add(-flowIdleTimeout - time.Second).UnixNano())
		ct.flows[flowKey{srcPort: uint16(i)}] = fs
	}

	ct.sweepFlows(now)

	if len(ct.flows) != 0 {
		t.Errorf("len(flows) = %d after sweeping 500 idle entries, want 0", len(ct.flows))
	}
}
