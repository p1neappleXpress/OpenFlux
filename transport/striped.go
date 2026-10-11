package transport

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// StripedTransport spreads frames over several child transports (lanes), puts
// them back in order on the receiving side and resends what the carrier
// loses, so the tunnel above it sees one ordered, lossless link.
//
// The Telemost SFU drops 10-30% of what it relays, in bursts. TCP inside the
// tunnel cannot live with that: each loss it sees cuts its window, and a lost
// retransmission costs it a timeout. So the stripe is a reliable link layer:
//
//   - Frames are cut into pieces that fit one lane packet, each with its own
//     seq. Lanes pull pieces on their own send clock (FramePuller): the ack
//     first, then retransmissions, then new data. A piece is stamped with the
//     lane and that lane's transmission count at the moment it really leaves.
//   - The receiver delivers strictly in seq order and, every ackInterval while
//     data flows, acks: the lowest seq it still misses, the highest it has
//     and the missing runs in between. Acks are cumulative, so a lost ack
//     costs only the wait for the next one.
//   - The sender declares a piece lost as soon as a piece that left after it
//     on the same lane is acked (a lane is FIFO), or once it is older than the
//     link's retransmission timeout, and resends it ahead of new data, on
//     another lane if there is one. Tick room the data leaves unused carries
//     early second copies of overdue pieces; a lane whose packets stop
//     arriving altogether only gets a probe now and then.
//   - New data waits in one queue shared by all lanes, managed by CoDel: when
//     the lanes cannot keep up, a frame is dropped at the head now and then -
//     the congestion signal TCP expects - instead of a deep queue that only
//     adds latency.
type StripedTransport struct {
	lanesMu  sync.RWMutex
	lanes    []Transport
	readyAt  []time.Time
	src      LaneSource
	maxLanes int
	scale    bool
	accepted atomic.Uint64

	capMu   sync.Mutex
	capBps  float64 // 0 = no cap
	tokens  float64
	tokenAt time.Time
	dropped atomic.Uint64

	cbMu sync.RWMutex
	cb   func([]byte)

	ro *reorderBuffer

	laneSent, laneRecv [maxStripeLanes]atomic.Uint64

	watchMu sync.Mutex
	watched map[string]bool
	watchAt time.Time

	snd sender
	asm []byte // frame being reassembled (delivery goroutine only)

	ackSent, ackRecv atomic.Uint64

	pumpMu sync.Mutex
	pumps  []chan struct{} // wake-ups of the lanes that cannot pull

	stopOnce sync.Once
	done     chan struct{}
}

const maxStripeLanes = 64

// FramePuller is a lane whose send clock takes frames from a source on every
// tick (up to max of them) instead of draining a queue filled by Send.
type FramePuller interface {
	SetFrameSource(src func(max int) [][]byte)
}

// LossReporter is implemented by lanes that know their recent uplink loss
// fraction (0..1); autoscale uses it to find the link's ceiling.
type LossReporter interface {
	UplinkLoss() float64
	UplinkReceived() uint64 // cumulative packets the far end acknowledged
}

// Wire formats. The first byte tells them apart from each other and from the
// lanes' own filler, which carries no stripe header at all.
//
//	data  [0xA7][seq u32][low u32][flag u8][piece]
//	ctl   [0xA8][JSON {"watch":[lane ids]}]
//	ack   [0xAE][cum u32][hi u32][n u8][(start u32, count u16) * n]
//
// low is the lowest seq the sender still waits to hear about: a receiver that
// starts listening mid-stream begins there, not at whatever arrives first. An
// ack says: every seq below cum has arrived (or was given up on), and of
// cum..hi everything has arrived except the n missing runs.
const (
	stripeMagic    = 0xA7
	stripeCtlMagic = 0xA8
	stripeAckMagic = 0xAE
	stripeHeader   = 9
	ackHeader      = 10
	ackMaxRuns     = 100
)

// Piece flags: the first byte of every data payload.
const (
	pieceCont = 1 // continues the previous piece
	pieceMore = 2 // another piece follows
)

