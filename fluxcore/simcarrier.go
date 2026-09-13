package fluxcore

import (
	"math/rand"
	"sync"
	"time"
)

// SimCarrier is a synthetic covert channel with an echoing exit, used by the
// demo and tests (spec §88 synthetic users). It behaves like a real carrier —
// the MetricsProbe drives it exactly the same way — but instead of a network it
// simulates one: it echoes ping frames as pongs after a tunable round-trip, and
// drops a fraction of frames to model loss. In "storm" mode latency and loss
// spike, which is what forces the Brain to fail over.
//
// It is NOT a stand-in for measurement logic: the RTT and loss the probe records
// come from real timing of these echoes, not from injected numbers.
type SimCarrier struct {
	name string

	mu        sync.Mutex
	inbound   func([]byte)
	connected bool
	storm     bool
	down      bool

	baseRTT time.Duration
	jitter  time.Duration
	loss    float64

	rng  *rand.Rand
	wg   sync.WaitGroup
	stop chan struct{}
}

// NewSimCarrier creates a carrier with a calm baseline RTT and light loss.
func NewSimCarrier(name string, baseRTT time.Duration) *SimCarrier {
	return &SimCarrier{
		name:    name,
		baseRTT: baseRTT,
		jitter:  baseRTT / 4,
		loss:    0.01,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(len(name)))),
		stop:    make(chan struct{}),
	}
}

func (s *SimCarrier) Name() string { return s.name }

func (s *SimCarrier) Start() error {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	return nil
}

func (s *SimCarrier) Stop() error {
	s.mu.Lock()
	s.connected = false
	s.mu.Unlock()
	s.wg.Wait() // let pending echoes finish
	return nil
}

func (s *SimCarrier) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected && !s.down
}

func (s *SimCarrier) SetInbound(cb func([]byte)) {
	s.mu.Lock()
	s.inbound = cb
	s.mu.Unlock()
}

// SetStorm toggles the degraded regime (high latency + heavy loss).
func (s *SimCarrier) SetStorm(v bool) { s.mu.Lock(); s.storm = v; s.mu.Unlock() }

// SetDown simulates a full carrier outage (nothing gets through, Connected=false).
func (s *SimCarrier) SetDown(v bool) { s.mu.Lock(); s.down = v; s.mu.Unlock() }

// Send models the covert channel + the exit. Real data frames are "sent" (the
// exit would forward them to the internet); ping frames are echoed back as
// pongs after the simulated round-trip so the probe can time them.
func (s *SimCarrier) Send(frame []byte) error {
	s.mu.Lock()
	if s.down || !s.connected {
		s.mu.Unlock()
		return nil // silently dropped; unanswered pings become loss on sweep
	}
	loss, rtt := s.loss, s.baseRTT+jitterOf(s.rng, s.jitter)
	if s.storm {
		loss = 0.6
		rtt = 340*time.Millisecond + jitterOf(s.rng, 220*time.Millisecond)
	}
	drop := s.rng.Float64() < loss
	isPing := len(frame) >= 5 && frame[0] == framePing
	cb := s.inbound
	s.mu.Unlock()

	if drop || !isPing || cb == nil {
		return nil
	}
	pong := make([]byte, 5)
	pong[0] = framePong
	copy(pong[1:], frame[1:5])

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(rtt)
		defer timer.Stop()
		select {
		case <-timer.C:
			s.mu.Lock()
			live := s.connected && !s.down
			inbound := s.inbound
			s.mu.Unlock()
			if live && inbound != nil {
				inbound(pong)
			}
		case <-s.stop:
		}
	}()
	return nil
}

func jitterOf(rng *rand.Rand, j time.Duration) time.Duration {
	if j <= 0 {
		return 0
	}
	return time.Duration(rng.Int63n(int64(j)))
}
