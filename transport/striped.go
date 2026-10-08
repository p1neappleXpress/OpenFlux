package transport

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// StripedTransport spreads frames over several child transports (lanes) and
// puts them back in order on the receiving side.
//
// Every frame gets a 5-byte header [stripeMagic][seq u32]. Send hands the frame
// to the connected lane with the shortest queue, so a slower lane simply gets
// fewer frames. The receiver keeps a reorder buffer and delivers strictly in
// seq order, so the tunnel above (and the TCP connections inside it) never see
// the lanes' different delays as reordering. A hole that stays open longer
// than the adaptive gap timeout is declared lost and skipped - TCP then sees
// an ordinary loss and retransmits, instead of a stalled stream.
type StripedTransport struct {
	lanesMu  sync.RWMutex
	lanes    []Transport
	readyAt  []time.Time
	src      LaneSource
	maxLanes int
	scale    bool
	started  bool
	accepted atomic.Uint64

	capMu   sync.Mutex
	capBps  float64 // 0 = no cap
	tokens  float64
	tokenAt time.Time
	seq     atomic.Uint32
	dropped atomic.Uint64
	rr      atomic.Uint32

	cbMu sync.RWMutex
	cb   func([]byte)

	ro *reorderBuffer

	laneSent, laneRecv [64]atomic.Uint64

	watchMu sync.Mutex
	watched map[string]bool
	watchAt time.Time

	stopOnce sync.Once
	done     chan struct{}
}

// LossReporter is implemented by lanes that know their recent uplink loss
// fraction (0..1); autoscale uses it to find the link's ceiling.
type LossReporter interface {
	UplinkLoss() float64
	UplinkReceived() uint64 // cumulative packets the far end acknowledged
}

// QueueDepther is implemented by lanes that can report how much they still
// have buffered for sending; StripedTransport prefers the emptiest lane.
type QueueDepther interface {
	QueueDepth() int
}

const stripeMagic = 0xA7
const stripeHeader = 5

// stripeCtlMagic marks a control frame: JSON {"watch":[lane ids]} - the lanes
// the sender of the frame is subscribed to. The far side then sends only on
// those, so no frame goes to a lane nobody is watching.
const stripeCtlMagic = 0xA8

// WatchSource reports the far-side lane IDs this node is subscribed to.
type WatchSource interface {
	Watching() []string
}

// LaneIDer is a lane with an identity the far side can name in a watch list.
type LaneIDer interface {
	LaneID() string
}

// stripeMaxDepth is the lane queue depth (fragments) above which Send drops
// instead of queueing more latency. STRIPE_MAX_DEPTH overrides.
var stripeMaxDepth = func() int {
	if n, err := strconv.Atoi(os.Getenv("STRIPE_MAX_DEPTH")); err == nil && n > 0 {
		return n
	}
	return 256
}()

// LaneSource creates additional lanes on demand and may ask for one itself
// (e.g. a receiver that sees a new peer lane nobody is watching yet).
type LaneSource interface {
	NewLane() Transport
	SetGrowHook(func())
}

// NewStripedTransport bonds lanes into one transport. Both ends must stripe.
func NewStripedTransport(lanes []Transport) *StripedTransport {
	return NewStripedTransportAuto(lanes, nil, len(lanes), false)
}

// NewStripedTransportAuto is NewStripedTransport plus growth: src supplies new
// lanes up to maxLanes; with autoscale the sender adds a lane whenever all
// lanes stay backed up and stops once a new lane no longer adds throughput.
// Lanes are never removed.
func NewStripedTransportAuto(lanes []Transport, src LaneSource, maxLanes int, autoscale bool) *StripedTransport {
	s := &StripedTransport{lanes: lanes, readyAt: make([]time.Time, len(lanes)), src: src,
		maxLanes: maxLanes, scale: autoscale, done: make(chan struct{})}
	s.ro = newReorderBuffer(s.deliver)
	if src != nil {
		src.SetGrowHook(func() { go s.addLane("peer lane appeared") })
	}
	return s
}

// laneWarmup keeps a fresh lane out of the send rotation until the far side
// has had time to find and subscribe to it.
const laneWarmup = 8 * time.Second

func (s *StripedTransport) addLane(why string) {
	if s.src == nil {
		return
	}
	s.lanesMu.Lock()
	if len(s.lanes) >= s.maxLanes {
		s.lanesMu.Unlock()
		return
	}
	l := s.src.NewLane()
	idx := len(s.lanes)
	s.lanes = append(s.lanes, l)
	s.readyAt = append(s.readyAt, time.Now().Add(laneWarmup))
	n := len(s.lanes)
	s.lanesMu.Unlock()
	l.Receive(func(b []byte) { s.onLane(idx, b) })
	utils.Infof("[STRIPE] +lane %d (%s), now %d lanes", idx, why, n)
	if err := l.Start(); err != nil {
		utils.Infof("[STRIPE] lane %d start: %v", idx, err)
	}
}