func envIntOr(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func envMs(name string, def int) time.Duration {
	return time.Duration(envIntOr(name, def)) * time.Millisecond
}

var (
	// stripePieceBytes is the most a piece carries: one lane fragment
	// (TELEMOST_FRAG_BYTES minus its 8-byte header) minus the stripe header
	// and flag, so every piece is exactly one lane packet.
	stripePieceBytes = envIntOr("STRIPE_PIECE_BYTES", envIntOr("TELEMOST_FRAG_BYTES", 1312)-8-stripeHeader-1)

	// ackInterval is how often the receiver acks while data flows.
	ackInterval = envMs("STRIPE_ACK_MS", 10)

	// holeDeadline is how long a hole may hold later frames back before it is
	// given up on. Retransmission normally fills it in one or two round trips,
	// but the SFU now and then stops forwarding every lane for a few seconds
	// (it renegotiates the subscriptions when someone joins or leaves); the
	// tunnel's TCP waits that out (see tcpMinRTO) and loses nothing.
	holeDeadline = envMs("STRIPE_HOLE_MS", 5000)

	// CoDel on the shared send queue: when the oldest frame has waited longer
	// than codelTarget for a whole codelInterval, drop at the head, faster
	// while it lasts. The interval is about the tunnel's TCP round trip.
	codelTarget   = envMs("STRIPE_CODEL_TARGET_MS", 30)
	codelInterval = envMs("STRIPE_CODEL_INTERVAL_MS", 250)

	// sendQueueLimit bounds the send queue (bytes) whatever CoDel decides.
	sendQueueLimit = envIntOr("STRIPE_QUEUE_BYTES", 4<<20)
)

// laneReorderSlack: a piece counts as lost once a piece that left more than
// this many transmissions after it on the same lane has been acked. A lane is
// FIFO end to end (one RTP stream), so any later piece will do.
const laneReorderSlack = 0

// LaneSource creates additional lanes on demand and may ask for one itself
// (e.g. a receiver that sees a new peer lane nobody is watching yet).
type LaneSource interface {
	NewLane() Transport
	SetGrowHook(func())
}

// WatchSource reports the far-side lane IDs this node is subscribed to.
type WatchSource interface {
	Watching() []string
}

// LaneIDer is a lane with an identity the far side can name in a watch list.
type LaneIDer interface {
	LaneID() string
}

// NewStripedTransport bonds lanes into one transport. Both ends must stripe.
func NewStripedTransport(lanes []Transport) *StripedTransport {
	return NewStripedTransportAuto(lanes, nil, len(lanes), false)
}

// NewStripedTransportAuto is NewStripedTransport plus growth: src supplies new
// lanes up to maxLanes; with autoscale the sender adds a lane whenever the
// lanes stay backed up and stops once a new lane no longer adds throughput.
// Lanes are never removed.
func NewStripedTransportAuto(lanes []Transport, src LaneSource, maxLanes int, autoscale bool) *StripedTransport {
	if maxLanes > maxStripeLanes {
		maxLanes = maxStripeLanes
	}
	s := &StripedTransport{lanes: lanes, readyAt: make([]time.Time, len(lanes)), src: src,
		maxLanes: maxLanes, scale: autoscale, done: make(chan struct{})}
	s.snd.init()
	s.ro = newReorderBuffer(s.deliverPiece)
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
	s.attach(idx, l)
	utils.Infof("[STRIPE] +lane %d (%s), now %d lanes", idx, why, n)
	if err := l.Start(); err != nil {
		utils.Infof("[STRIPE] lane %d start: %v", idx, err)
	}
}

// attach wires lane idx: its receive side, and its send side - a pull source
// when the lane has a send clock of its own, a pump goroutine otherwise.
func (s *StripedTransport) attach(idx int, l Transport) {
	l.Receive(func(b []byte) { s.onLane(idx, b) })
	if p, ok := l.(FramePuller); ok {
		p.SetFrameSource(func(max int) [][]byte { return s.pull(idx, max) })
		return
	}
	wake := make(chan struct{}, 1)
	s.pumpMu.Lock()
	s.pumps = append(s.pumps, wake)
	s.pumpMu.Unlock()
	go s.pump(idx, l, wake)
}

// pump feeds a lane that has no send clock: whatever is ready goes out at once.
func (s *StripedTransport) pump(idx int, l Transport, wake chan struct{}) {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-wake:
		case <-t.C:
		}
		for {
			frames := s.pull(idx, 64)
			for _, f := range frames {
				_ = l.Send(f)
			}
			if len(frames) < 64 {
				break
			}
		}
	}
}

