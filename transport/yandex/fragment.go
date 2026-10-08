package yandex

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// One VP8 frame cannot carry an arbitrary amount of tunnel data: the body has
// to stay inside a single RTP packet, so anything past roughly a kilobyte has
// to be split across several frames and put back together by the receiver.
//
// This is not a theoretical limit. A single batch frame holding one full-size
// TCP segment is a little over 1500 bytes, and one holding a TLS certificate
// chain is tens of kilobytes. Without fragmentation, GenerateKeyFrame clamped
// the body and shipped a frame whose length prefix still advertised the full
// size, so the receiver failed with "zstd decode: unexpected EOF" or "truncated
// packet: need 1472, have 1196" and dropped the data. Small HTTP pages fit by
// accident; anything larger is lost, which is why HTTPS through the tunnel
// failed while plain HTTP worked.
//
// Wire format of a fragment body, before the VP8 wrapper:
//
//	[0]     0xF1        magic
//	[1]     index       zero-based fragment number
//	[2]     count       total fragments in this frame
//	[3:7]   total       length of the reassembled wire frame, big endian
//	[7]     frameSeq    identifies which frame this fragment belongs to
//	[8:]    bytes       this fragment's slice of the wire frame
//
// frameSeq exists because the reassembler used to tell frames apart purely by
// (count, total): with every publish tick now able to emit up to
// maxFramesPerTick frames at once (transport/yandex/telemost.go), fragments
// from two different, similarly-sized frames can legitimately arrive
// interleaved, and two unrelated frames sharing the same byte length is not
// rare - most short tunnel segments round-trip-ack a handful of near-identical
// sizes. Without a real identity, a fragment from the wrong frame could be
// accepted into the one being assembled, splicing two frames' bytes together
// into tunnel data that looks intact but silently isn't.
const (
	fragMagic            = 0xF1
	fragHeaderLen        = 8
	maxFragmentsPerFrame = 255
)

// maxVP8Payload is the largest wire-frame piece one RTP packet carries.
//
// 1312 puts the whole RTP payload (VP8 descriptor + fake keyframe header + our
// piece) at 1332 bytes, the size proven to cross the SFU in both directions.
// At the old 1400 (1420-byte payloads) a Real run through a server delivered
// ~90% of the packets uphill but only ~40% downhill to a phone: the SFU adds
// header extensions and SRTP overhead, and a packet near the 1500-byte path
// MTU is dropped or fragmented on the way. TELEMOST_FRAG_BYTES overrides.
var (
	maxVP8Payload    = envIntDefault("TELEMOST_FRAG_BYTES", 1312)
	maxFragmentBytes = maxVP8Payload
)

// fragFrameSeq hands out the frameSeq byte for FragmentFrame. It only needs
// to be different for frames close enough together to still be in flight at
// once - reassembleTimeout (500ms) at 30fps is at most ~15 frames - so
// wrapping at 256 leaves a wide margin and one counter shared by every
// transport instance in the process is harmless: a reassembler only ever
// compares frameSeq values from its own sender's stream.
var fragFrameSeq atomic.Uint32

// FragmentFrame splits one complete wire-v2 batch frame into bodies that each
// fit inside a single VP8 frame. A frame that already fits is returned as a
// single fragment rather than being passed through, so both ends agree on the
// format unconditionally and there is no "small frames use a different shape"
// branch to get wrong.
func FragmentFrame(wire []byte) ([][]byte, error) {
	chunkSize := maxFragmentBytes - fragHeaderLen
	n := (len(wire) + chunkSize - 1) / chunkSize
	if n == 0 {
		n = 1
	}
	if n > maxFragmentsPerFrame {
		return nil, fmt.Errorf("wire frame of %d bytes needs %d fragments, limit is %d",
			len(wire), n, maxFragmentsPerFrame)
	}
	seq := byte(fragFrameSeq.Add(1))
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(wire) {
			end = len(wire)
		}
		b := make([]byte, 0, maxFragmentBytes)
		b = append(b, fragMagic, byte(i), byte(n))
		b = binary.BigEndian.AppendUint32(b, uint32(len(wire)))
		b = append(b, seq)
		b = append(b, wire[start:end]...)
		// Tried padding a short last fragment out to the same size as its
		// siblings here: a real run's logs showed the reassembler losing the
		// same fragment index over and over, always the final, short one of
		// a frame, which reads like something downstream treating an
		// undersized "video frame" as invalid. Scoped to multi-fragment
		// frames only (padding single-fragment ones too - every bare SYN or
		// ACK - was worse: a constant stream of full-size frames instead of
		// the occasional one), it still measured worse than not padding at
		// all: abandons got more frequent and spread across fragment
		// indices instead of concentrating on the last one, consistent with
		// the larger average burst tripping real bandwidth policy rather
		// than fixing a frame-validity check. Left out; push() still
		// tolerates a frame that came in padded (any build still running
		// the old wire shape), it just never produces one.
		out = append(out, b)
	}
	return out, nil
}

