package l3

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestUDPDoesNotCloseAsTCP(t *testing.T) {
	pkt := udpPacket([4]byte{10, 10, 10, 2}, [4]byte{1, 1, 1, 1}, 1000, 2000, []byte{0, 0, 0, 0, 0, 5})
	if isTCPClosing(pkt) || isTCPClosing(nil) {
		t.Fatal("non-TCP packet treated as a TCP close")
	}
}

func TestFlowRejectsMalformedTransportLengths(t *testing.T) {
	for _, n := range []uint16{0, 7, 9, 65535} {
		pkt := udpPacket([4]byte{}, [4]byte{}, 1000, 2000, nil)
		binary.BigEndian.PutUint16(pkt[24:26], n)
		if _, ok := extractFlowKey(pkt); ok {
			t.Fatalf("accepted UDP length %d", n)
		}
	}
	pkt := make([]byte, 40)
	pkt[0], pkt[9] = 0x45, 6
	binary.BigEndian.PutUint16(pkt[2:4], 40)
	if _, ok := extractFlowKey(pkt); ok {
		t.Fatal("accepted TCP data offset zero")
	}
	pkt[32] = 5 << 4
	if _, ok := extractFlowKey(pkt); !ok {
		t.Fatal("rejected valid TCP header")
	}
}

func TestUDPChecksumUsesDeclaredLength(t *testing.T) {
	src, dst := [4]byte{10, 0, 0, 1}, [4]byte{10, 0, 0, 2}
	pkt := udpPacket(src, dst, 1000, 2000, []byte("odd"))
	udpEnd := len(pkt)
	pkt = append(pkt, 0xaa, 0xbb)
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	fixChecksums(pkt)
	if transportChecksum(pkt[20:udpEnd], src, dst, 17) != 0 {
		t.Fatal("checksum incorrectly includes IP padding")
	}
}

func TestConntrackExpiresBeforeSweepAndStops(t *testing.T) {
	c := newConntrack()
	defer c.Close()
	k := flowKey{proto: 17, dstPort: 53}
	if !c.Insert(k) {
		t.Fatal("insert failed")
	}
	c.mu.Lock()
	c.entries[k].lastSeen = time.Now().Add(-ctTimeoutDNS - time.Second)
	c.mu.Unlock()
	if c.Exists(k) {
		t.Fatal("expired DNS flow is still accepted")
	}
	c.Close()
	if c.Insert(flowKey{proto: 6}) {
		t.Fatal("insert succeeded after shutdown")
	}
}