func (s *StripedTransport) wakePumps() {
	s.pumpMu.Lock()
	for _, w := range s.pumps {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	s.pumpMu.Unlock()
}

func (s *StripedTransport) snapshotLanes() ([]Transport, []time.Time) {
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()
	return s.lanes, s.readyAt
}

func (s *StripedTransport) Start() error {
	lanes, _ := s.snapshotLanes()
	for i, l := range lanes {
		s.attach(i, l)
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
	go s.ackLoop()
	go s.lossLoop()
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

// Send queues a frame for the lanes. It never blocks: a full queue drops, and
// TCP above sees an ordinary loss.
func (s *StripedTransport) Send(data []byte) error {
	if !s.IsConnected() {
		return fmt.Errorf("striped: no connected lane")
	}
	if len(data) == 0 {
		return nil
	}
	if !s.takeTokens(len(data)) {
		s.dropped.Add(1)
		return nil
	}
	if !s.snd.enqueue(data) {
		s.dropped.Add(1)
		return nil
	}
	s.accepted.Add(uint64(len(data)))
	s.wakePumps()
	return nil
}

// eligible reports whether lane i may carry stripe traffic now: connected,
// and watched by the far side (or, before the far side has said what it
// watches, past its warmup).
func (s *StripedTransport) eligible(i int, now time.Time) bool {
	lanes, readyAt := s.snapshotLanes()
	if i >= len(lanes) || !lanes[i].IsConnected() {
		return false
	}
	s.watchMu.Lock()
	watched := s.watched
	if now.Sub(s.watchAt) > 5*time.Second {
		watched = nil // stale or never heard: fall back to the warmup rule
	}
	s.watchMu.Unlock()
	if watched != nil {
		id, ok := lanes[i].(LaneIDer)
		return ok && watched[id.LaneID()]
	}
	return !now.Before(readyAt[i])
}

// pull is lane i's send clock asking for up to max frames.
func (s *StripedTransport) pull(i int, max int) [][]byte {
	if i >= maxStripeLanes || max <= 0 {
		return nil
	}
	now := time.Now()
	if !s.eligible(i, now) {
		return nil
	}
	out := s.snd.pull(i, max, now)
	if n := len(out); n > 0 {
		s.laneSent[i].Add(uint64(n))
	}
	return out
}

func (s *StripedTransport) Receive(cb func([]byte)) {
	s.cbMu.Lock()
	s.cb = cb
	s.cbMu.Unlock()
}

// deliverPiece runs on the reorder buffer's single delivery goroutine: it puts
// the pieces of a frame back together. gap says frames were skipped just
// before this one, which voids a half-assembled frame.
func (s *StripedTransport) deliverPiece(b []byte, gap bool) {
	if len(b) == 0 {
		return
	}
	flag, p := b[0], b[1:]
	if gap {
		s.asm = nil
	}
	if flag&pieceCont == 0 {
		if flag&pieceMore == 0 {
			s.deliver(p)
			return
		}
		s.asm = append(make([]byte, 0, 4*len(p)), p...)
		return
	}
	if s.asm == nil {
		return // the start of this frame was lost
	}
	s.asm = append(s.asm, p...)
	if flag&pieceMore == 0 {
		s.deliver(s.asm)
		s.asm = nil
	}
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
	if len(b) > stripeHeader && b[0] == stripeMagic {
		if lane >= 0 && lane < maxStripeLanes {
			s.laneRecv[lane].Add(1)
		}
		s.ro.push(binary.BigEndian.Uint32(b[1:5]), binary.BigEndian.Uint32(b[5:9]), b[stripeHeader:])
		return
	}
	if len(b) >= ackHeader && b[0] == stripeAckMagic {
		s.ackRecv.Add(1)
		if s.snd.onAck(b, time.Now()) {
			s.wakePumps()
		}
		return
	}
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
	// Lane-level filler (keepalives) carries no stripe header; hand it up
	// untouched - the codec layer decodes it to nothing.
	s.deliver(b)
}

// ackLoop sends the receiver's state to the sender: every ackInterval while
// new pieces arrive, and at least every 4 intervals while a hole is open.
func (s *StripedTransport) ackLoop() {
	t := time.NewTicker(ackInterval)
	defer t.Stop()
	var lastGen uint64
	var lastAt time.Time
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			gen, holes, ok := s.ro.ackState()
			if !ok || (gen == lastGen && (!holes || now.Sub(lastAt) < 4*ackInterval)) {
				continue
			}
			lastGen, lastAt = gen, now
			s.snd.setAck(s.ro.ackFrame())
			s.ackSent.Add(1)
			s.wakePumps()
		}
	}
}

// lossLoop catches what no ack reveals - the tail of a burst - by the
// retransmission timeout, and keeps the lanes' figures.
func (s *StripedTransport) lossLoop() {
	t := time.NewTicker(ackInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			if s.snd.detectLoss(now) {
				s.wakePumps()
			}
			s.snd.tickLanes(now)
		}
	}
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
		d, sk, late, buffered, gaps := s.ro.snapshot()
		loss, _ := s.uplinkLoss()
		s.watchMu.Lock()
		nw := len(s.watched)
		if time.Since(s.watchAt) > 5*time.Second {
			nw = -1
		}
		s.watchMu.Unlock()
		ss := s.snd.snapshot()
		var per []string
		for i := range lanes {
			if i >= maxStripeLanes {
				break
			}
			id := ""
			if li, ok := lanes[i].(LaneIDer); ok && len(li.LaneID()) >= 4 {
				id = li.LaneID()[:4]
			}
			dead := ""
			if ss.laneDead[i] {
				dead = " DEAD"
			}
			per = append(per, fmt.Sprintf("%d:%s s%d/r%d loss%.0f%%%s", i, id, s.laneSent[i].Load(), s.laneRecv[i].Load(), ss.laneLoss[i]*100, dead))
		}
		utils.Packetf("[STRIPE] per-lane %s | arrival gaps 1:%d 2-3:%d 4-7:%d 8-15:%d 16-31:%d 32-63:%d 64-127:%d 128+:%d",
			strings.Join(per, " "), gaps[0], gaps[1], gaps[2], gaps[3], gaps[4], gaps[5], gaps[6], gaps[7])
		utils.Packetf("[STRIPE] farWatches=%d lanes up=%d/%d | rx delivered=%d skipped=%d late=%d buffered=%d acks=%d | tx pieces=%d rtx=%d (rto %d) dups=%d inflight=%d queue=%dKB/%dms codelDrop=%d sendDropped=%d acks=%d srtt=%s rto=%s | uplinkLoss=%.1f%% cap=%.0fKB/s",
			nw, up, len(lanes), d, sk, late, buffered, s.ackSent.Load(),
			ss.pieces, ss.rtx, ss.rtxTimeout, ss.dups, ss.inflight, ss.queueBytes/1024, ss.queueDelay.Milliseconds(), ss.codelDrops, s.dropped.Load(), s.ackRecv.Load(),
			ss.srtt.Round(time.Millisecond), ss.rto.Round(time.Millisecond), loss*100, s.getCap()/1000)
	}
}

