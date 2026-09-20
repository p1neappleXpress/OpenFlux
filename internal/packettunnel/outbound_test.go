package packettunnel

import (
	"encoding/binary"
	"testing"
)

func testIPv4UDP(port uint16) []byte {
	p := make([]byte, 31)
	p[0], p[9] = 0x45, 17
	binary.BigEndian.PutUint16(p[2:4], 28)
	binary.BigEndian.PutUint16(p[22:24], port)
	binary.BigEndian.PutUint16(p[24:26], 8)
	return p
}

func TestClassifyIPv4UDP(t *testing.T) {
	for _, tc := range []struct {
		name string
		pkt  []byte
		udp  bool
		want OutboundAction
	}{
		{"legacy exit rejects", testIPv4UDP(443), false, RejectUDP},
		{"updated exit forwards", testIPv4UDP(443), true, Forward},
		{"DNS stays local", testIPv4UDP(53), true, ResolveDNS},
		{"short packet", []byte{0x45}, true, Drop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, pkt := ClassifyIPv4(tc.pkt, tc.udp)
			if got != tc.want {
				t.Fatalf("action = %v, want %v", got, tc.want)
			}
			if got != Drop && len(pkt) != 28 {
				t.Fatalf("packet length = %d, want 28", len(pkt))
			}
		})
	}
	for _, mutate := range []func([]byte){
		func(p []byte) { p[0] = 0x65 },
		func(p []byte) { binary.BigEndian.PutUint16(p[2:4], 32) },
		func(p []byte) { binary.BigEndian.PutUint16(p[6:8], 0x2000) },
		func(p []byte) { binary.BigEndian.PutUint16(p[24:26], 7) },
		func(p []byte) { binary.BigEndian.PutUint16(p[24:26], 20) },
	} {
		p := testIPv4UDP(443)
		mutate(p)
		if action, _ := ClassifyIPv4(p, true); action != Drop {
			t.Fatalf("malformed packet was accepted: %v", action)
		}
	}
}
