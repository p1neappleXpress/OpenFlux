package fluxcore

import (
	"math"
	"sync"
	"time"
)

// Ewma is an exponentially weighted moving average.
// It is the cheapest possible "memory": recent samples matter more than old
// ones, with no ring buffer to size or allocate.
type Ewma struct {
	alpha float64
	value float64
	init  bool
}

// NewEwma builds an EWMA with the given smoothing factor (0<alpha<=1).
// Larger alpha = reacts faster / noisier. Out-of-range falls back to 0.2.
func NewEwma(alpha float64) *Ewma {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.2
	}
	return &Ewma{alpha: alpha}
}

// Update folds a new sample in and returns the new average.
func (e *Ewma) Update(x float64) float64 {
	if !e.init {
		e.value, e.init = x, true
		return e.value
	}
	e.value = e.alpha*x + (1-e.alpha)*e.value
	return e.value
}

// Value returns the current average (0 before the first sample).
func (e *Ewma) Value() float64 { return e.value }

// Ready reports whether at least one sample has been seen.
func (e *Ewma) Ready() bool { return e.init }

// Sampler turns a stream of raw observations (RTT samples, byte counts, probe
// results) into a smoothed Metrics snapshot. It is safe for concurrent use.
//
// A `now` func is injected so tests can drive time deterministically.
type Sampler struct {
	mu sync.Mutex

	rtt    *Ewma // seconds
	jitter *Ewma // seconds
	loss   *Ewma // 0..1
	tput   *Ewma // bytes/sec

	lastRTT float64
	haveRTT bool

	winStart time.Time
	winBytes int64

	lastSeen   time.Time
	connected  bool
	bytesSent  uint64
	bytesRecv  uint64
	reconnects uint64
	lastErr    string

	now func() time.Time
}

// NewSampler creates a Sampler with sane smoothing defaults.
func NewSampler() *Sampler { return newSampler(time.Now) }

func newSampler(now func() time.Time) *Sampler {
	return &Sampler{
		rtt:      NewEwma(0.25),
		jitter:   NewEwma(0.25),
		loss:     NewEwma(0.2),
		tput:     NewEwma(0.3),
		winStart: now(),
		now:      now,
	}
}

// ObserveRTT records a successful probe round-trip. It also updates jitter
// (mean deviation of RTT) and counts the probe as "not lost".
func (s *Sampler) ObserveRTT(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secs := d.Seconds()
	if s.haveRTT {
		s.jitter.Update(math.Abs(secs - s.lastRTT))
	}
	s.rtt.Update(secs)
	s.lastRTT, s.haveRTT = secs, true
	s.loss.Update(0)
	s.lastSeen = s.now()
	s.connected = true
}

// ObserveLoss records a probe that timed out or a dropped send (e.g. the
// Yandex writer queue overflowing). It pushes the loss ratio up.
func (s *Sampler) ObserveLoss() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loss.Update(1)
}

// ObserveBytes records application bytes moved (sent or received) and folds
// them into a smoothed bytes/sec throughput over ~1s windows.
func (s *Sampler) ObserveBytes(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.winBytes += int64(n)
	elapsed := s.now().Sub(s.winStart).Seconds()
	if elapsed >= 1.0 {
		s.tput.Update(float64(s.winBytes) / elapsed)
		s.winBytes = 0
		s.winStart = s.now()
	}
	s.lastSeen = s.now()
}

// SetConnected / RecordSend / RecordRecv / RecordReconnect / SetError mirror
// the counters the original transport already tracked, so the adapter can feed
// them straight in.
func (s *Sampler) SetConnected(v bool) { s.mu.Lock(); s.connected = v; s.mu.Unlock() }
func (s *Sampler) RecordSend(n int) {
	s.mu.Lock()
	s.bytesSent += uint64(n)
	s.mu.Unlock()
}
func (s *Sampler) RecordRecv(n int) {
	s.mu.Lock()
	s.bytesRecv += uint64(n)
	s.lastSeen = s.now()
	s.mu.Unlock()
}
func (s *Sampler) RecordReconnect() { s.mu.Lock(); s.reconnects++; s.mu.Unlock() }
func (s *Sampler) SetError(e string) { s.mu.Lock(); s.lastErr = e; s.mu.Unlock() }

// Snapshot builds an immutable Metrics from the current smoothed state.
func (s *Sampler) Snapshot() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Metrics{
		RTT:        time.Duration(s.rtt.Value() * float64(time.Second)),
		Jitter:     time.Duration(s.jitter.Value() * float64(time.Second)),
		LossRatio:  s.loss.Value(),
		Throughput: s.tput.Value(),
		LastSeen:   s.lastSeen,
		Connected:  s.connected,
		BytesSent:  s.bytesSent,
		BytesRecv:  s.bytesRecv,
		Reconnects: s.reconnects,
		LastError:  s.lastErr,
	}
}