// takeTokens enforces the autoscaler's rate cap (bytes/s).
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

// autoscaleLoop grows the lane count while the send queue stays backed up,
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

		lanes, _ := s.snapshotLanes()
		if s.snd.backlogged() {
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
		if !atCeiling && !ceilingAt.IsZero() && s.getCap() > 0 {
			// Re-probe: lift the cap and let the next overload try a lane.
			ceilingAt = time.Time{}
			s.setCap(0)
		}
	}
}

// sender is the sending half of the link: the queue of new frames, the
// pieces in flight, the retransmission queue and the ack waiting to go.
type sender struct {
	mu sync.Mutex

	queue      []queuedFrame // new frames, oldest first
	queueBytes int
	cur        []byte // the rest of the frame being cut into pieces
	curStarted bool

	codelFirstAbove time.Time
	codelDropping   bool
	codelDropNext   time.Time
	codelCount      int
	codelDrops      uint64

	seq     uint32 // last seq handed out
	low     uint32 // lowest seq not yet acked
	ring    []txEntry
	rtxQ    []uint32
	ack     []byte // the newest ack, waiting for a lane
	pieces  uint64
	rtx     uint64
	dupSent uint64
	rtxTime uint64 // retransmissions caused by the timeout, not by acks

	laneTx  [maxStripeLanes]uint64 // transmissions per lane, in send order
	laneAck [maxStripeLanes]uint64 // highest of them known delivered
	lane    [maxStripeLanes]laneState

	srtt, rttvar time.Duration
	minRTT       time.Duration
	minRTTAt     time.Time
	triesLogAt   time.Time
}

type queuedFrame struct {
	b  []byte
	at time.Time
}

// txEntry is a piece in flight.
type txEntry struct {
	seq     uint32
	buf     []byte
	ok      bool  // sent and not yet acked
	queued  bool  // waiting in rtxQ
	dups    uint8 // spare-room copies sent since the last (re)send
	dupAt   time.Time
	lane    uint8
	laneIdx uint64 // laneTx[lane] when it last left
	at      time.Time
	tries   uint8
}

// txRingSize bounds the pieces in flight (about 2 s at 11 full lanes).
const txRingSize = 1 << 15

func (q *sender) init() {
	q.ring = make([]txEntry, txRingSize)
	// A random first seq, so a restarted sender is far from the window the
	// receiver still keeps for its previous run.
	q.seq = mrand.Uint32()
	q.low = q.seq + 1
}

// enqueue adds a frame to the send queue; false if the queue is full.
func (q *sender) enqueue(b []byte) bool {
	cp := append([]byte(nil), b...)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queueBytes+len(cp) > sendQueueLimit {
		return false
	}
	q.queue = append(q.queue, queuedFrame{b: cp, at: time.Now()})
	q.queueBytes += len(cp)
	return true
}