func (s *StripedTransport) snapshotLanes() ([]Transport, []time.Time) {
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()
	return s.lanes, s.readyAt
}

func (s *StripedTransport) Start() error {
	lanes, _ := s.snapshotLanes()
	for i, l := range lanes {
		lane := i
		l.Receive(func(b []byte) { s.onLane(lane, b) })
	}
	var firstErr error
	started := 0
	for i, l := range lanes {
		if err := l.Start(); err != nil {
			utils.Debugf("[STRIPE] lane %d start: %v", i, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		started++
	}
	if started == 0 {
		return fmt.Errorf("striped: no lane started: %w", firstErr)
	}
	go s.ro.run(s.done)
	go s.statsLoop()
	if _, ok := s.src.(WatchSource); ok {
		go s.watchLoop()
	}
	if s.scale {
		go s.autoscaleLoop()
	}
	return nil
}

func (s *StripedTransport) Stop() error {
	s.stopOnce.Do(func() { close(s.done) })
	var wg sync.WaitGroup
	lanes, _ := s.snapshotLanes()
	for _, l := range lanes {
		wg.Add(1)
		go func(l Transport) { defer wg.Done(); _ = l.Stop() }(l)
	}
	wg.Wait()
	return nil
}

func (s *StripedTransport) Send(data []byte) error {
	lane, depth := s.pickLane()
	if lane == nil {
		return fmt.Errorf("striped: no connected lane")
	}
	if !s.takeTokens(len(data)) {
		s.dropped.Add(1)
		return nil
	}
	if depth >= stripeMaxDepth {
		// Every lane is backed up. Drop here, before a seq is spent, so the
		// receiver sees no hole to wait on - TCP just sees an ordinary loss.
		s.dropped.Add(1)
		return nil
	}
	buf := make([]byte, stripeHeader+len(data))
	buf[0] = stripeMagic
	binary.BigEndian.PutUint32(buf[1:5], s.seq.Add(1))
	copy(buf[stripeHeader:], data)
	s.accepted.Add(uint64(len(data)))
	if i := s.laneIndex(lane); i >= 0 && i < 64 {
		s.laneSent[i].Add(1)
	}
	return lane.Send(buf)
}

// pickLane returns the connected lane with the shortest send queue; ties (and
// lanes that cannot report a depth) rotate round-robin.
func (s *StripedTransport) pickLane() (Transport, int) {
	lanes, readyAt := s.snapshotLanes()
	n := len(lanes)
	if n == 0 {
		return nil, 0
	}
	now := time.Now()
	s.watchMu.Lock()
	watched := s.watched
	if now.Sub(s.watchAt) > 5*time.Second {
		watched = nil // stale or never heard: fall back to the warmup rule
	}
	s.watchMu.Unlock()
	start := int(s.rr.Add(1)) % n
	var best Transport
	bestDepth := int(^uint(0) >> 1)
	for k := 0; k < n; k++ {
		i := (start + k) % n
		l := lanes[i]
		if !l.IsConnected() {
			continue
		}
		if watched != nil {
			if id, ok := l.(LaneIDer); !ok || !watched[id.LaneID()] {
				continue
			}
		} else if now.Before(readyAt[i]) {
			continue
		}
		d := 0
		if q, ok := l.(QueueDepther); ok {
			d = q.QueueDepth()
		}
		if d < bestDepth {
			best, bestDepth = l, d
		}
	}
	return best, bestDepth
}

func (s *StripedTransport) Receive(cb func([]byte)) {
	s.cbMu.Lock()
	s.cb = cb
	s.cbMu.Unlock()
}

func (s *StripedTransport) deliver(b []byte) {
	s.cbMu.RLock()
	cb := s.cb
	s.cbMu.RUnlock()
	if cb != nil {
		cb(b)
	}
}

func (s *StripedTransport) onLane(lane int, b []byte) {
	if len(b) > 1 && b[0] == stripeCtlMagic {
		var m struct {
			Watch []string `json:"watch"`
		}
		if json.Unmarshal(b[1:], &m) == nil {
			w := make(map[string]bool, len(m.Watch))
			for _, id := range m.Watch {
				w[id] = true
			}
			s.watchMu.Lock()
			s.watched, s.watchAt = w, time.Now()
			s.watchMu.Unlock()
		}
		return
	}
	if len(b) < stripeHeader || b[0] != stripeMagic {
		// Lane-level filler (keepalives) carries no stripe header; hand it up
		// untouched - the codec layer decodes it to nothing.
		s.deliver(b)
		return
	}
	if lane < 64 {
		s.laneRecv[lane].Add(1)
	}
	s.ro.push(binary.BigEndian.Uint32(b[1:5]), b[stripeHeader:])
}

func (s *StripedTransport) IsConnected() bool {
	lanes, _ := s.snapshotLanes()
	for _, l := range lanes {
		if l.IsConnected() {
			return true
		}
	}
	return false
}

func (s *StripedTransport) Stats() TransportStats {
	var st TransportStats
	lanes, _ := s.snapshotLanes()
	for _, l := range lanes {
		ls := l.Stats()
		st.BytesSent += ls.BytesSent
		st.BytesReceived += ls.BytesReceived
		st.PacketsSent += ls.PacketsSent
		st.PacketsRecv += ls.PacketsRecv
		st.Reconnects += ls.Reconnects
		if ls.Uptime > st.Uptime {
			st.Uptime = ls.Uptime
		}
	}
	st.Connected = s.IsConnected()
	return st
}

func (s *StripedTransport) laneIndex(l Transport) int {
	lanes, _ := s.snapshotLanes()
	for i, x := range lanes {
		if x == l {
			return i
		}
	}
	return -1
}

// watchLoop tells the far side, every second and on every lane, which of its
// lanes we are subscribed to.
func (s *StripedTransport) watchLoop() {
	ws := s.src.(WatchSource)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		ids := ws.Watching()
		if len(ids) == 0 {
			continue
		}
		body, _ := json.Marshal(map[string][]string{"watch": ids})
		frame := append([]byte{stripeCtlMagic}, body...)
		lanes, _ := s.snapshotLanes()
		for _, l := range lanes {
			if l.IsConnected() {
				_ = l.Send(frame)
			}
		}
	}
}

func (s *StripedTransport) statsLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		up := 0
		lanes, _ := s.snapshotLanes()
		for _, l := range lanes {
			if l.IsConnected() {
				up++
			}
		}
		d, sk, late, buffered, to := s.ro.snapshot()
		loss, _ := s.uplinkLoss()
		s.watchMu.Lock()
		nw := len(s.watched)
		if time.Since(s.watchAt) > 5*time.Second {
			nw = -1
		}
		s.watchMu.Unlock()
		var per []string
		for i := range lanes {
			if i >= 64 {
				break
			}
			id := ""
			if li, ok := lanes[i].(LaneIDer); ok && len(li.LaneID()) >= 4 {
				id = li.LaneID()[:4]
			}
			per = append(per, fmt.Sprintf("%d:%s s%d/r%d", i, id, s.laneSent[i].Load(), s.laneRecv[i].Load()))
		}
		utils.Infof("[STRIPE] per-lane %s", strings.Join(per, " "))
		utils.Infof("[STRIPE] farWatches=%d lanes up=%d/%d delivered=%d skipped=%d late=%d buffered=%d gapTimeout=%s sendDropped=%d uplinkLoss=%.1f%% cap=%.0fKB/s",
			nw, up, len(lanes), d, sk, late, buffered, to, s.dropped.Load(), loss*100, s.getCap()/1000)
	}
}

