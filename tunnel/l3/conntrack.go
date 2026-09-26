package l3

import (
	"sync"
	"time"
)

const (
	ctTimeoutEstablished = 5 * time.Minute
	ctTimeoutClosing     = 15 * time.Second
	ctTimeoutUDP         = 2 * time.Minute
	ctTimeoutDNS         = 15 * time.Second
	ctSweepInterval      = 30 * time.Second
	ctMaxEntries         = 65536
)

type ctEntry struct {
	lastSeen time.Time
	dying    bool
}

type conntrack struct {
	mu      sync.RWMutex
	entries map[flowKey]*ctEntry
	stop    chan struct{}
	stopped sync.Once
}

func newConntrack() *conntrack {
	ct := &conntrack{
		entries: make(map[flowKey]*ctEntry, 1024),
		stop:    make(chan struct{}),
	}
	go ct.sweepLoop()
	return ct
}

func (c *conntrack) Insert(k flowKey) bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.stop:
		return false
	default:
	}
	if e, ok := c.entries[k]; ok {
		e.lastSeen = now
	} else {
		if len(c.entries) >= ctMaxEntries {
			return false
		}
		c.entries[k] = &ctEntry{lastSeen: now}
	}
	return true
}

func (c *conntrack) Touch(k flowKey, dying bool) {
	c.mu.Lock()
	if e, ok := c.entries[k]; ok {
		e.lastSeen = time.Now()
		if dying {
			e.dying = true
		}
	}
	c.mu.Unlock()
}

func (c *conntrack) Exists(k flowKey) bool {
	c.mu.RLock()
	e, ok := c.entries[k]
	if ok {
		ok = time.Since(e.lastSeen) <= flowTimeout(k, e)
	}
	c.mu.RUnlock()
	return ok
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

func (c *conntrack) sweep() {
	now := time.Now()
	c.mu.Lock()
	for k, e := range c.entries {
		timeout := flowTimeout(k, e)
		if now.Sub(e.lastSeen) > timeout {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

func flowTimeout(k flowKey, e *ctEntry) time.Duration {
	if k.proto == 17 {
		if k.srcPort == 53 || k.dstPort == 53 {
			return ctTimeoutDNS
		}
		return ctTimeoutUDP
	}
	if e.dying {
		return ctTimeoutClosing
	}
	return ctTimeoutEstablished
}