// setAck replaces the ack waiting to go out: acks are cumulative, only the
// newest one matters.
func (q *sender) setAck(b []byte) {
	q.mu.Lock()
	q.ack = b
	q.mu.Unlock()
}

// pull hands lane i up to max frames: the ack, retransmissions, new pieces.
func (q *sender) pull(i, max int, now time.Time) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out [][]byte
	if q.ack != nil {
		out = append(out, q.ack)
		q.ack = nil
	}
	ls := &q.lane[i]
	dead := ls.dead(now) && q.othersAlive(i, now)
	if dead {
		if now.Sub(ls.probeAt) < 100*time.Millisecond {
			return out
		}
		ls.probeAt = now
		max = len(out) + 1 // one new piece: if it arrives, the lane is back
	}
	start := len(out)
	if !dead && len(q.rtxQ) > 0 {
		// Resend on another lane than the one that lost it when there is
		// one: losses come in bursts, and the lane may still be in one.
		others := q.othersAlive(i, now)
		keep := q.rtxQ[:0]
		for _, sq := range q.rtxQ {
			e := &q.ring[sq%txRingSize]
			if !e.ok || e.seq != sq || !e.queued {
				continue
			}
			// ...but not for long: the others may be failing too.
			mine := others && int(e.lane) == i && now.Sub(e.at) < 2*q.rto()
			if len(out) >= max || mine {
				keep = append(keep, sq)
				continue
			}
			e.queued = false
			e.tries++
			e.dups = 0
			if e.tries == 4 && now.Sub(q.triesLogAt) > time.Second {
				q.triesLogAt = now
				utils.Debugf("[STRIPE] seq %d: 4th send, now on lane %d (was lane %d, %s ago)", sq, i, e.lane, now.Sub(e.at).Round(time.Millisecond))
			}
			q.stamp(e, i, now)
			q.rtx++
			out = append(out, e.buf)
		}
		q.rtxQ = keep
	}
	for len(out) < max {
		if q.seq+1-q.low >= txRingSize {
			break // the ring is full of pieces in flight
		}
		p := q.nextPiece(now)
		if p == nil {
			break
		}
		q.seq++
		buf := make([]byte, stripeHeader+len(p))
		buf[0] = stripeMagic
		binary.BigEndian.PutUint32(buf[1:5], q.seq)
		binary.BigEndian.PutUint32(buf[5:9], q.low)
		copy(buf[stripeHeader:], p)
		e := &q.ring[q.seq%txRingSize]
		*e = txEntry{seq: q.seq, buf: buf, ok: true, tries: 1}
		q.stamp(e, i, now)
		q.pieces++
		out = append(out, buf)
	}
	if len(out) < max && !dead && dupOn {
		out = q.duplicates(i, max, now, out)
	}
	ls.sentN(len(out)-start, now)
	return out
}

// duplicates fills a tick's spare room with second copies of pieces whose ack
// is overdue, oldest first, on another lane than the first copy. A lane
// always has its clock's worth of packets to send; when the data does not use
// them they carry nothing but filler. Spent on early copies they repair a
// loss a timeout sooner where no later piece of the same lane would reveal
// it - the tail of a burst, a lane that went quiet - and while the SFU drops
// the most, in the first seconds of traffic, a lost resend costs less. They
// stop by themselves once the data fills the lanes.
func (q *sender) duplicates(i, max int, now time.Time, out [][]byte) [][]byte {
	if q.srtt == 0 {
		return out // no round trip known yet: nothing is overdue
	}
	overdue := q.srtt * 5 / 4
	if overdue < 40*time.Millisecond {
		overdue = 40 * time.Millisecond
	}
	scanned := 0
	for x := q.low; seqBefore(x, q.seq+1) && len(out) < max && scanned < 512; x++ {
		scanned++
		e := &q.ring[x%txRingSize]
		if !e.ok || e.seq != x || e.queued || e.dups > 0 || int(e.lane) == i || now.Sub(e.at) < overdue {
			continue
		}
		e.dups, e.dupAt = e.dups+1, now
		q.dupSent++
		out = append(out, e.buf)
	}
	return out
}

// dupOn: STRIPE_DUP=0 turns the spare-room copies off.
var dupOn = os.Getenv("STRIPE_DUP") != "0"

// othersAlive reports whether some lane other than i is carrying traffic.
func (q *sender) othersAlive(i int, now time.Time) bool {
	for j := range q.lane {
		if j != i && q.lane[j].started && !q.lane[j].deliveredAt.IsZero() && !q.lane[j].dead(now) {
			return true
		}
	}
	return false
}

