package transport

import (
	"bytes"
	"encoding/binary"
)

// FlowHash identifies the flow an IP packet belongs to, so MultiStreamTransport
// can keep every packet of one connection on the same inner stream. It returns
// false when data is not an IPv4/IPv6 packet.
//
// The hash is symmetric: both directions of a connection hash the same, so the
// client and the exit node pick the same document for it (when they list the
// same documents). A connection then depends on one document instead of two.
//
// Ports are used for TCP and UDP only, and never for IPv4 fragments: a
// non-first fragment carries no port numbers, and all fragments of a datagram
// have to hash the same.
func FlowHash(pkt []byte) (uint64, bool) {
	if len(pkt) == 0 {
		return 0, false
	}

	var (
		proto    byte
		src, dst []byte
		l4       []byte
	)
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return 0, false
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return 0, false
		}
		proto = pkt[9]
		src, dst = pkt[12:16], pkt[16:20]
		// MF flag or a non-zero fragment offset.
		if binary.BigEndian.Uint16(pkt[6:8])&0x3fff == 0 {
			l4 = pkt[ihl:]
		}
	case 6:
		if len(pkt) < 40 {
			return 0, false
		}
		proto = pkt[6]
		src, dst = pkt[8:24], pkt[24:40]
		l4 = pkt[40:]
	default:
		return 0, false
	}

	var sport, dport uint16
	if (proto == 6 || proto == 17) && len(l4) >= 4 {
		sport = binary.BigEndian.Uint16(l4[0:2])
		dport = binary.BigEndian.Uint16(l4[2:4])
	}

	// Canonical endpoint order makes the hash direction-independent.
	if c := bytes.Compare(src, dst); c > 0 || (c == 0 && sport > dport) {
		src, dst = dst, src
		sport, dport = dport, sport
	}

	const (
		fnvOffset = 14695981039346656037
		fnvPrime  = 1099511628211
	)
	h := uint64(fnvOffset)
	mixByte := func(b byte) {
		h ^= uint64(b)
		h *= fnvPrime
	}
	mixByte(proto)
	for _, b := range src {
		mixByte(b)
	}
	mixByte(byte(sport >> 8))
	mixByte(byte(sport))
	for _, b := range dst {
		mixByte(b)
	}
	mixByte(byte(dport >> 8))
	mixByte(byte(dport))

	// splitmix64 finalizer: FNV's low bits are weak, and the caller takes the
	// hash modulo a small stream count.
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h, true
}
