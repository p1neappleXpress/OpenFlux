package l3

import (
	"sync"
	"time"
)

const (
	ctTimeoutEstablished = 5 * time.Minute
	ctTimeoutClosing     = 15 * time.Second
	ctSweepInterval      = 30 * time.Second

	// ctShardCount splits the conntrack table so sweep() only ever holds one
	// shard's lock at a time. Before sharding, sweep held a single global
	// lock across a full-table scan, blocking every Insert/Touch/Exists call
	// on the packet-forwarding hot path for the whole scan duration.
	ctShardCount = 32
)

type ctEntry struct {
	lastSeen time.Time
	dying    bool
}

type ctShard struct {
	mu      sync.RWMutex
	entries map[flowKey]*ctEntry
}

type conntrack struct {
	shards  [ctShardCount]*ctShard
	stop    chan struct{}
	stopped sync.Once
}

func newConntrack() *conntrack {
	ct := &conntrack{
		stop: make(chan struct{}),
	}
	for i := range ct.shards {
		ct.shards[i] = &ctShard{entries: make(map[flowKey]*ctEntry, 64)}
	}
	go ct.sweepLoop()
	return ct
}

// shardIndex picks a shard for k. It doesn't need to be cryptographically
// strong, just spread real traffic evenly -- in particular, many flows from
// one client to the same destination:port differ only in the ephemeral
// source port, so the finalizer below must mix that into the low bits that
// %ctShardCount reads (a plain XOR of the raw fields does not: srcPort's
// bits alone would land above bit 15 and never move the shard index).
func shardIndex(k flowKey) uint32 {
	h := k.srcIP*2654435761 ^ k.dstIP*40503 ^ uint32(k.srcPort)<<16 ^ uint32(k.dstPort) ^ uint32(k.proto)
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	return h % ctShardCount
}

func (c *conntrack) shardFor(k flowKey) *ctShard {
	return c.shards[shardIndex(k)]
}

func (c *conntrack) Insert(k flowKey) {
	s := c.shardFor(k)
	now := time.Now()
	s.mu.Lock()
	if e, ok := s.entries[k]; ok {
		e.lastSeen = now
	} else {
		s.entries[k] = &ctEntry{lastSeen: now}
	}
	s.mu.Unlock()
}

func (c *conntrack) Touch(k flowKey, dying bool) {
	s := c.shardFor(k)
	s.mu.Lock()
	if e, ok := s.entries[k]; ok {
		e.lastSeen = time.Now()
		if dying {
			e.dying = true
		}
	}
	s.mu.Unlock()
}

func (c *conntrack) Exists(k flowKey) bool {
	s := c.shardFor(k)
	s.mu.RLock()
	_, ok := s.entries[k]
	s.mu.RUnlock()
	return ok
}

// get is a test/introspection helper; production code has no need to read
// an entry back out (Exists/Touch cover every real use).
func (c *conntrack) get(k flowKey) (ctEntry, bool) {
	s := c.shardFor(k)
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[k]
	if !ok {
		return ctEntry{}, false
	}
	return *e, true
}

func (c *conntrack) Close() {
	c.stopped.Do(func() {
		close(c.stop)
	})
}

func (c *conntrack) sweepLoop() {
	t := time.NewTicker(ctSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.sweep()
		}
	}
}

// sweep expires stale entries one shard at a time, so it never holds more
// than one shard's lock at once -- packet handling on every other shard
// keeps flowing while a sweep is in progress.
func (c *conntrack) sweep() {
	now := time.Now()
	for _, s := range c.shards {
		s.mu.Lock()
		for k, e := range s.entries {
			timeout := ctTimeoutEstablished
			if e.dying {
				timeout = ctTimeoutClosing
			}
			if now.Sub(e.lastSeen) > timeout {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}