func (q *sender) stamp(e *txEntry, lane int, now time.Time) {
	q.laneTx[lane]++
	e.lane, e.laneIdx, e.at = uint8(lane), q.laneTx[lane], now
}

// nextPiece cuts the next piece ([flag][bytes]) off the queue.
func (q *sender) nextPiece(now time.Time) []byte {
	if q.cur == nil {
		f, ok := q.dequeue(now)
		if !ok {
			return nil
		}
		q.cur, q.curStarted = f, false
	}
	n := len(q.cur)
	if n > stripePieceBytes {
		n = stripePieceBytes
	}
	flag := byte(0)
	if q.curStarted {
		flag |= pieceCont
	}
	if n < len(q.cur) {
		flag |= pieceMore
	}
	p := make([]byte, 1+n)
	p[0] = flag
	copy(p[1:], q.cur[:n])
	q.cur, q.curStarted = q.cur[n:], true
	if len(q.cur) == 0 {
		q.cur = nil
	}
	return p
}

// dequeue takes the oldest frame, dropping at the head as CoDel (RFC 8289)
// says when frames have waited too long for too long.
func (q *sender) dequeue(now time.Time) ([]byte, bool) {
	for len(q.queue) > 0 {
		f := q.queue[0]
		q.queue[0] = queuedFrame{}
		q.queue = q.queue[1:]
		q.queueBytes -= len(f.b)
		if q.codelShouldDrop(now.Sub(f.at), now) {
			q.codelDrops++
			continue
		}
		return f.b, true
	}
	q.codelFirstAbove = time.Time{}
	q.codelDropping = false
	return nil, false
}

func (q *sender) codelShouldDrop(sojourn time.Duration, now time.Time) bool {
	okToDrop := false
	if sojourn < codelTarget || q.queueBytes < 2*stripePieceBytes {
		q.codelFirstAbove = time.Time{}
	} else if q.codelFirstAbove.IsZero() {
		q.codelFirstAbove = now.Add(codelInterval)
	} else if !now.Before(q.codelFirstAbove) {
		okToDrop = true
	}
	if q.codelDropping {
		if !okToDrop {
			q.codelDropping = false
			return false
		}
		if !now.Before(q.codelDropNext) {
			q.codelCount++
			q.codelDropNext = q.codelControl(q.codelDropNext)
			return true
		}
		return false
	}
	if okToDrop {
		q.codelDropping = true
		if q.codelCount > 2 && now.Sub(q.codelDropNext) < 16*codelInterval {
			q.codelCount -= 2
		} else {
			q.codelCount = 1
		}
		q.codelDropNext = q.codelControl(now)
		return true
	}
	return false
}

func (q *sender) codelControl(t time.Time) time.Time {
	return t.Add(time.Duration(float64(codelInterval) / math.Sqrt(float64(q.codelCount))))
}

