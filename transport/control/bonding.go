package control

import (
	"encoding/binary"
	"fmt"
)

// BondingAckSpan is how many sequence numbers, up to the newest, one
// BondingAck reports. At the rates carriers reach (a few thousand packets
// a second) it covers well over the 50 ms between acks, so a lost ack is
// made up by the next one.
const BondingAckSpan = 512

// BondingAck is the receiver's report of which data packets arrived: bit i
// of Bits stands for sequence Newest-i. The sender credits each reported
// packet to the carrier it went out on, which is how it learns each
// carrier's real rate (see transport's bond scheduler).
type BondingAck struct {
	Newest uint64
	Bits   [BondingAckSpan / 64]uint64
}

const bondingAckSize = 8 + BondingAckSpan/8

// Set records seq as arrived. A newer seq shifts the map; one older than
// the span is not recorded.
func (a *BondingAck) Set(seq uint64) {
	if seq == 0 {
		return
	}
	if seq > a.Newest {
		a.shift(seq - a.Newest)
		a.Newest = seq
	}
	if d := a.Newest - seq; d < BondingAckSpan {
		a.Bits[d/64] |= 1 << (d % 64)
	}
}

// shift moves every bit n places older, dropping those that leave the span.
func (a *BondingAck) shift(n uint64) {
	if n >= BondingAckSpan {
		a.Bits = [BondingAckSpan / 64]uint64{}
		return
	}
	words, bits := int(n/64), n%64
	for i := len(a.Bits) - 1; i >= 0; i-- {
		var v uint64
		if j := i - words; j >= 0 {
			v = a.Bits[j] << bits
			if bits > 0 && j > 0 {
				v |= a.Bits[j-1] >> (64 - bits)
			}
		}
		a.Bits[i] = v
	}
}

// Has reports whether seq was recorded.
func (a BondingAck) Has(seq uint64) bool {
	if seq == 0 || seq > a.Newest {
		return false
	}
	d := a.Newest - seq
	return d < BondingAckSpan && a.Bits[d/64]&(1<<(d%64)) != 0
}

// Encode is the SubtypeBondingAck payload: Newest, then the map, big endian.
func (a BondingAck) Encode() []byte {
	out := make([]byte, bondingAckSize)
	binary.BigEndian.PutUint64(out, a.Newest)
	for i, w := range a.Bits {
		binary.BigEndian.PutUint64(out[8+8*i:], w)
	}
	return out
}

// DecodeBondingAck reads a SubtypeBondingAck payload.
func DecodeBondingAck(p []byte) (BondingAck, error) {
	var a BondingAck
	if len(p) != bondingAckSize {
		return a, fmt.Errorf("bonding ack: %d bytes, want %d", len(p), bondingAckSize)
	}
	a.Newest = binary.BigEndian.Uint64(p)
	for i := range a.Bits {
		a.Bits[i] = binary.BigEndian.Uint64(p[8+8*i:])
	}
	return a, nil
}
