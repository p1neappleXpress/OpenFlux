package l3

import "encoding/binary"

type flowKey struct {
	srcIP, dstIP     uint32
	srcPort, dstPort uint16
	proto            uint8
}

func extractFlowKey(pkt []byte) (flowKey, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return flowKey{}, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 {
		return flowKey{}, false
	}
	if pkt[9] != 6 {
		return flowKey{}, false
	}
	return flowKey{
		srcIP:   binary.BigEndian.Uint32(pkt[12:16]),
		dstIP:   binary.BigEndian.Uint32(pkt[16:20]),
		srcPort: binary.BigEndian.Uint16(pkt[ihl : ihl+2]),
		dstPort: binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4]),
		proto:   6,
	}, true
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
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+14 {
		return false
	}
	flags := pkt[ihl+13]
	return flags&0x01 != 0 || flags&0x04 != 0
}

func fixChecksums(pkt []byte) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return
	}
	pkt[10], pkt[11] = 0, 0
	ipSum := onesComplementSum(pkt[:20])
	pkt[10] = byte(ipSum >> 8)
	pkt[11] = byte(ipSum)

	if pkt[9] != 6 {
		return
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 {
		return
	}
	tcp := pkt[ihl:]
	tcp[16], tcp[17] = 0, 0
	var src, dst [4]byte
	copy(src[:], pkt[12:16])
	copy(dst[:], pkt[16:20])
	sum := tcpChecksum(tcp, src, dst)
	tcp[16] = byte(sum >> 8)
	tcp[17] = byte(sum)
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

func tcpChecksum(tcp []byte, src, dst [4]byte) uint16 {
	var sum uint32
	sum += uint32(src[0])<<8 | uint32(src[1])
	sum += uint32(src[2])<<8 | uint32(src[3])
	sum += uint32(dst[0])<<8 | uint32(dst[1])
	sum += uint32(dst[2])<<8 | uint32(dst[3])
	sum += uint32(6)
	sum += uint32(len(tcp))
	i := 0
	for ; i+1 < len(tcp); i += 2 {
		sum += uint32(tcp[i])<<8 | uint32(tcp[i+1])
	}
	if i < len(tcp) {
		sum += uint32(tcp[i]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(^sum)
}