// onAck applies an ack; true if it queued retransmissions.
func (q *sender) onAck(b []byte, now time.Time) bool {
	cum := binary.BigEndian.Uint32(b[1:5])
	hi := binary.BigEndian.Uint32(b[5:9])
	n := int(b[9])
	if len(b) < ackHeader+6*n {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// Only an ack about the pieces of this run counts; one left over from
	// before a restart (ours or the receiver's) describes other seqs.
	inflight := int64(q.seq + 1 - q.low)
	if d := int64(int32(cum - q.low)); d < -txRingSize || d > inflight {
		return false
	}
	if d := int64(int32(hi - q.low)); d < -txRingSize-1 || d >= inflight {
		return false
	}
	var sample time.Duration
	acked := func(sq uint32) {
		e := &q.ring[sq%txRingSize]
		if !e.ok || e.seq != sq {
			return
		}
		e.ok, e.buf = false, nil
		d := now.Sub(e.at)
		// Which copy came in is unknown when there were several: a resent
		// piece acked sooner than a round trip after the resend came in on
		// its earlier copy, and crediting its lane with the resend would make
		// every piece sent there since then look lost. A piece that also went
		// out as a spare-room copy may have arrived on either lane.
		// An ack sooner than a round trip after the copy left came in on the
		// original.
		original := e.dups == 0 || now.Sub(e.dupAt) < q.minRTT*7/8
		if original && (e.tries == 1 || d >= q.minRTT*7/8) {
			q.lane[e.lane].got(now)
			if e.laneIdx > q.laneAck[e.lane] {
				q.laneAck[e.lane] = e.laneIdx
			}
		}
		if e.tries == 1 && original && (sample == 0 || d < sample) {
			sample = d
		}
	}
	for ; seqBefore(q.low, cum); q.low++ {
		acked(q.low)
	}
	x := q.low
	if seqBefore(x, cum) {
		x = cum
	}
	for r := 0; r <= n && !seqBefore(hi, x); r++ {
		end := hi + 1 // the end of the received stretch before run r
		var runEnd uint32
		if r < n {
			start := binary.BigEndian.Uint32(b[ackHeader+6*r:])
			cnt := uint32(binary.BigEndian.Uint16(b[ackHeader+6*r+4:]))
			end, runEnd = start, start+cnt
		}
		for ; seqBefore(x, end); x++ {
			acked(x)
		}
		if r < n && seqBefore(x, runEnd) {
			x = runEnd
		}
	}
	if sample > 0 {
		q.rttSample(sample)
	}
	return q.detectLocked(now)
}

func (q *sender) rttSample(d time.Duration) {
	if now := time.Now(); q.minRTT == 0 || d < q.minRTT || now.Sub(q.minRTTAt) > 10*time.Second {
		q.minRTT, q.minRTTAt = d, now
	}
	if q.srtt == 0 {
		q.srtt, q.rttvar = d, d/2
		return
	}
	dev := q.srtt - d
	if dev < 0 {
		dev = -dev
	}
	q.rttvar = (3*q.rttvar + dev) / 4
	q.srtt = (7*q.srtt + d) / 8
}

// rto is the link's retransmission timeout: a piece neither acked nor shown
// lost by its lane after this long is sent again.
func (q *sender) rto() time.Duration {
	if q.srtt == 0 {
		return 500 * time.Millisecond
	}
	r := q.srtt + 4*q.rttvar + 2*ackInterval
	if r < 50*time.Millisecond {
		r = 50 * time.Millisecond
	}
	if r > 2*time.Second {
		r = 2 * time.Second
	}
	return r
}

func (q *sender) detectLoss(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.detectLocked(now)
}

// detectLocked queues every piece in flight that its lane shows lost, or
// that has waited longer than the timeout (doubled once it has been resent:
// a lost resend should not cost the stream hundreds of milliseconds more).
func (q *sender) detectLocked(now time.Time) bool {
	rto := q.rto()
	added := false
	for x := q.low; seqBefore(x, q.seq+1); x++ {
		e := &q.ring[x%txRingSize]
		if !e.ok || e.seq != x || e.queued {
			continue
		}
		byLane := q.laneAck[e.lane] > e.laneIdx+laneReorderSlack
		if !byLane {
			wait := rto << min(int(e.tries)-1, 1)
			if now.Sub(e.at) < wait {
				continue
			}
			q.rtxTime++
		}
		q.lane[e.lane].lostOne()
		e.queued = true
		q.rtxQ = append(q.rtxQ, x)
		added = true
	}
	return added
}

// tickLanes rolls the lanes' loss figures.
func (q *sender) tickLanes(now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.lane {
		q.lane[i].tick(now)
	}
}

// backlogged reports whether new data is waiting longer than CoDel's target.
func (q *sender) backlogged() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue) > 0 && time.Since(q.queue[0].at) >= codelTarget
}

type senderStats struct {
	pieces, rtx, rtxTimeout, codelDrops, dups uint64
	inflight, queueBytes                      int
	queueDelay, srtt, rto                     time.Duration
	laneLoss                                  [maxStripeLanes]float64
	laneDead                                  [maxStripeLanes]bool
}

func (q *sender) snapshot() senderStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := senderStats{pieces: q.pieces, rtx: q.rtx, rtxTimeout: q.rtxTime, codelDrops: q.codelDrops, dups: q.dupSent,
		inflight: int(q.seq + 1 - q.low), queueBytes: q.queueBytes, srtt: q.srtt, rto: q.rto()}
	for i := range q.lane {
		st.laneLoss[i], st.laneDead[i] = q.lane[i].loss, q.lane[i].dead(time.Now())
	}
	if len(q.queue) > 0 {
		st.queueDelay = time.Since(q.queue[0].at)
	}
	return st
}

// reorderBuffer delivers frames in seq order. Frames sit in a ring indexed by
// seq; a hole is given up on once the frame right after it has waited longer
// than holeDeadline. It also keeps what the ack reports.
type reorderBuffer struct {
	mu      sync.Mutex
	started bool
	next    uint32
	hi      uint32 // highest seq seen
	gen     uint64 // bumped by every new piece
	ring    []reorderSlot
	count   int
	out     chan reorderOut
	gapNext bool

	delivered, skipped, late uint64
	gaps                     [8]uint64 // runs of seqs jumped over on arrival: 1, 2-3, 4-7, ... 128+
}

