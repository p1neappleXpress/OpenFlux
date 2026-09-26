package l3

import (
	"bytes"
	"encoding/binary"
	"openflux/transport"
	"testing"
	"time"
)

func icmpQuote(quote []byte, typ, code byte, egress [4]byte) []byte {
	p := make([]byte, 28+len(quote))
	p[0], p[8], p[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:16], []byte{203, 0, 113, 1})
	copy(p[16:20], egress[:])
	p[20], p[21] = typ, code
	binary.BigEndian.PutUint16(p[26:28], 1280)
	copy(p[28:], quote)
	binary.BigEndian.PutUint16(p[22:24], onesComplementSum(p[20:]))
	fixIPChecksum(p)
	return p
}

func TestICMPUDPQuoteTranslation(t *testing.T) {
	n, _ := testNAT(t)
	backend := &recordingBackend{}
	out := &recordingTransport{packets: make(chan []byte, 16)}
	ex := &L3Exit{backend: backend, trans: out, udp: n, ct: newConntrack()}
	defer ex.Stop()
	original := udpPacket(clientIPBytes, [4]byte{203, 0, 113, 7}, 50000, 443, bytes.Repeat([]byte{42}, 1200))
	ex.handleFromTransport(append([]byte(nil), original...))
	for _, length := range []int{28, len(backend.sent)} {
		for _, kind := range [][2]byte{{3, 4}, {3, 3}, {11, 0}, {12, 0}} {
			p := icmpQuote(backend.sent[:length], kind[0], kind[1], backend.EgressIP())
			ex.handleFromInternet(p)
			select {
			case got := <-out.packets:
				if !bytes.Equal(got[28:], original[:length]) || !bytes.Equal(got[16:20], clientIPBytes[:]) || onesComplementSum(got[:20]) != 0 || onesComplementSum(got[20:]) != 0 {
					t.Fatal("invalid NAT quote/checksums")
				}
			default:
				t.Fatal("ICMP not delivered")
			}
		}
	}
	for _, mutate := range []func([]byte){func(p []byte) { p[len(p)-1] ^= 1 }, func(p []byte) {
		p[20] = 5
		p[22], p[23] = 0, 0
		binary.BigEndian.PutUint16(p[22:24], onesComplementSum(p[20:]))
	}, func(p []byte) {
		p[28+22] ^= 1
		p[22], p[23] = 0, 0
		binary.BigEndian.PutUint16(p[22:24], onesComplementSum(p[20:]))
	}} {
		p := icmpQuote(backend.sent[:28], 3, 4, backend.EgressIP())
		mutate(p)
		ex.handleFromInternet(p)
		select {
		case <-out.packets:
			t.Fatal("invalid/unrelated ICMP accepted")
		default:
		}
	}
	n.mu.Lock()
	for _, m := range n.forward {
		m.lastSeen = time.Now().Add(-ctTimeoutUDP - time.Second)
	}
	n.mu.Unlock()
	if ex.translateICMP(icmpQuote(backend.sent[:28], 3, 4, backend.EgressIP())) {
		t.Fatal("expired flow accepted")
	}
}

func TestICMPTCPTruncatedQuote(t *testing.T) {
	n, _ := testNAT(t)
	ex := &L3Exit{backend: &recordingBackend{}, udp: n, ct: newConntrack()}
	defer ex.Stop()
	p := make([]byte, 40)
	p[0], p[8], p[9], p[32] = 0x45, 64, 6, 0x50
	binary.BigEndian.PutUint16(p[2:4], 40)
	copy(p[12:16], clientIPBytes[:])
	copy(p[16:20], []byte{203, 0, 113, 7})
	binary.BigEndian.PutUint16(p[20:22], 50123)
	binary.BigEndian.PutUint16(p[22:24], 443)
	fixChecksums(p)
	original := append([]byte(nil), p...)
	rewriteSNAT(p, ex.backend.EgressIP())
	fixChecksums(p)
	key, _ := extractFlowKey(p)
	ex.ct.Insert(key)
	for _, length := range []int{28, 40} {
		q := icmpQuote(p[:length], 3, 4, ex.backend.EgressIP())
		if !ex.translateICMP(q) || !bytes.Equal(q[28:], original[:length]) {
			t.Fatal("TCP quote not restored")
		}
	}
}

