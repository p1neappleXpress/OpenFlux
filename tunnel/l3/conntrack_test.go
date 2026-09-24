package l3

import (
	"testing"
	"time"
)

func TestConntrackSweepExpiresEstablishedAndClosingEntries(t *testing.T) {
	ct := newConntrack()
	defer ct.Close()

	established := flowKey{srcIP: 1, srcPort: 1}
	closing := flowKey{srcIP: 2, srcPort: 2}
	fresh := flowKey{srcIP: 3, srcPort: 3}

	ct.Insert(established)
	ct.Insert(closing)
	ct.Touch(closing, true)
	ct.Insert(fresh)

	// Force established/closing into the past without waiting real minutes.
	now := time.Now()
	for _, k := range []flowKey{established, closing} {
		s := ct.shardFor(k)
		s.mu.Lock()
		s.entries[k].lastSeen = now.Add(-ctTimeoutEstablished - time.Second)
		s.mu.Unlock()
	}

	ct.sweep()

	if _, ok := ct.get(established); ok {
		t.Error("established entry past its 5-minute timeout was not swept")
	}
	if _, ok := ct.get(closing); ok {
		t.Error("closing entry was not swept")
	}
	if _, ok := ct.get(fresh); !ok {
		t.Error("fresh entry was incorrectly swept")
	}
}

// Regression guard for a real bug this test suite caught during development:
// a naive shardIndex XORed k.srcPort<<16 into the hash without ever mixing
// those bits back down, so flows that differ only in source port -- the
// common case of one client making many connections to the same
// destination:port -- all landed in the same shard regardless of value,
// defeating the whole point of sharding for exactly that traffic pattern.
func TestShardIndexDistributesBySourcePortAlone(t *testing.T) {
	seen := make(map[uint32]bool)
	for port := uint16(0); port < 256; port++ {
		k := flowKey{srcIP: 1, dstIP: 2, dstPort: 443, srcPort: port}
		seen[shardIndex(k)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("256 srcPort-only variations landed in %d shard(s), want spread across multiple", len(seen))
	}
}

// Before sharding, sweep() held one global lock for the whole table scan,
// so every Insert/Touch/Exists on the packet-forwarding hot path blocked
// for the scan's entire duration, no matter which flow they touched. This
// verifies a sweep in progress on one shard doesn't block operations on a
// different shard.
func TestConntrackSweepDoesNotBlockOtherShards(t *testing.T) {
	ct := newConntrack()
	defer ct.Close()

	// Find two keys that land in different shards.
	var busy, other flowKey
	found := false
	for i := uint16(0); i < 1000 && !found; i++ {
		a := flowKey{srcPort: i}
		b := flowKey{srcPort: i + 1}
		if shardIndex(a) != shardIndex(b) {
			busy, other = a, b
			found = true
		}
	}
	if !found {
		t.Fatal("test setup: could not find two keys in different shards")
	}

	shard := ct.shardFor(busy)
	shard.mu.Lock() // simulate sweep holding this one shard's lock

	done := make(chan struct{})
	go func() {
		ct.Insert(other) // must not touch the locked shard
		close(done)
	}()

	select {
	case <-done:
		// good: Insert on the other shard was not blocked
	case <-time.After(500 * time.Millisecond):
		shard.mu.Unlock()
		t.Fatal("Insert on an unrelated shard blocked while a different shard's lock was held")
	}
	shard.mu.Unlock()
}
