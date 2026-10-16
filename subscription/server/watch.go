package server

import (
	"context"
	"sync"
	"time"
)

// nodeCheckEvery is the interval of the background pass that heals the
// autoNode pools whose channel left.
const nodeCheckEvery = 3 * time.Minute

// nodeRetryAfter is how long a failed creation waits before the next attempt.
const nodeRetryAfter = 3 * time.Minute

// nodeWatch makes sure a missing channel of an autoNode pool gets created,
// one attempt at a time, and not more often than one failure test per window.
// It never blocks an HTTP handler: the creation goes to a goroutine.
type nodeWatch struct {
	mu    sync.Mutex
	busy  map[string]bool      // channel names being created
	again map[string]time.Time // channel -> try again not before
}

func newNodeWatch() *nodeWatch {
	return &nodeWatch{busy: map[string]bool{}, again: map[string]time.Time{}}
}

// start asks for the requested channel to be created. It checks the channel on
// disk again under the lock, so a parallel or finished attempt is not doubled.
func (s *Server) start(r nodeRequest) {
	s.w.mu.Lock()
	if s.w.busy[r.Channel] {
		s.w.mu.Unlock()
		return
	}
	if t, ok := s.w.again[r.Channel]; ok && s.now().Before(t) {
		s.w.mu.Unlock()
		return
	}
	if _, err := s.links.link(r.Pool, r.Channel, nil); err == nil {
		s.w.mu.Unlock()
		return
	}
	s.w.busy[r.Channel] = true
	s.w.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := s.prov.Ensure(ctx, r)
		s.w.mu.Lock()
		defer s.w.mu.Unlock()
		s.w.busy[r.Channel] = false
		if err != nil {
			s.logf("pool %s: channel %s: autoNode: %v", r.Pool.ID, r.Channel, err)
			s.w.again[r.Channel] = s.now().Add(nodeRetryAfter)
			return
		}
		delete(s.w.again, r.Channel)
	}()
}

// startNodeWatch heals the autoNode pools at once, then every nodeCheckEvery,
// until ctx ends. Called from Serve.
func (s *Server) startNodeWatch(ctx context.Context) {
	s.ensureAll()
	go func() {
		ticker := time.NewTicker(nodeCheckEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.ensureAll()
			}
		}
	}()
}

func (s *Server) ensureAll() {
	for _, p := range s.cfg.Pools {
		if p.AutoNode && !p.PerUser {
			s.start(nodeRequest{Pool: p, Channel: p.Channel})
		}
	}
}
