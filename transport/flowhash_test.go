package transport

import (
	"encoding/binary"
	"testing"
)

func TestFlowHashSymmetric(t *testing.T) {
	out := tcpPacket(clientIP, serverIP, 40001, 443, 0)
	back := tcpPacket(serverIP, clientIP, 443, 40001, 0)
	h1, ok1 := FlowHash(out)
	h2, ok2 := FlowHash(back)
	if !ok1 || !ok2 || h1 != h2 {
		t.Fatalf("directions hash differently: %x/%v vs %x/%v", h1, ok1, h2, ok2)
	}
	other, _ := FlowHash(tcpPacket(clientIP, serverIP, 40002, 443, 0))
	if other == h1 {
		t.Fatal("different ports hash the same")
	}
}

// All fragments of one datagram must hash the same, although only the first
// one carries the ports.
func TestFlowHashIPv4FragmentsIgnorePorts(t *testing.T) {
	first := tcpPacket(clientIP, serverIP, 40001, 443, 0)
	binary.BigEndian.PutUint16(first[6:8], 0x2000)            // MF
	later := tcpPacket(clientIP, serverIP, 0xdead, 0xbeef, 0) // garbage where ports would be
	binary.BigEndian.PutUint16(later[6:8], 185)               // offset, no MF
	h1, _ := FlowHash(first)
	h2, _ := FlowHash(later)
	if h1 != h2 {
		t.Fatal("fragments of one datagram hash differently")
	}
}

func TestFlowHashIPv6(t *testing.T) {
	pkt := func(src, dst byte, sport, dport uint16) []byte {
		p := make([]byte, 60)
		p[0] = 0x60
		p[6] = 6
		p[8+15] = src
		p[24+15] = dst
		binary.BigEndian.PutUint16(p[40:42], sport)
		binary.BigEndian.PutUint16(p[42:44], dport)
		return p
	}
	h1, ok1 := FlowHash(pkt(1, 2, 5000, 443))
	h2, ok2 := FlowHash(pkt(2, 1, 443, 5000))
	if !ok1 || !ok2 || h1 != h2 {
		t.Fatal("IPv6 flow directions hash differently")
	}
}

func TestFlowHashRejectsNonIP(t *testing.T) {
	for _, p := range [][]byte{nil, {0x00}, {0x45, 0, 0}, make([]byte, 30)} {
		if _, ok := FlowHash(p); ok {
			t.Errorf("FlowHash accepted %x", p)
		}
	}
}