type mtuBackend struct {
	recordingBackend
	mtu       int
	fragments [][]byte
}

func (b *mtuBackend) Send(p []byte) error {
	if len(p) > b.mtu {
		return &PacketTooBigError{b.mtu}
	}
	b.fragments = append(b.fragments, append([]byte(nil), p...))
	return nil
}

func TestEgressMTUFeedbackAndFragmentation(t *testing.T) {
	for _, df := range []bool{true, false} {
		t.Run(map[bool]string{true: "DF", false: "fragment"}[df], func(t *testing.T) {
			n, _ := testNAT(t)
			b := &mtuBackend{mtu: 1280}
			out := &recordingTransport{packets: make(chan []byte, 4)}
			ex := &L3Exit{backend: b, trans: out, ct: newConntrack(), udp: n}
			defer ex.Stop()
			p := udpPacket(clientIPBytes, [4]byte{203, 0, 113, 7}, 50000, 443, bytes.Repeat([]byte{1}, 2000))
			if df {
				p[6] = 0x40
				fixIPChecksum(p)
			}
			original := append([]byte(nil), p...)
			ex.handleFromTransport(p)
			if df {
				select {
				case got := <-out.packets:
					if got[20] != 3 || got[21] != 4 || binary.BigEndian.Uint16(got[26:28]) != 1280 || !bytes.Equal(got[28:], original[:28]) || onesComplementSum(got[20:]) != 0 {
						t.Fatal("bad fragmentation-needed")
					}
				default:
					t.Fatal("no MTU feedback")
				}
				if len(n.forward) != 0 {
					t.Fatal("failed send leaked mapping")
				}
			} else {
				if len(b.fragments) != 2 {
					t.Fatalf("got %d fragments", len(b.fragments))
				}
				var r reassembler
				var got []byte
				for _, f := range b.fragments {
					got, _ = r.add(f, time.Now())
				}
				if got == nil || !validUDPChecksums(got) {
					t.Fatal("bad fragmented NAT datagram")
				}
				if binary.BigEndian.Uint16(b.fragments[0][4:6]) == 0 {
					t.Fatal("kernel could assign different fragment IDs")
				}
			}
		})
	}
}

func TestReassemblyReorderOverlapExpiryLimits(t *testing.T) {
	p := udpPacket(clientIPBytes, [4]byte{203, 0, 113, 7}, 50000, 443, bytes.Repeat([]byte{7}, 4000))
	binary.BigEndian.PutUint16(p[4:6], 12)
	fixIPChecksum(p)
	parts, err := fragmentIPv4(p, 1280)
	if err != nil {
		t.Fatal(err)
	}
	var r reassembler
	now := time.Now()
	var got []byte
	for i := len(parts) - 1; i >= 0; i-- {
		got, _ = r.add(parts[i], now)
	}
	if !bytes.Equal(got, p) || r.bytes != 0 || len(r.sets) != 0 {
		t.Fatal("reassembly failed/leaked")
	}
	r.add(parts[0], now)
	r.add(parts[0], now)
	if r.bytes != 0 || len(r.sets) != 0 {
		t.Fatal("overlap retained")
	}
	r.add(parts[0], now)
	r.add(parts[1], now.Add(fragmentLifetime))
	if len(r.sets) != 1 || r.bytes != len(parts[1]) {
		t.Fatal("expiry refreshed by fragments")
	}
	var bounded reassembler
	for i := 0; i < fragmentFlowLimit+20; i++ {
		f := append([]byte(nil), parts[0]...)
		binary.BigEndian.PutUint16(f[4:6], uint16(i))
		fixIPChecksum(f)
		bounded.add(f, now)
	}
	if len(bounded.sets) != fragmentFlowLimit || bounded.bytes > fragmentByteLimit {
		t.Fatal("unbounded reassembly")
	}
}