// frameReassembler collects fragments until a whole frame is available.
//
// Video is a lossy medium and the SFU drops and reorders packets, so a
// reassembly that waited forever for a missing fragment would eventually hand
// the tunnel a spliced-together frame. Each slot therefore carries a
// deadline: the frame is abandoned and the slot freed once the gap stops
// being worth waiting for, which costs one lost frame instead of every
// subsequent frame too.
//
// Fragments are kept in a slot per index rather than appended as they arrive
// in order. An earlier version only accepted a fragment when its index
// equaled the next expected one and otherwise discarded it outright ("hold
// the rest for the timeout" was a comment, not code - nothing was held). Real
// RTP delivery reorders packets even when none are lost, and sending more
// than one fragment per publish tick (maxFramesPerTick) made that reordering
// far more likely to land inside a single frame's fragments. Every
// out-of-order fragment was silently thrown away, so the frame it belonged to
// was already missing a piece the moment it arrived - reassembly could only
// ever time out, discarding a whole TCP-sized chunk (up to maxBatchBytes) for
// a fragment that in fact showed up on time, just second. Slotting by index
// makes arrival order within a frame irrelevant: it completes as soon as
// every index is filled, regardless of the order that happened in.
//
// A second kind of reordering sits one level up: frame A can still be
// missing a fragment when frame B's fragments start arriving. One shared
// buffer can only track one frame, so B starting still evicted A outright -
// correctness held (frameSeq below stops two unrelated frames from being
// spliced together), but A was lost just as surely as before, for no reason
// but ordering. reassemblySlots lets several frames be tracked at once so
// that kind of interleaving, measured on a real run at roughly one frame a
// second, no longer costs anything.
type reassemblySlot struct {
	active   bool     // false once freed - skipped by findSlot, reusable
	seq      byte     // frameSeq of the frame this slot is assembling
	total    int      // reassembled frame length in bytes, from the fragment header
	count    int      // number of fragments this frame is split into
	received int      // how many distinct indices are filled
	parts    [][]byte // parts[i] is fragment i's payload, nil until it arrives
	deadline int64    // unix nanos
}

func (s *reassemblySlot) reset() {
	s.active = false
	s.seq = 0
	s.total = 0
	s.count = 0
	s.received = 0
	s.parts = nil
	s.deadline = 0
}

// reassemblySlots bounds how many frames reassembleFrom tracks concurrently.
// Real frames are typically one or two fragments and complete within a
// handful of milliseconds of their first fragment arriving, so the number
// actually in flight at once - waiting on one straggling fragment - stays
// small; this just needs enough headroom over that to absorb a burst
// without the oldest, most-nearly-complete ones getting evicted by newer
// ones that arrived first.
const reassemblySlots = 24

type frameReassembler struct {
	slots [reassemblySlots]reassemblySlot
}

// reassembleTimeout bounds how long a partial frame is held.
//
// A genuinely lost RTP packet is not necessarily gone for good: Pion answers
// the SFU's own RTCP NACK by retransmitting it, and that recovered packet
// still reaches ReadRTP - just late. Abandoning the frame before it has a
// chance to arrive throws away real data that was in fact only delayed,
// forcing recovery one layer up, at the TCP level, where a lost segment costs
// a full RTO backoff (hundreds of ms doubling from there) instead of one
// frame.
//
// Measured on a live run: RTCP NACKs for this stream's lost packets fire
// roughly once a second, and the matching Receiver Report one second later
// already shows cumulative loss back at 0 - so the NACK round trip that
// recovers a lost packet here is consistently under ~1s. 500ms was cutting
// that recovery off partway through on a real run (reassembly logged
// "abandoned" at close to that same once-a-second rate). This only cost
// nothing extra to raise once reassemblySlots stopped a slow slot from
// blocking anything else: the slot just sits occupied a bit longer.
const reassembleTimeout = 1200

