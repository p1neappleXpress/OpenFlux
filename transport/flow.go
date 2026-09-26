package transport

import (
	"encoding/binary"
	"hash/fnv"
)

// flowKeyBytes is a 14-byte key that identifies a TCP/UDP flow:
// srcIP(4) + dstIP(4) + srcPort(2) + dstPort(2) + proto(1) + pad(1).
//
// All-zero keys (non-IPv4 or too short packets) still hash deterministically,
// so the same packet always lands on the same transport.
type flowKeyBytes [14]byte

// extractFlowKeyBytes builds a flowKeyBytes from an IPv4 packet.
// For non-IPv4 or truncated packets it returns a zero key.
func extractFlowKeyBytes(pkt []byte) flowKeyBytes {
	var k flowKeyBytes
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return k
	}
	copy(k[0:4], pkt[12:16])
	copy(k[4:8], pkt[16:20])
	proto := pkt[9]
	k[12] = proto
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+4 || ihl < 20 {
		return k
	}
	if proto == 6 || proto == 17 {
		binary.BigEndian.PutUint16(k[8:10], binary.BigEndian.Uint16(pkt[ihl:ihl+2]))
		binary.BigEndian.PutUint16(k[10:12], binary.BigEndian.Uint16(pkt[ihl+2:ihl+4]))
	}
	return k
}

// flowHashBytes returns a stable FNV-1a hash of the flow key.
func flowHashBytes(k flowKeyBytes) uint64 {
	h := fnv.New64a()
	h.Write(k[:])
	return h.Sum64()
}