func FuzzIPv4Reassembly(f *testing.F) {
	f.Add(udpPacket(clientIPBytes, [4]byte{203, 0, 113, 7}, 1234, 443, []byte("seed")))
	f.Fuzz(func(t *testing.T, p []byte) {
		if p, ok := sliceIPv4(p); ok {
			var r reassembler
			out, ready := r.add(p, time.Now())
			if ready {
				if _, ok := sliceIPv4(out); !ok {
					t.Fatal("invalid result")
				}
			}
		}
	})
}

type limitedTransport struct{ recordingTransport }

func (t *limitedTransport) PeerParameters() (transport.PeerParameters, bool) {
	return transport.PeerParameters{Capabilities: transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP, MaxPacketSize: 1280}, true
}

func TestReturnPathNegotiatedMTU(t *testing.T) {
	for _, df := range []bool{false, true} {
		b := &recordingBackend{}
		out := &limitedTransport{recordingTransport{packets: make(chan []byte, 4)}}
		ex := &L3Exit{backend: b, trans: out}
		wire := udpPacket([4]byte{203, 0, 113, 7}, b.EgressIP(), 443, 40000, bytes.Repeat([]byte{3}, 2000))
		if df {
			wire[6] = 0x40
			fixIPChecksum(wire)
		}
		p := append([]byte(nil), wire...)
		copy(p[16:20], clientIPBytes[:])
		binary.BigEndian.PutUint16(p[22:24], 50000)
		fixChecksums(p)
		if err := ex.sendClient(p, wire); err != nil {
			t.Fatal(err)
		}
		if df {
			if b.sent == nil || b.sent[20] != 3 || b.sent[21] != 4 || !bytes.Equal(b.sent[16:20], wire[12:16]) || !bytes.Equal(b.sent[28:], wire[:28]) {
				t.Fatal("return MTU error did not quote original wire packet")
			}
		} else {
			var r reassembler
			var got []byte
			for len(out.packets) > 0 {
				f := <-out.packets
				if len(f) > 1280 {
					t.Fatal("oversized return fragment")
				}
				got, _ = r.add(f, time.Now())
			}
			if got == nil || !bytes.Equal(got[20:], p[20:]) || !validUDPChecksums(got) {
				t.Fatal("return fragments lost payload or checksums")
			}
		}
	}
}

func TestFragmentMalformedAndByteBudget(t *testing.T) {
	p := udpPacket(clientIPBytes, [4]byte{203, 0, 113, 7}, 1234, 443, bytes.Repeat([]byte{2}, 65500))
	parts, err := fragmentIPv4(p, 1280)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte){func(p []byte) { p[6] |= 0x40 }, func(p []byte) { p[6] |= 0x80 }, func(p []byte) { p[7] = 0xff; p[6] = 0x3f }} {
		f := append([]byte(nil), parts[0]...)
		mutate(f)
		fixIPChecksum(f)
		var r reassembler
		if _, ok := r.add(f, time.Now()); ok || r.bytes != 0 {
			t.Fatal("invalid fragment retained")
		}
	}
	var r reassembler
	// Exercise the global byte budget independently of the flow/part caps.
	for id := 0; id < fragmentFlowLimit; id++ {
		for _, p := range parts[:len(parts)-1] {
			f := append([]byte(nil), p...)
			binary.BigEndian.PutUint16(f[4:6], uint16(id))
			fixIPChecksum(f)
			r.add(f, time.Now())
		}
	}
	if r.bytes > fragmentByteLimit || len(r.sets) > fragmentFlowLimit {
		t.Fatal("resource budget exceeded")
	}
}