type reorderSlot struct {
	seq  uint32
	data []byte
	at   time.Time
	ok   bool
}

const reorderWindow = txRingSize

// reorderOut is one in-order frame; gap says seqs were skipped just before it.
type reorderOut struct {
	b   []byte
	gap bool
}

func newReorderBuffer(deliver func([]byte, bool)) *reorderBuffer {
	r := &reorderBuffer{ring: make([]reorderSlot, reorderWindow), out: make(chan reorderOut, 8192)}
	go func() {
		for o := range r.out {
			deliver(o.b, o.gap)
		}
	}()
	return r
}

func seqBefore(a, b uint32) bool { return int32(a-b) < 0 }

// push files piece seq; low is the sender's lowest unacknowledged seq.
func (r *reorderBuffer) push(seq, low uint32, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.started {
		r.started = true
		r.startAt(seq, low)
	}
	r.gen++ // even a duplicate or a late piece: the sender should hear we have it
	if d := int32(seq - r.next); d >= 2*reorderWindow || d < -reorderWindow {
		// Nowhere near the window: the sender restarted (each run starts at
		// a random seq). Start over at its new position.
		r.resyncLocked(seq, low)
	}
	if seqBefore(seq, r.next) {
		r.late++
		return
	}
	// Too far ahead for the window: give up on the oldest holes.
	for seq-r.next >= reorderWindow {
		r.skipToLocked(r.lowestLocked(seq))
	}
	sl := &r.ring[seq%reorderWindow]
	if sl.ok && sl.seq == seq {
		return // duplicate
	}
	*sl = reorderSlot{seq: seq, data: data, at: now, ok: true}
	r.count++
	if seqBefore(r.hi, seq) {
		if g := seq - r.hi - 1; g > 0 {
			b := 0
			for g > 1 && b < len(r.gaps)-1 {
				g >>= 1
				b++
			}
			r.gaps[b]++
		}
		r.hi = seq
	}
	r.drainLocked()
}

func (r *reorderBuffer) resyncLocked(seq, low uint32) {
	utils.Infof("[STRIPE] peer restarted (seq %d, expected %d): starting over", seq, r.next)
	for i := range r.ring {
		r.ring[i] = reorderSlot{}
	}
	r.count, r.gapNext = 0, true
	r.startAt(seq, low)
}

// startAt begins the stream at the sender's low mark, so pieces it sent
// before the first one that got here are still waited for.
func (r *reorderBuffer) startAt(seq, low uint32) {
	r.next = seq
	if d := seq - low; d < reorderWindow/2 {
		r.next = low
	}
	r.hi = r.next - 1
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
		r.out <- reorderOut{b: b, gap: r.gapNext}
		r.gapNext = false
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
		r.gapNext = true
		r.next = s
		r.gen++
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
		for r.count > 0 {
			low := r.lowestLocked(r.next + reorderWindow)
			waited := time.Since(r.ring[low%reorderWindow].at)
			if waited <= holeDeadline {
				break
			}
			utils.Infof("[STRIPE] gave up on %d piece(s) from seq %d after %s", low-r.next, r.next, waited.Round(time.Millisecond))
			r.skipToLocked(low)
		}
		r.mu.Unlock()
	}
}

// ackState reports the change counter and whether a hole is open.
func (r *reorderBuffer) ackState() (gen uint64, holes, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen, r.count > 0, r.started
}

// ackFrame builds an ack of the current state (see the wire formats).
func (r *reorderBuffer) ackFrame() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	hi := r.hi
	if seqBefore(hi, r.next) {
		hi = r.next - 1
	}
	b := make([]byte, ackHeader, ackHeader+6*8)
	b[0] = stripeAckMagic
	binary.BigEndian.PutUint32(b[1:5], r.next)
	n := 0
	for s := r.next; seqBefore(s, hi); {
		if sl := &r.ring[s%reorderWindow]; sl.ok && sl.seq == s {
			s++
			continue
		}
		if n == ackMaxRuns {
			hi = s - 1 // report no further than the runs listed
			break
		}
		start := s
		for seqBefore(s, hi) && s-start < math.MaxUint16 {
			if sl := &r.ring[s%reorderWindow]; sl.ok && sl.seq == s {
				break
			}
			s++
		}
		b = binary.BigEndian.AppendUint32(b, start)
		b = binary.BigEndian.AppendUint16(b, uint16(s-start))
		n++
	}
	binary.BigEndian.PutUint32(b[5:9], hi)
	b[9] = byte(n)
	return b
}

func (r *reorderBuffer) snapshot() (delivered, skipped, late uint64, buffered int, gaps [8]uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.delivered, r.skipped, r.late, r.count, r.gaps
}
