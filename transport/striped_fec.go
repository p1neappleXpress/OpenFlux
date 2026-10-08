package transport

import (
	"encoding/binary"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
)

// Forward error correction for the striped transport.
//
// The Telemost SFU drops 10-30% of the RTP it relays, in bursts of 4-15
// packets a few times a second. Retransmission alone (ARQ) costs a round trip
// per loss and stalls everything behind it; TCP inside the tunnel reads either
// the loss or the stall as congestion and collapses. FEC hides the loss
// without waiting: frames are grouped, each group gets Reed-Solomon parity
// frames, and the receiver rebuilds what is missing from any k of the k+r
// frames of a group.
//
// Frames are dealt round-robin into fecInterleave groups that are open at the
// same time, so a burst of consecutive lost packets hits many groups once
// each instead of one group many times. A group closes when it has fecK frames
// or is fecMaxAge old, whichever comes first (a quiet link sends short groups,
// which is mostly replication - cheap in absolute terms).
//
// Wire formats (stripe magic bytes are in striped.go):
//
//	data   [0xAC][seq u32][gid u32][idx u8][payload]
//	parity [0xAB][gid u32][k u8][r u8][pidx u8][seq u32 * k][len u16 * k][shard]
const (
	stripeFecDataMagic   = 0xAC
	stripeFecParityMagic = 0xAB
	fecDataHeader        = 10
)

var fecEnabled = os.Getenv("STRIPE_FEC") != "0"

func envIntOr(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

var (
	fecK          = envIntOr("STRIPE_FEC_K", 16)
	fecInterleave = envIntOr("STRIPE_FEC_D", 8)
	fecMaxAge     = time.Duration(envIntOr("STRIPE_FEC_AGE_MS", 15)) * time.Millisecond
	// fecParityPct scales the parity count (100 = the default table below).
	fecParityPct = envIntOr("STRIPE_FEC_PCT", 100)
)

// fecParityCount is the number of parity frames for a group of k data frames.
func fecParityCount(k int) int {
	var r int
	switch {
	case k <= 1:
		r = 2
	case k <= 3:
		r = 2
	case k <= 6:
		r = 3
	case k <= 10:
		r = 4
	default:
		r = (k + 1) / 2
	}
	r = (r*fecParityPct + 50) / 100
	if r < 1 {
		r = 1
	}
	return r
}

type fecFrame struct {
	seq  uint32
	data []byte
}

type fecOpenGroup struct {
	gid    uint32
	frames []fecFrame
	first  time.Time
}

// fecTx is the sender's state.
type fecTx struct {
	mu   sync.Mutex
	open []*fecOpenGroup
	gid  uint32
}

// fecRxGroup is what the receiver has seen of one group.
type fecRxGroup struct {
	created, updated time.Time
	k, r             int
	meta             bool
	seqs             []uint32
	lens             []uint16
	data             map[int][]byte
	par              map[int][]byte
	done             bool
}

type fecRx struct {
	mu     sync.Mutex
	groups map[uint32]*fecRxGroup
}

var (
	rsMu     sync.Mutex
	rsCached = map[[2]int]reedsolomon.Encoder{}
)

func rsEncoder(k, r int) (reedsolomon.Encoder, error) {
	rsMu.Lock()
	defer rsMu.Unlock()
	key := [2]int{k, r}
	if e, ok := rsCached[key]; ok {
		return e, nil
	}
	e, err := reedsolomon.New(k, r)
	if err != nil {
		return nil, err
	}
	rsCached[key] = e
	return e, nil
}

// fecAdd puts a frame into its open group and returns the header fields for
// the data frame plus the group if this frame closed it.
func (s *StripedTransport) fecAdd(seq uint32, data []byte) (gid uint32, idx int, closed *fecOpenGroup) {
	t := &s.fecTx
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = make([]*fecOpenGroup, fecInterleave)
	}
	slot := int(seq % uint32(fecInterleave))
	g := t.open[slot]
	if g == nil {
		t.gid++
		g = &fecOpenGroup{gid: t.gid, first: time.Now()}
		t.open[slot] = g
	}
	idx = len(g.frames)
	g.frames = append(g.frames, fecFrame{seq: seq, data: data})
	gid = g.gid
	if len(g.frames) >= fecK {
		closed = g
		t.open[slot] = nil
	}
	return gid, idx, closed
}

// fecFlush closes groups that have waited long enough.
func (s *StripedTransport) fecFlush(now time.Time) {
	t := &s.fecTx
	var out []*fecOpenGroup
	t.mu.Lock()
	for i, g := range t.open {
		if g != nil && now.Sub(g.first) >= fecMaxAge {
			out = append(out, g)
			t.open[i] = nil
		}
	}
	t.mu.Unlock()
	for _, g := range out {
		s.fecEmit(g)
	}
}

