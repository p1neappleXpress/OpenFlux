package l3

import (
	"encoding/binary"
	"testing"
	"time"
)

func tcpPacket(src, dst [4]byte, srcPort, dstPort uint16, flags byte) []byte {
	p := make([]byte, 40)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8] = 64
	p[9] = 6
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	binary.BigEndian.PutUint16(p[20:22], srcPort)
	binary.BigEndian.PutUint16(p[22:24], dstPort)
	p[32] = 5 << 4
	p[33] = flags
	fixChecksums(p)
	return p
}

func TestOutboundTCPFlow(t *testing.T) {
	egress, remote := [4]byte{192, 168, 1, 10}, [4]byte{93, 184, 216, 34}
	syn := tcpPacket(egress, remote, 50000, 443, 0x02)
	f, ok := outboundTCPFlow(syn)
	if !ok || f != (sentFlow{srcPort: 50000, dstIP: ipU32(remote), dstPort: 443}) {
		t.Fatalf("SYN flow = %+v, %v", f, ok)
	}
	// The host's RST for the same flow must map to the same key.
	if rst, ok := outboundTCPFlow(tcpPacket(egress, remote, 50000, 443, 0x04)); !ok || rst != f {
		t.Fatalf("RST flow = %+v, %v; want %+v", rst, ok, f)
	}

	if _, ok := outboundTCPFlow(udpPacket(egress, remote, 50000, 53, []byte("x"))); ok {
		t.Fatal("UDP packet read as a TCP flow")
	}
	frag := tcpPacket(egress, remote, 50000, 443, 0x10)
	binary.BigEndian.PutUint16(frag[6:8], 3) // fragment offset 24 bytes: no ports
	if _, ok := outboundTCPFlow(frag); ok {
		t.Fatal("non-first fragment read as a TCP flow")
	}
	if _, ok := outboundTCPFlow(syn[:22]); ok {
		t.Fatal("truncated packet read as a TCP flow")
	}
}

func TestSentFlowsExpire(t *testing.T) {
	s := newSentFlows(time.Minute)
	f := sentFlow{srcPort: 50000, dstIP: 1, dstPort: 443}
	other := sentFlow{srcPort: 50001, dstIP: 1, dstPort: 443}
	t0 := time.Unix(1000, 0)

	s.record(f, t0)
	if !s.contains(f, t0.Add(time.Minute)) {
		t.Fatal("flow forgotten within its TTL")
	}
	if s.contains(other, t0) {
		t.Fatal("unsent flow reported as sent")
	}
	if s.contains(f, t0.Add(time.Minute+time.Nanosecond)) {
		t.Fatal("flow kept past its TTL")
	}

	// Every send pushes the deadline forward.
	s.record(f, t0.Add(50*time.Second))
	s.record(other, t0)
	s.expire(t0.Add(90 * time.Second))
	if s.len() != 1 || !s.contains(f, t0.Add(90*time.Second)) {
		t.Fatalf("after expire: len=%d, refreshed flow kept=%v", s.len(), s.contains(f, t0.Add(90*time.Second)))
	}
}
