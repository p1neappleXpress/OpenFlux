package yandex

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Telemost caps what one subscriber PeerConnection receives at ~16 Mbit/s in
// total, whatever the number of streams in its slots, and a single published
// stream tops out at the same rate. Several participants in ONE room break
// that ceiling: each lane is a full participant that publishes its own camera
// and subscribes to exactly one peer lane's camera - one slot, paged onto that
// peer with setSlotsOffset - so every subscriber's 16 Mbit/s carries distinct
// data. transport.StripedTransport spreads frames over the lanes and restores
// their order on the other side.

// NewTelemostGroup creates n lanes joined to the same conference. The group
// is also a transport.LaneSource, so the stripe can grow it later.
func NewTelemostGroup(cookies, conferenceURL string, n int, isExitNode bool, config transport.TransportConfig) *TelemostGroup {
	g := &TelemostGroup{cookies: cookies, url: conferenceURL, isExit: isExitNode, config: config}
	for i := 0; i < n; i++ {
		g.newLane()
	}
	// Peers become eligible for growth only after peerSettle, which no
	// description event announces - re-evaluate on a clock as well.
	go func() {
		for range time.Tick(2 * time.Second) {
			g.recompute()
		}
	}()
	return g
}

// TelemostGroup is the set of lanes (participants) one node runs in a room.
type TelemostGroup struct {
	cookies, url string
	isExit       bool
	config       transport.TransportConfig

	mu      sync.Mutex
	lanes   []*TelemostTransport
	grow    func()
	growing atomic.Bool
}

// Lanes returns the lanes created so far.
func (g *TelemostGroup) Lanes() []transport.Transport {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]transport.Transport, len(g.lanes))
	for i, l := range g.lanes {
		out[i] = l
	}
	return out
}

func (g *TelemostGroup) newLane() *TelemostTransport {
	t := NewTelemostTransport(g.cookies, g.url, g.isExit, g.config)
	if os.Getenv("TELEMOST_PUBLISH_KIND") == "" {
		t.publishKind = kindCamera // a room has one screen share; lanes are many
	}
	g.mu.Lock()
	t.lane = &stripeLane{group: g, index: len(g.lanes), parts: map[string]tmPart{}, lastReq: -1}
	g.lanes = append(g.lanes, t)
	g.mu.Unlock()
	return t
}

// NewLane implements transport.LaneSource.
func (g *TelemostGroup) NewLane() transport.Transport {
	t := g.newLane()
	g.growing.Store(false)
	return t
}

// SetGrowHook implements transport.LaneSource.
func (g *TelemostGroup) SetGrowHook(f func()) {
	g.mu.Lock()
	g.grow = f
	g.mu.Unlock()
}

type tmPart struct {
	streaming bool      // publishes camera or screen
	gone      bool      // disconnectedAt set or removed
	since     time.Time // first seen streaming
}

// peerSettle is how long a peer must stream before we add a lane for it, so
// ghosts of killed participants (no disconnectedAt yet) do not make us grow.
const peerSettle = 15 * time.Second

// stripeLane is the per-lane state of a TelemostTransport running in a group.
type stripeLane struct {
	group *TelemostGroup
	index int

	mu       sync.Mutex
	parts    map[string]tmPart
	target   string
	view     *slotsView
	shutdown bool // shutdownAllVideo last requested
	lastReq  int
	lastAt   time.Time
}

type slotsView struct {
	offset          int
	prev, cur, next []string
}

// recompute hands each lane a distinct peer to watch: every streaming
// participant of the room that is not one of our own lanes, in a stable order.
func (g *TelemostGroup) recompute() {
	g.mu.Lock()
	own := map[string]bool{}
	for _, l := range g.lanes {
		if l.peerID != "" {
			own[l.peerID] = true
		}
	}
	seen := map[string]tmPart{}
	for _, l := range g.lanes {
		l.lane.mu.Lock()
		for id, p := range l.lane.parts {
			q := seen[id]
			q.streaming = q.streaming || p.streaming
			q.gone = q.gone || p.gone
			if !p.since.IsZero() && (q.since.IsZero() || p.since.Before(q.since)) {
				q.since = p.since
			}
			seen[id] = q
		}
		l.lane.mu.Unlock()
	}
	var peers []string
	settled := 0
	for id, p := range seen {
		if p.streaming && !p.gone && !own[id] {
			peers = append(peers, id)
			if time.Since(p.since) >= peerSettle {
				settled++
			}
		}
	}
	sort.Strings(peers)
	lanes := append([]*TelemostTransport(nil), g.lanes...)
	grow := g.grow
	g.mu.Unlock()

	// More peer lanes than ours: one of them is unwatched, so its frames
	// would be lost. Ask the stripe for another lane (one at a time).
	if settled > len(lanes) && grow != nil && g.growing.CompareAndSwap(false, true) {
		grow()
	}

	// Stable assignment: a lane keeps its peer while that peer is alive (a
	// re-steer costs that lane a resubscription), and only free lanes pick
	// up peers nobody watches yet.
	alive := map[string]bool{}
	for _, p := range peers {
		alive[p] = true
	}
	taken := map[string]bool{}
	targets := make([]string, len(lanes))
	for i, l := range lanes {
		l.lane.mu.Lock()
		cur := l.lane.target
		l.lane.mu.Unlock()
		if alive[cur] && !taken[cur] {
			targets[i] = cur
			taken[cur] = true
		}
	}
	free := peers[:0:0]
	for _, p := range peers {
		if !taken[p] {
			free = append(free, p)
		}
	}
	for i := range lanes {
		if targets[i] == "" && len(free) > 0 {
			targets[i], free = free[0], free[1:]
		}
	}
	for i, l := range lanes {
		l.setStripeTarget(targets[i])
	}
}

