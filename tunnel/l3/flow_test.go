package l3

import (
	"encoding/binary"
	"testing"
)

func udpPacket(src, dst [4]byte, srcPort, dstPort uint16, payload []byte) []byte {
	pkt := make([]byte, 20+8+len(payload))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = 17
	copy(pkt[12:16], src[:])
	copy(pkt[16:20], dst[:])
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], dstPort)
	binary.BigEndian.PutUint16(pkt[24:26], uint16(8+len(payload)))
	copy(pkt[28:], payload)
	// Set a non-zero placeholder so fixChecksums enables the IPv4 UDP checksum.
	pkt[26] = 0xff
	pkt[27] = 0xff
	fixChecksums(pkt)
	return pkt
}

func TestExtractUDPFlowKey(t *testing.T) {
	pkt := udpPacket([4]byte{10, 10, 10, 2}, [4]byte{1, 1, 1, 1}, 53000, 53, []byte("query"))
	k, ok := extractFlowKey(pkt)
	if !ok {
		t.Fatal("UDP flow was rejected")
	}
	if k.proto != 17 || k.srcPort != 53000 || k.dstPort != 53 {
		t.Fatalf("unexpected flow key: %+v", k)
	}
}

func TestUDPChecksumsAfterNATRewrite(t *testing.T) {
	pkt := udpPacket([4]byte{10, 10, 10, 2}, [4]byte{8, 8, 8, 8}, 42000, 53, []byte("payload"))
	rewriteSNAT(pkt, [4]byte{192, 0, 2, 10})
	fixChecksums(pkt)
	if got := onesComplementSum(pkt[:20]); got != 0 {
		t.Fatalf("IPv4 checksum verification = %#04x, want 0", got)
	}
	var src, dst [4]byte
	copy(src[:], pkt[12:16])
	copy(dst[:], pkt[16:20])
	if got := transportChecksum(pkt[20:], src, dst, 17); got != 0 {
		t.Fatalf("UDP checksum verification = %#04x, want 0", got)
	}
}

func TestFragmentedUDPIsRejected(t *testing.T) {
	pkt := udpPacket([4]byte{10, 10, 10, 2}, [4]byte{1, 1, 1, 1}, 1000, 2000, nil)
	binary.BigEndian.PutUint16(pkt[6:8], 0x2000)
	if _, ok := extractFlowKey(pkt); ok {
		t.Fatal("fragmented UDP packet unexpectedly produced a flow key")
	}
}

func TestZeroUDPChecksumRemainsDisabled(t *testing.T) {
	pkt := udpPacket([4]byte{10, 10, 10, 2}, [4]byte{1, 1, 1, 1}, 1000, 2000, nil)
	pkt[26], pkt[27] = 0, 0
	rewriteSNAT(pkt, [4]byte{192, 0, 2, 10})
	fixChecksums(pkt)
	if pkt[26] != 0 || pkt[27] != 0 {
		t.Fatalf("zero UDP checksum changed to %02x%02x", pkt[26], pkt[27])
	}
}
