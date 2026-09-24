package l3

import "testing"

type fakeBackend struct {
	egress [4]byte
	sent   [][]byte
}

func (f *fakeBackend) EgressIP() [4]byte { return f.egress }
func (f *fakeBackend) Send(pkt []byte) error {
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	f.sent = append(f.sent, cp)
	return nil
}
func (f *fakeBackend) Recv(cb func([]byte)) {}
func (f *fakeBackend) Close() error         { return nil }

// buildTCPPacket returns a minimal 40-byte IPv4/TCP packet (no options, no
// payload) with the given TCP flags byte.
func buildTCPPacket(srcIP, dstIP [4]byte, srcPort, dstPort uint16, flags byte) []byte {
	pkt := make([]byte, 40)
	pkt[0] = 0x45 // version 4, IHL 5 (20 bytes)
	pkt[2], pkt[3] = 0x00, 40 // total length
	pkt[8] = 64 // TTL
	pkt[9] = 6  // protocol: TCP
	copy(pkt[12:16], srcIP[:])
	copy(pkt[16:20], dstIP[:])

	pkt[20], pkt[21] = byte(srcPort>>8), byte(srcPort)
	pkt[22], pkt[23] = byte(dstPort>>8), byte(dstPort)
	pkt[32] = 0x50 // data offset: 5 words (20 bytes), no options
	pkt[33] = flags
	return pkt
}

// Before this fix, handleFromTransport special-cased a client-originated RST
// by counting it and returning immediately, before SNAT rewrite, conntrack
// insert, or backend.Send — the real destination server never saw the RST
// and kept the connection half-open. RST should be forwarded like any other
// packet; isTCPClosing already recognizes it and marks the conntrack entry
// dying so it expires in the short "closing" bucket instead of the 5-minute
// "established" one.
func TestHandleFromTransportForwardsClientRST(t *testing.T) {
	backend := &fakeBackend{egress: [4]byte{198, 51, 100, 1}}
	exit := &L3Exit{
		backend: backend,
		ct:      newConntrack(),
	}

	clientSrc := [4]byte{10, 10, 10, 2}
	realDst := [4]byte{93, 184, 216, 34}
	pkt := buildTCPPacket(clientSrc, realDst, 55555, 443, 0x04) // RST only

	exit.handleFromTransport(pkt)

	if len(backend.sent) != 1 {
		t.Fatalf("backend.Send was called %d times, want 1 (the RST must reach the real destination)", len(backend.sent))
	}

	k, ok := extractFlowKey(pkt) // pkt was rewritten in place (SNAT), but ports/proto are unaffected
	if !ok {
		t.Fatal("extractFlowKey failed on the (rewritten) packet")
	}
	entry, exists := exit.ct.get(k)
	if !exists {
		t.Fatal("conntrack entry was not created for the RST'd flow")
	}
	if !entry.dying {
		t.Fatal("conntrack entry was not marked dying after an RST, so it will sit in the 5-minute established bucket instead of the 15-second closing one")
	}
}