func (t *TelemostTransport) setStripeTarget(target string) {
	ln := t.lane
	ln.mu.Lock()
	changed := ln.target != target
	ln.target = target
	needSlots := (target == "") != ln.shutdown
	ln.mu.Unlock()
	if !changed {
		return
	}
	utils.Infof("[Telemost] lane %d -> watching %q", ln.index, short(target))
	if needSlots {
		_ = t.sendSetSlots()
	}
	t.steerSlots()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// onDescriptions records participants from update/upsert/removeDescription.
func (t *TelemostTransport) onDescriptions(v interface{}, removed bool) {
	ln := t.lane
	if ln == nil {
		return
	}
	var list []interface{}
	switch x := v.(type) {
	case map[string]interface{}:
		if d, ok := x["description"].([]interface{}); ok {
			list = d
		} else {
			list = []interface{}{x}
		}
	case []interface{}:
		list = x
	}
	ln.mu.Lock()
	for _, e := range list {
		var id string
		var m map[string]interface{}
		switch y := e.(type) {
		case string:
			id = y
		case map[string]interface{}:
			m = y
			id, _ = y["id"].(string)
			if id == "" {
				id, _ = y["participantId"].(string)
			}
		}
		if id == "" {
			continue
		}
		p := ln.parts[id]
		if removed {
			p.gone = true
		} else if m != nil {
			sv, _ := m["sendVideo"].(bool)
			ss, _ := m["sendSharing"].(bool)
			p.streaming = sv || ss
			_, dis := m["disconnectedAt"]
			p.gone = dis
			if p.streaming && p.since.IsZero() {
				p.since = time.Now()
			}
		}
		ln.parts[id] = p
	}
	ln.mu.Unlock()
	ln.group.recompute()
}

// onSlotsConfig remembers the current layout page and steers toward the target.
func (t *TelemostTransport) onSlotsConfig(sc interface{}) {
	ln := t.lane
	if ln == nil {
		return
	}
	b, _ := json.Marshal(sc)
	var raw struct {
		Offset    int                      `json:"offset"`
		Slots     []map[string]interface{} `json:"slots"`
		PrevSlots []map[string]interface{} `json:"prevSlots"`
		NextSlots []map[string]interface{} `json:"nextSlots"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	ids := func(es []map[string]interface{}) []string {
		out := make([]string, len(es))
		for i, e := range es {
			for _, k := range []string{"participantVideoByMid", "participantScreenSharingByMid", "participant"} {
				if p, ok := e[k].(map[string]interface{}); ok {
					out[i], _ = p["participantId"].(string)
					break
				}
			}
		}
		return out
	}
	ln.mu.Lock()
	ln.view = &slotsView{offset: raw.Offset, prev: ids(raw.PrevSlots), cur: ids(raw.Slots), next: ids(raw.NextSlots)}
	ln.mu.Unlock()
	t.steerSlots()
}

// steerSlots pages the single slot onto the target participant.
func (t *TelemostTransport) steerSlots() {
	ln := t.lane
	ln.mu.Lock()
	v, target := ln.view, ln.target
	if v == nil || target == "" {
		ln.mu.Unlock()
		return
	}
	want := -1
	for i, id := range v.cur {
		if id == target {
			want = v.offset + i
		}
	}
	if want == v.offset {
		ln.mu.Unlock()
		return // already on it
	}
	if want < 0 {
		prevStart := v.offset - len(v.prev)
		if prevStart < 0 {
			prevStart = 0
		}
		for i, id := range v.prev {
			if id == target {
				want = prevStart + i
			}
		}
		for i, id := range v.next {
			if id == target {
				want = v.offset + len(v.cur) + i
			}
		}
	}
	if want < 0 {
		// Not on this page: scan forward, wrap to the start at the end.
		if len(v.next) > 0 {
			want = v.offset + len(v.cur) + len(v.next)
		} else {
			want = 0
		}
	}
	if want == ln.lastReq && time.Since(ln.lastAt) < 700*time.Millisecond {
		ln.mu.Unlock()
		return
	}
	ln.lastReq, ln.lastAt = want, time.Now()
	ln.mu.Unlock()
	_ = t.sendSetSlotsOffset(want)
}

// QueueDepth reports the fragments still waiting for the publish clock.
func (t *TelemostTransport) QueueDepth() int { return t.queuedFrames() }

// UplinkLoss is the fraction of our published packets the SFU reported lost
// in its latest Receiver Report.
func (t *TelemostTransport) UplinkLoss() float64 { return float64(t.uplinkLoss.Load()) / 256 }

// UplinkReceived is the cumulative packet count the SFU reports receiving.
func (t *TelemostTransport) UplinkReceived() uint64 { return t.sfuGot.Load() }

// LaneID is this lane's participant id in the room.
func (t *TelemostTransport) LaneID() string { return t.peerID }

// Watching lists the peer lanes our lanes are currently subscribed to.
func (g *TelemostGroup) Watching() []string {
	g.mu.Lock()
	lanes := append([]*TelemostTransport(nil), g.lanes...)
	g.mu.Unlock()
	var out []string
	for _, l := range lanes {
		l.lane.mu.Lock()
		if l.lane.target != "" {
			out = append(out, l.lane.target)
		}
		l.lane.mu.Unlock()
	}
	return out
}