// fecEmit encodes and sends the parity frames of a closed group.
func (s *StripedTransport) fecEmit(g *fecOpenGroup) {
	k := len(g.frames)
	if k == 0 {
		return
	}
	r := fecParityCount(k)
	enc, err := rsEncoder(k, r)
	if err != nil {
		return
	}
	l := 0
	for _, f := range g.frames {
		if len(f.data) > l {
			l = len(f.data)
		}
	}
	shards := make([][]byte, k+r)
	for i, f := range g.frames {
		sh := make([]byte, l)
		copy(sh, f.data)
		shards[i] = sh
	}
	for j := 0; j < r; j++ {
		shards[k+j] = make([]byte, l)
	}
	if enc.Encode(shards) != nil {
		return
	}
	metaLen := 1 + 4 + 3 + 4*k + 2*k
	for j := 0; j < r; j++ {
		b := make([]byte, metaLen+l)
		b[0] = stripeFecParityMagic
		binary.BigEndian.PutUint32(b[1:], g.gid)
		b[5], b[6], b[7] = byte(k), byte(r), byte(j)
		for i, f := range g.frames {
			binary.BigEndian.PutUint32(b[8+4*i:], f.seq)
			binary.BigEndian.PutUint16(b[8+4*k+2*i:], uint16(len(f.data)))
		}
		copy(b[metaLen:], shards[k+j])
		if lane, _ := s.pickLane(); lane != nil {
			if lane.Send(b) == nil {
				s.fecParitySent.Add(1)
			}
		}
	}
}

// fecOnData records a data frame of a group (the frame itself is pushed to the
// reorder buffer by the caller).
func (s *StripedTransport) fecOnData(gid uint32, idx int, seq uint32, data []byte) {
	rx := &s.fecRx
	now := time.Now()
	rx.mu.Lock()
	defer rx.mu.Unlock()
	g := rx.group(gid, now)
	if g.done {
		return
	}
	g.data[idx] = data
	g.updated = now
}

// fecOnParity records a parity frame.
func (s *StripedTransport) fecOnParity(b []byte) {
	if len(b) < 8 {
		return
	}
	gid := binary.BigEndian.Uint32(b[1:5])
	k, r, p := int(b[5]), int(b[6]), int(b[7])
	metaLen := 8 + 6*k
	if k == 0 || len(b) < metaLen {
		return
	}
	rx := &s.fecRx
	now := time.Now()
	rx.mu.Lock()
	defer rx.mu.Unlock()
	g := rx.group(gid, now)
	if g.done {
		return
	}
	if !g.meta {
		g.k, g.r, g.meta = k, r, true
		g.seqs = make([]uint32, k)
		g.lens = make([]uint16, k)
		for i := 0; i < k; i++ {
			g.seqs[i] = binary.BigEndian.Uint32(b[8+4*i:])
			g.lens[i] = binary.BigEndian.Uint16(b[8+4*k+2*i:])
		}
	}
	g.par[p] = b[metaLen:]
	g.updated = now
}

func (rx *fecRx) group(gid uint32, now time.Time) *fecRxGroup {
	if rx.groups == nil {
		rx.groups = make(map[uint32]*fecRxGroup)
	}
	g := rx.groups[gid]
	if g == nil {
		g = &fecRxGroup{created: now, updated: now, data: map[int][]byte{}, par: map[int][]byte{}}
		rx.groups[gid] = g
	}
	return g
}

// fecRecover rebuilds the missing frames of groups that have stopped
// receiving (so the missing ones are not just late) and drops old groups.
func (s *StripedTransport) fecRecover(now time.Time) {
	type rec struct {
		seq  uint32
		data []byte
	}
	var out []rec
	rx := &s.fecRx
	rx.mu.Lock()
	for gid, g := range rx.groups {
		if now.Sub(g.created) > 5*time.Second {
			delete(rx.groups, gid)
			continue
		}
		if g.done || !g.meta || len(g.data) >= g.k || len(g.data)+len(g.par) < g.k {
			continue
		}
		if now.Sub(g.updated) < 15*time.Millisecond {
			continue
		}
		enc, err := rsEncoder(g.k, g.r)
		if err != nil {
			continue
		}
		l := 0
		for _, p := range g.par {
			l = len(p)
			break
		}
		shards := make([][]byte, g.k+g.r)
		for i := 0; i < g.k; i++ {
			if d, ok := g.data[i]; ok {
				sh := make([]byte, l)
				copy(sh, d)
				shards[i] = sh
			}
		}
		for j := 0; j < g.r; j++ {
			if p, ok := g.par[j]; ok {
				shards[g.k+j] = p
			}
		}
		if enc.ReconstructData(shards) != nil {
			continue
		}
		for i := 0; i < g.k; i++ {
			if _, ok := g.data[i]; ok {
				continue
			}
			n := int(g.lens[i])
			if n > len(shards[i]) {
				continue
			}
			out = append(out, rec{g.seqs[i], append([]byte(nil), shards[i][:n]...)})
		}
		g.done = true
	}
	rx.mu.Unlock()
	for _, r := range out {
		s.fecRecovered.Add(1)
		s.ro.push(r.seq, r.data)
	}
}

// fecLoop runs the sender's group flusher and the receiver's recovery.
func (s *StripedTransport) fecLoop() {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			s.fecFlush(now)
			s.fecRecover(now)
		}
	}
}
