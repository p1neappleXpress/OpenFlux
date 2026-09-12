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
const (
	batchFormatVersion = 0x02
	batchFlagZstd      = 0x01
)

var (
	zstdEnc *zstd.Encoder
	zstdDec *zstd.Decoder
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

// encodeBatch serializes packets into a single wire frame, compressing the
// whole batch with zstd only when that actually shrinks it.
func encodeBatch(pkts [][]byte) []byte {
	framed := frameBatch(pkts)
	compressed := zstdEnc.EncodeAll(framed, nil)

	if len(compressed) < len(framed) {
		out := make([]byte, 2, 2+len(compressed))
		out[0] = batchFormatVersion
		out[1] = batchFlagZstd
		return append(out, compressed...)
	}
	out := make([]byte, 2, 2+len(framed))
	out[0] = batchFormatVersion
	out[1] = 0
	return append(out, framed...)
}

// decodeBatch reverses encodeBatch, returning the original packets.
func decodeBatch(data []byte) ([][]byte, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("batch frame too short: %d bytes", len(data))
	}
	if data[0] != batchFormatVersion {
		return nil, fmt.Errorf("unknown batch version 0x%02x", data[0])
	}
	flags := data[1]
	payload := data[2:]

	framed := payload
	if flags&batchFlagZstd != 0 {
		var err error
		framed, err = zstdDec.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("zstd decode: %w", err)
		}
	}

	var pkts [][]byte
	for len(framed) > 0 {
		if len(framed) < 2 {
			return nil, fmt.Errorf("truncated length prefix")
		}
		n := int(binary.BigEndian.Uint16(framed[:2]))
		framed = framed[2:]
		if len(framed) < n {
			return nil, fmt.Errorf("truncated packet: need %d, have %d", n, len(framed))
		}
		pkt := make([]byte, n)
		copy(pkt, framed[:n])
		pkts = append(pkts, pkt)
		framed = framed[n:]
	}
	return pkts, nil
}
