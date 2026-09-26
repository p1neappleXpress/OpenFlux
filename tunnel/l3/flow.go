package l3

import "encoding/binary"

type flowKey struct {
	srcIP, dstIP     uint32
	srcPort, dstPort uint16
	proto            uint8
}

func extractFlowKey(pkt []byte) (flowKey, bool) {
	var ok bool
	pkt, ok = sliceIPv4(pkt)
	if !ok {
		return flowKey{}, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || isFragmentedIPv4(pkt) {
		return flowKey{}, false
	}
	proto := pkt[9]
	minHeader := 0
	switch proto {
	case 6:
		minHeader = 20
	case 17:
		minHeader = 8
	default:
		return flowKey{}, false
	}
	if len(pkt) < ihl+minHeader {
		return flowKey{}, false
	}
	if proto == 17 {
		n := int(binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6]))
		if n < 8 || n > len(pkt)-ihl {
			return flowKey{}, false
		}
	} else {
		n := int(pkt[ihl+12]>>4) * 4
		if n < 20 || n > len(pkt)-ihl {
			return flowKey{}, false
		}
	}
	return flowKey{
		srcIP:   binary.BigEndian.Uint32(pkt[12:16]),
		dstIP:   binary.BigEndian.Uint32(pkt[16:20]),
		srcPort: binary.BigEndian.Uint16(pkt[ihl : ihl+2]),
		dstPort: binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4]),
		proto:   proto,
	}, true
}

func isFragmentedIPv4(pkt []byte) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	frag := binary.BigEndian.Uint16(pkt[6:8])
	return frag&0x3fff != 0 // MF flag or a non-zero fragment offset.
}

func reverseKey(k flowKey) flowKey {
	return flowKey{
		srcIP:   k.dstIP,
		dstIP:   k.srcIP,
		srcPort: k.dstPort,
		dstPort: k.srcPort,
		proto:   k.proto,
	}
}

func rewriteSNAT(pkt []byte, newSrc [4]byte) {
	copy(pkt[12:16], newSrc[:])
}

func rewriteDNAT(pkt []byte, newDst [4]byte) {
	copy(pkt[16:20], newDst[:])
}

func isTCPClosing(pkt []byte) bool {
	if _, ok := extractFlowKey(pkt); !ok || pkt[9] != 6 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+14 {
		return false
	}
	flags := pkt[ihl+13]
	return flags&0x01 != 0 || flags&0x04 != 0
}

func isTCPRST(pkt []byte) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 || pkt[9] != 6 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+14 {
		return false
	}
	return pkt[ihl+13]&0x04 != 0
}

func fixChecksums(pkt []byte) {
	var ok bool
	pkt, ok = sliceIPv4(pkt)
	if !ok || isFragmentedIPv4(pkt) {
		return
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return
	}
	pkt[10], pkt[11] = 0, 0
	ipSum := onesComplementSum(pkt[:ihl])
	pkt[10] = byte(ipSum >> 8)
	pkt[11] = byte(ipSum)

	proto := pkt[9]
	segment := pkt[ihl:]
	checksumOffset := 0
	switch proto {
	case 6:
		if len(segment) < 20 {
			return
		}
		checksumOffset = 16
	case 17:
		if len(segment) < 8 {
			return
		}
		n := int(binary.BigEndian.Uint16(segment[4:6]))
		if n < 8 || n > len(segment) {
			return
		}
		segment = segment[:n]
		checksumOffset = 6
		// A zero UDP checksum is valid for IPv4 and must remain disabled.
		if segment[checksumOffset] == 0 && segment[checksumOffset+1] == 0 {
			return
		}
	default:
		return
	}
	segment[checksumOffset], segment[checksumOffset+1] = 0, 0
	var src, dst [4]byte
	copy(src[:], pkt[12:16])
	copy(dst[:], pkt[16:20])
	sum := transportChecksum(segment, src, dst, proto)
	if proto == 17 && sum == 0 {
		sum = 0xffff
	}
	segment[checksumOffset] = byte(sum >> 8)
	segment[checksumOffset+1] = byte(sum)
}

func onesComplementSum(b []byte) uint16 {
	var sum uint32
	i := 0
	for ; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if i < len(b) {
		sum += uint32(b[i]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(^sum)
}

func transportChecksum(segment []byte, src, dst [4]byte, proto uint8) uint16 {
	var sum uint32
	sum += uint32(src[0])<<8 | uint32(src[1])
	sum += uint32(src[2])<<8 | uint32(src[3])
	sum += uint32(dst[0])<<8 | uint32(dst[1])
	sum += uint32(dst[2])<<8 | uint32(dst[3])
	sum += uint32(proto)
	sum += uint32(len(segment))
	i := 0
	for ; i+1 < len(segment); i += 2 {
		sum += uint32(segment[i])<<8 | uint32(segment[i+1])
	}
	if i < len(segment) {
		sum += uint32(segment[i]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(^sum)
}

// Call only after extractFlowKey validated the IPv4 and UDP lengths. Tunnel
// input has no checksum-offload metadata, so validate before applying SNAT.
// Do not apply this to Linux raw receive without accounting for offload (e.g.
// loopback UDP can arrive with a partial checksum).
func validUDPChecksums(pkt []byte) bool {
	ihl := int(pkt[0]&0x0f) * 4
	if onesComplementSum(pkt[:ihl]) != 0 {
		return false
	}
	udp := pkt[ihl:]
	if binary.BigEndian.Uint16(udp[6:8]) == 0 {
		return true // IPv4 explicitly permits an omitted UDP checksum.
	}
	var src, dst [4]byte
	copy(src[:], pkt[12:16])
	copy(dst[:], pkt[16:20])
	return transportChecksum(udp[:binary.BigEndian.Uint16(udp[4:6])], src, dst, 17) == 0
}
