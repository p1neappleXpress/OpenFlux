package server

import (
	"sync"
	"time"
)

// limiter counts events per key in a sliding window, in memory. It is what
// stops password guessing and registration floods.
type limiter struct {
	mu   sync.Mutex
	now  func() time.Time
	hits map[string][]time.Time
	last time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, hits: map[string][]time.Time{}}
}

// count is how many events key had within window.
func (l *limiter) count(key string, window time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(window)
	return len(l.recent(key, window))
}

// add records one event.
func (l *limiter) add(key string, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gc(window)
	l.hits[key] = append(l.recent(key, window), l.now())
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

func (l *limiter) recent(key string, window time.Duration) []time.Time {
	cut := l.now().Add(-window)
	h := l.hits[key]
	i := 0
	for i < len(h) && h[i].Before(cut) {
		i++
	}
	return h[i:]
}

// gc drops stale keys now and then so the map cannot grow without bound.
func (l *limiter) gc(window time.Duration) {
	if l.now().Sub(l.last) < time.Minute {
		return
	}
	l.last = l.now()
	for k := range l.hits {
		if len(l.recent(k, window)) == 0 {
			delete(l.hits, k)
		}
	}
}