// takeTokens enforces the autoscaler's rate cap (bytes/s) before a frame gets
// a seq, so capping never leaves holes for the receiver to wait on.
func (s *StripedTransport) takeTokens(n int) bool {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.capBps <= 0 {
		return true
	}
	now := time.Now()
	s.tokens += now.Sub(s.tokenAt).Seconds() * s.capBps
	s.tokenAt = now
	if burst := s.capBps / 10; s.tokens > burst {
		s.tokens = burst
	}
	if s.tokens < float64(n) {
		return false
	}
	s.tokens -= float64(n)
	return true
}

func (s *StripedTransport) setCap(bps float64) {
	s.capMu.Lock()
	s.capBps = bps
	s.tokenAt = time.Now()
	s.capMu.Unlock()
}

func (s *StripedTransport) getCap() float64 {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	return s.capBps
}

// uplinkLoss averages the lanes' reported uplink loss (ready lanes only).
func (s *StripedTransport) uplinkLoss() (float64, bool) {
	lanes, readyAt := s.snapshotLanes()
	now := time.Now()
	sum, n := 0.0, 0
	for i, l := range lanes {
		lr, ok := l.(LossReporter)
		if !ok || !l.IsConnected() || now.Before(readyAt[i]) {
			continue
		}
		sum += lr.UplinkLoss()
		n++
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// uplinkReceived sums what the lanes' far ends report receiving (packets).
func (s *StripedTransport) uplinkReceived() uint64 {
	lanes, _ := s.snapshotLanes()
	var n uint64
	for _, l := range lanes {
		if lr, ok := l.(LossReporter); ok {
			n += lr.UplinkReceived()
		}
	}
	return n
}

// autoscaleLoop grows the lane count while every ready lane stays backed up,
// and runs an AIMD rate cap on the lanes' uplink loss: a new lane that brings
// loss instead of throughput marks the link's ceiling and the cap holds the
// rate just under it. Lanes are never removed; extra ones just carry less.
func (s *StripedTransport) autoscaleLoop() {
	const (
		busySecs     = 3
		settle       = 15 * time.Second
		minGain      = 1.10
		probeLoss    = 0.02
		holdAfterCap = 15 * time.Second
		bytesPerPkt  = 1340.0 // payload per lane packet reaching the far end
		highLoss     = 0.05
		reprobeAge   = 3 * time.Minute
	)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var hist, ghist [3]uint64
	var last, glast uint64
	busy, lossy, clean := 0, 0, 0
	probing := false
	var probeAt, ceilingAt time.Time
	var rateBefore, gotBefore, lossSum, ceilRate float64
	cleanSince := time.Now()
	var holdUntil time.Time
	lossN := 0
	for k := 0; ; k++ {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		cur := s.accepted.Load()
		hist[k%3] = cur - last
		last = cur
		rate := float64(hist[0]+hist[1]+hist[2]) / 3
		g := s.uplinkReceived()
		if g >= glast {
			ghist[k%3] = g - glast
		}
		glast = g
		got := float64(ghist[0]+ghist[1]+ghist[2]) / 3 // packets/s the far end really received
		loss, haveLoss := s.uplinkLoss()

		_, depth := s.pickLane()
		lanes, _ := s.snapshotLanes()
		if depth >= stripeMaxDepth/2 {
			busy++
		} else {
			busy = 0
		}

		// AIMD on uplink loss.
		if haveLoss && loss > highLoss {
			lossy++
			clean = 0
		} else if haveLoss && loss < 0.02 {
			clean++
			lossy = 0
		} else {
			cleanSince = time.Now()
		}
		holding := time.Now().Before(holdUntil)
		if lossy >= 2 && !probing && !holding {
			// What actually got through right now is the link's rate.
			ceilRate = got * bytesPerPkt
			if ceilRate <= 0 || ceilRate > rate {
				ceilRate = rate
			}
			c := ceilRate * 0.95
			s.setCap(c)
			lossy, cleanSince, holdUntil = 0, time.Now(), time.Now().Add(holdAfterCap)
			utils.Infof("[STRIPE] uplink loss %.1f%%, far end %.0f KB/s -> cap %.0f KB/s", loss*100, ceilRate/1000, c/1000)
		}
		if cp := s.getCap(); clean >= 10 && cp > 0 {
			clean = 0
			if time.Since(cleanSince) >= 30*time.Second {
				ceilRate *= 1.05 // probe above the last known ceiling
				cleanSince = time.Now()
			}
			next := cp * 1.03
			if lim := ceilRate * 0.98; ceilRate > 0 && next > lim {
				next = lim
			}
			if next > cp {
				s.setCap(next)
			}
		}

		if probing {
			if time.Since(probeAt) >= settle-5*time.Second {
				lossSum += loss
				lossN++
			}
			if time.Since(probeAt) >= settle {
				probing = false
				avg := lossSum / float64(max(lossN, 1))
				gain := got / max(gotBefore, 1)
				if avg > probeLoss || gain < minGain {
					ceilingAt = time.Now()
					ceilRate = gotBefore * bytesPerPkt
					if ceilRate <= 0 || ceilRate > rateBefore {
						ceilRate = rateBefore
					}
					cleanSince, holdUntil = time.Now(), time.Now().Add(holdAfterCap)
					s.setCap(ceilRate)
					utils.Infof("[STRIPE] ceiling: lane %d gave x%.2f at far end (%.0f -> %.0f pkt/s), loss %.1f%%; cap %.0f KB/s on %d lanes",
						len(lanes)-1, gain, gotBefore, got, avg*100, ceilRate/1000, len(lanes))
				} else {
					utils.Infof("[STRIPE] lane paid off: far end %.0f -> %.0f pkt/s (x%.2f), loss %.1f%%",
						gotBefore, got, gain, avg*100)
				}
			}
		}
		atCeiling := !ceilingAt.IsZero() && time.Since(ceilingAt) < reprobeAge
		if busy >= busySecs && !probing && !atCeiling && s.getCap() == 0 && len(lanes) < s.maxLanes {
			rateBefore, gotBefore = rate, got
			probing, probeAt, busy, lossSum, lossN = true, time.Now(), 0, 0, 0
			s.addLane(fmt.Sprintf("overloaded at %.0f KB/s, uplink loss %.1f%%", rate/1000, loss*100))
		}
		if atCeiling == false && !ceilingAt.IsZero() && s.getCap() > 0 {
			// Re-probe: lift the cap and let the next overload try a lane.
			ceilingAt = time.Time{}
			s.setCap(0)
		}
	}
}

// reorderBuffer delivers frames in seq order. Frames sit in a ring indexed by
// seq; a hole is given up as lost once the frame right after it has waited
// longer than the gap timeout, so a run of holes behind stale frames clears in
// one pass instead of costing one timeout each.
type reorderBuffer struct {
	mu      sync.Mutex
	started bool
	next    uint32
	ring    [reorderWindow]reorderSlot
	count   int
	est     time.Duration // smoothed upper estimate of how long holes take to fill
	out     chan []byte

	delivered, skipped, late uint64
}

type reorderSlot struct {
	seq  uint32
	data []byte
	at   time.Time
	ok   bool
}

const (
	reorderWindow     = 16384
	reorderMinTimeout = 25 * time.Millisecond
	reorderMaxTimeout = 400 * time.Millisecond
)

func newReorderBuffer(deliver func([]byte)) *reorderBuffer {
	r := &reorderBuffer{est: 40 * time.Millisecond, out: make(chan []byte, 8192)}
	go func() {
		for b := range r.out {
			deliver(b)
		}
	}()
	return r
}

func seqBefore(a, b uint32) bool { return int32(a-b) < 0 }

func (r *reorderBuffer) timeout() time.Duration {
	to := 2*r.est + 10*time.Millisecond
	if to < reorderMinTimeout {
		to = reorderMinTimeout
	}
	if to > reorderMaxTimeout {
		to = reorderMaxTimeout
	}
	return to
}

func (r *reorderBuffer) push(seq uint32, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.started {
		r.started = true
		r.next = seq
	}
	if seqBefore(seq, r.next) {
		r.late++
		return
	}
	// Too far ahead for the window: give up on the oldest holes.
	for seq-r.next >= reorderWindow {
		r.skipToLocked(r.lowestLocked(seq))
	}
	if seq == r.next && r.count > 0 {
		// A hole just filled: learn how long lane skew keeps holes open.
		if nx := &r.ring[(seq+1)%reorderWindow]; nx.ok && nx.seq == seq+1 {
			sample := now.Sub(nx.at)
			if sample > r.est {
				r.est = (r.est + 3*sample) / 4
			} else {
				r.est = (r.est*63 + sample) / 64
			}
		}
	}
	sl := &r.ring[seq%reorderWindow]
	if sl.ok && sl.seq == seq {
		return // duplicate
	}
	*sl = reorderSlot{seq: seq, data: data, at: now, ok: true}
	r.count++
	r.drainLocked()
}

func (r *reorderBuffer) drainLocked() {
	for r.count > 0 {
		sl := &r.ring[r.next%reorderWindow]
		if !sl.ok || sl.seq != r.next {
			return
		}
		b := sl.data
		*sl = reorderSlot{}
		r.count--
		r.next++
		r.delivered++
		r.out <- b
	}
}

// lowestLocked returns the lowest buffered seq at or after next, or limit if
// nothing is buffered before it.
func (r *reorderBuffer) lowestLocked(limit uint32) uint32 {
	for s := r.next; seqBefore(s, limit); s++ {
		if sl := &r.ring[s%reorderWindow]; sl.ok && sl.seq == s {
			return s
		}
	}
	return limit
}

func (r *reorderBuffer) skipToLocked(s uint32) {
	if seqBefore(r.next, s) {
		r.skipped += uint64(s - r.next)
		r.next = s
	}
	r.drainLocked()
}

func (r *reorderBuffer) run(done chan struct{}) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		r.mu.Lock()
		to := r.timeout()
		for r.count > 0 {
			low := r.lowestLocked(r.next + reorderWindow)
			if time.Since(r.ring[low%reorderWindow].at) <= to {
				break
			}
			r.skipToLocked(low)
		}
		r.mu.Unlock()
	}
}

func (r *reorderBuffer) snapshot() (delivered, skipped, late uint64, buffered int, to time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.delivered, r.skipped, r.late, r.count, r.timeout()
}