// findSlot returns the active slot already assembling frameSeq, or nil.
func (r *frameReassembler) findSlot(seq byte) *reassemblySlot {
	for i := range r.slots {
		if r.slots[i].active && r.slots[i].seq == seq {
			return &r.slots[i]
		}
	}
	return nil
}

// claimSlot returns a slot for a frame not currently being tracked: the
// first free one, or - if every slot is busy - the one closest to its own
// deadline anyway, logging what it discards. Picking the one nearest timeout
// rather than, say, the least complete one keeps this a close approximation
// of "evict whichever was going to be abandoned soonest regardless."
func (r *frameReassembler) claimSlot() *reassemblySlot {
	for i := range r.slots {
		if !r.slots[i].active {
			return &r.slots[i]
		}
	}
	victim := &r.slots[0]
	for i := 1; i < len(r.slots); i++ {
		if r.slots[i].deadline < victim.deadline {
			victim = &r.slots[i]
		}
	}
	utils.Debugf("[Telemost] frame reassembly evicted (all %d slots busy): got %d/%d fragments of frame %d",
		reassemblySlots, victim.received, victim.count, victim.seq)
	return victim
}

func (r *frameReassembler) push(body []byte, now int64) (frame []byte, ok bool) {
	if len(body) < fragHeaderLen || body[0] != fragMagic {
		// Not one of ours. Handing it straight through keeps the decoder
		// tolerant of anything that already matches the batch wire format.
		return body, true
	}
	index := int(body[1])
	count := int(body[2])
	total := int(binary.BigEndian.Uint32(body[3:7]))
	seq := body[7]
	payload := body[fragHeaderLen:]

	if count == 0 || index >= count || total > maxFrameReassemblyBytes {
		return nil, false
	}

	// Every fragment FragmentFrame sends is padded out to the same fixed
	// size (see there for why), so what actually arrived is not this
	// fragment's real length - total, count and index already say exactly
	// how long it should be, and that is trusted over the padded length.
	chunkSize := maxFragmentBytes - fragHeaderLen
	want := chunkSize
	if index == count-1 {
		want = total - index*chunkSize
	}
	if want < 0 || want > len(payload) {
		return nil, false
	}
	payload = payload[:want]

	// Sweep timed-out slots on every call, not only when their own frame's
	// next fragment would have touched them - an abandoned frame's last
	// fragment may never arrive at all, and the slot (and the log line
	// recording it) should not have to wait for unrelated traffic to notice.
	for i := range r.slots {
		s := &r.slots[i]
		if s.active && now > s.deadline {
			var missing []int
			for idx, p := range s.parts {
				if p == nil {
					missing = append(missing, idx)
				}
			}
			utils.Debugf("[Telemost] frame reassembly abandoned: got %d/%d fragments, total=%d bytes, missing=%v",
				s.received, s.count, s.total, missing)
			s.reset()
		}
	}

	slot := r.findSlot(seq)
	if slot == nil {
		slot = r.claimSlot()
		slot.reset()
		slot.active = true
		slot.seq = seq
		slot.count = count
		slot.total = total
		slot.parts = make([][]byte, count)
		slot.deadline = now + int64(reassembleTimeout)*1e6
	}
	if slot.parts[index] == nil {
		// payload aliases the caller's read buffer, which gets reused on the
		// next read; it has to be copied out to survive until the frame
		// completes.
		buf := make([]byte, len(payload))
		copy(buf, payload)
		slot.parts[index] = buf
		slot.received++
	}
	if slot.received < slot.count {
		return nil, false
	}
	out := make([]byte, 0, slot.total)
	for _, p := range slot.parts {
		out = append(out, p...)
	}
	frameTotal := slot.total
	slot.reset()
	if len(out) != frameTotal {
		return nil, false
	}
	return out, true
}

// maxFrameReassemblyBytes caps what a peer can make us allocate. A frame this
// size already needs 139 fragments, so anything larger is a mistake or an
// attempt to exhaust memory.
const maxFrameReassemblyBytes = 1 << 20
