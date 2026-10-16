package transport

import (
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Wire format for a batched frame (one Yandex/transport message can now carry
// many tunnel packets):
//
//	[0]   version byte (batchFormatVersion)
//	[1]   flags (bit0 = payload is zstd-compressed)
//	[2:]  payload: a sequence of [2-byte big-endian length][packet] records,
//	      optionally zstd-compressed as a whole.
//
// Only wire-v2 frames are accepted. The retired wire-v3 prototype is gone:
// capability negotiation now lives in NegotiatedTransport (transport/negotiated.go),
// inside the authenticated envelope.
const (
	batchFormatVersion = 0x02
	batchFlagZstd      = 0x01
	maxFrameBytes      = 1 << 20
	maxFrameRecords    = 1024
)

var (
	zstdEnc     *zstd.Encoder
	zstdEncFast *zstd.Encoder // for carriers whose bytes are cheap
	zstdDec     *zstd.Decoder
)

func init() {
	var err error
	// SpeedDefault (~level 3): far better ratio than LZ4 at a CPU cost that is
	// irrelevant next to the Yandex channel's latency. Single-shot EncodeAll /
	// DecodeAll are safe for concurrent use on a shared instance.
	zstdEnc, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		panic(fmt.Sprintf("zstd encoder init: %v", err))
	}
	zstdEncFast, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		panic(fmt.Sprintf("zstd fast encoder init: %v", err))
	}
	zstdDec, err = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		// Bound the damage from a malformed/hostile frame injected into the
		// shared document: cap decompressed memory.
		zstd.WithDecoderMaxMemory(8<<20),
	)
	if err != nil {
		panic(fmt.Sprintf("zstd decoder init: %v", err))
	}
}

// frameBatch concatenates packets into length-prefixed records.
func frameBatch(pkts [][]byte) []byte {
	total := 0
	for _, p := range pkts {
		total += 2 + len(p)
	}
	out := make([]byte, 0, total)
	var lenbuf [2]byte
	for _, p := range pkts {
		binary.BigEndian.PutUint16(lenbuf[:], uint16(len(p)))
		out = append(out, lenbuf[:]...)
		out = append(out, p...)
	}
	return out
}

// compressGate decides whether a batch is worth offering to zstd. Most of what
// a tunnel carries is TLS, which does not compress, and zstd then costs a
// third of the CPU of a busy node for nothing. After a batch that did not
// shrink by at least 3% the gate stops trying for a while, backing off
// exponentially while the traffic stays incompressible, and trying again as
// soon as a probe succeeds. The wire format is untouched: a batch is flagged
// compressed or not by itself. A gate belongs to one encoder goroutine.
type compressGate struct {
	skip    int // batches to send as they are before the next probe
	backoff int // length of the next skip window

	// cheapBytes marks a carrier whose bytes cost little (a plain TCP
	// connection) next to the CPU spent shrinking them: it compresses only
	// when that saves a good part of the batch, and at the fastest level.
	// The repeated envelope of every packet alone is worth a few percent of
	// any batch, which is not worth a third of a node's CPU.
	cheapBytes bool
}

const (
	gateMinSkip     = 4
	gateMaxSkip     = 128
	gateMinSaving   = 33 // compress only when it saves at least 1/33 (about 3%)
	gateCheapSaving = 6  // ... or 1/6 (about 17%) on a carrier with cheap bytes
)

// try reports whether this batch should be offered to the compressor.
func (g *compressGate) try() bool {
	if g == nil {
		return true
	}
	if g.skip > 0 {
		g.skip--
		return false
	}
	return true
}

// result feeds back what the compressor achieved for a batch it was offered.
func (g *compressGate) result(raw, compressed int) {
	if g == nil {
		return
	}
	div := gateMinSaving
	if g.cheapBytes {
		div = gateCheapSaving
	}
	if compressed+raw/div < raw {
		g.backoff = 0
		return
	}
	if g.backoff == 0 {
		g.backoff = gateMinSkip
	} else if g.backoff < gateMaxSkip {
		g.backoff *= 2
	}
	g.skip = g.backoff
}

// encodeBatch serializes packets into a single wire-v2 frame, compressing
// the whole batch with zstd only when that actually shrinks it.
func encodeBatch(pkts [][]byte) []byte { return encodeBatchGated(pkts, nil) }

// encodeBatchGated is encodeBatch with a compressGate (nil tries every batch).
// The frame is built in place, so a batch that is not compressed costs one
// allocation.
func encodeBatchGated(pkts [][]byte, g *compressGate) []byte {
	total := 2
	for _, p := range pkts {
		total += 2 + len(p)
	}
	out := getBuf(total)[:2]
	out[0], out[1] = batchFormatVersion, 0
	for _, p := range pkts {
		out = append(out, byte(len(p)>>8), byte(len(p)))
		out = append(out, p...)
	}
	if !g.try() {
		return out
	}
	framed := out[2:]
	enc := zstdEnc
	if g != nil && g.cheapBytes {
		enc = zstdEncFast
	}
	compressed := enc.EncodeAll(framed, nil)
	g.result(len(framed), len(compressed))
	if len(compressed) < len(framed) && (g == nil || !g.cheapBytes || len(compressed)+len(framed)/gateCheapSaving < len(framed)) {
		putBuf(out)
		z := make([]byte, 2, 2+len(compressed))
		z[0] = batchFormatVersion
		z[1] = batchFlagZstd
		return append(z, compressed...)
	}
	return out
}

// EncodeBatchFrame serializes packets into a wire-v2 frame for transports that
// carry the frame themselves instead of delegating to BatchedTransport - the
// Telemost SFU path wraps each frame in a VP8 keyframe rather than passing it
// up the transport stack. Such a transport must produce byte for byte what
// BatchedTransport would have produced, or the receiving side rejects it.
func EncodeBatchFrame(pkts [][]byte) []byte { return encodeBatch(pkts) }

// decodeBatch reverses encodeBatch, returning the original packets.
// Only wire-v2 frames are accepted; anything else is rejected.
func decodeBatch(data []byte) ([][]byte, error) { return decodeBatchOpt(data, false) }

// decodeBatchOpt is decodeBatch; with alias set an uncompressed frame is not
// copied: the packets are views of data, which the caller then owns for good
// (the plaintext an EncryptedTransport just opened, for one).
func decodeBatchOpt(data []byte, alias bool) ([][]byte, error) {
	if len(data) > maxFrameBytes+2 {
		return nil, fmt.Errorf("batch frame exceeds size limit")
	}
	if len(data) < 2 {
		return nil, fmt.Errorf("batch frame too short: %d bytes", len(data))
	}
	if data[0] != batchFormatVersion {
		return nil, fmt.Errorf("unknown batch version 0x%02x", data[0])
	}
	flags := data[1]
	if flags & ^byte(batchFlagZstd) != 0 {
		return nil, fmt.Errorf("unknown batch flags 0x%02x", flags)
	}
	payload := data[2:]

	framed := payload
	if flags&batchFlagZstd != 0 {
		var err error
		framed, err = zstdDec.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("zstd decode: %w", err)
		}
	}
	if len(framed) > maxFrameBytes {
		return nil, fmt.Errorf("decoded batch exceeds size limit")
	}

	// One allocation holds the whole batch: the packets are capacity-limited
	// views into it, so each is still its own to append to or keep. A
	// decompressed frame is already a fresh buffer; an uncompressed one aliases
	// the caller's, so it is copied once.
	slab := framed
	if flags&batchFlagZstd == 0 && !alias {
		slab = append(make([]byte, 0, len(framed)), framed...)
	}
	var pkts [][]byte
	for len(slab) > 0 {
		if len(pkts) >= maxFrameRecords {
			return nil, fmt.Errorf("batch exceeds record limit")
		}
		if len(slab) < 2 {
			return nil, fmt.Errorf("truncated length prefix")
		}
		n := int(binary.BigEndian.Uint16(slab[:2]))
		slab = slab[2:]
		if len(slab) < n {
			return nil, fmt.Errorf("truncated packet: need %d, have %d", n, len(slab))
		}
		pkts = append(pkts, slab[:n:n])
		slab = slab[n:]
	}
	return pkts, nil
}
