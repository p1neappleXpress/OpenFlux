package l3

import (
	"encoding/binary"
	"errors"
	"openflux/transport"
	"sync/atomic"
	"time"
)

// PacketTooBigError carries a route MTU reported by the backend, not a guessed
// Internet path MTU. Zero retains RFC 1191's unknown-next-hop-MTU meaning.
type PacketTooBigError struct{ MTU int }

var fragmentID atomic.Uint32

func (e *PacketTooBigError) Error() string { return "IPv4 packet exceeds egress route MTU" }

func (t *L3Exit) sendNetwork(p []byte) error {
	err := t.backend.Send(p)
	var tooBig *PacketTooBigError
	if !errors.As(err, &tooBig) || binary.BigEndian.Uint16(p[6:8])&0x4000 != 0 {
		return err
	}
	fragments, fragErr := fragmentIPv4(p, tooBig.MTU)
	if fragErr != nil {
		return err
	}
	for _, f := range fragments {
		if err := t.backend.Send(f); err != nil {
			return err
		}
	}
	return nil
}

func fragmentIPv4(p []byte, mtu int) ([][]byte, error) {
	// Fragmenting headers with options requires copy-bit handling; never emit
	// incorrectly copied options. Such packets get an explicit size error.
	if len(p) < 20 || p[0] != 0x45 || isFragmentedIPv4(p) || p[6]&0x40 != 0 || mtu < 68 || mtu >= len(p) {
		return nil, errors.New("cannot fragment IPv4 packet")
	}
	chunk := (mtu - 20) &^ 7
	id := binary.BigEndian.Uint16(p[4:6])
	if id == 0 {
		for id == 0 {
			id = uint16(fragmentID.Add(1))
		}
	}
	var out [][]byte
	for off := 0; off < len(p)-20; off += chunk {
		size := min(chunk, len(p)-20-off)
		f := make([]byte, 20+size)
		copy(f, p[:20])
		binary.BigEndian.PutUint16(f[4:6], id)
		copy(f[20:], p[20+off:20+off+size])
		flags := uint16(off / 8)
		if off+size < len(p)-20 {
			flags |= 0x2000
		}
		binary.BigEndian.PutUint16(f[6:8], flags)
		binary.BigEndian.PutUint16(f[2:4], uint16(len(f)))
		fixIPChecksum(f)
		out = append(out, f)
	}
	return out, nil
}

func fixIPChecksum(p []byte) {
	ihl := int(p[0]&15) * 4
	p[10], p[11] = 0, 0
	binary.BigEndian.PutUint16(p[10:12], onesComplementSum(p[:ihl]))
}

func (t *L3Exit) reportSendError(original []byte, err error) {
	var tooBig *PacketTooBigError
	if !errors.As(err, &tooBig) {
		return
	}
	egress := t.backend.EgressIP()
	p := makeTooBig(original, egress, tooBig.MTU)
	if p == nil {
		return
	}
	if err := t.trans.Send(p); err != nil {
		t.sendToClientErrs.Add(1)
	} else {
		t.pktToTransport.Add(1)
	}
}

func makeTooBig(original []byte, source [4]byte, mtu int) []byte {
	ihl := int(original[0]&15) * 4
	// Never generate an ICMP error about another error or a multicast packet.
	if len(original) < ihl+8 || original[9] == 1 || original[16] >= 224 || original[12] >= 224 || original[12] == 0 {
		return nil
	}
	p := make([]byte, 28+ihl+8)
	p[0], p[8], p[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:16], source[:])
	copy(p[16:20], original[12:16])
	p[20], p[21] = 3, 4
	if mtu >= 68 && mtu <= 65535 {
		binary.BigEndian.PutUint16(p[26:28], uint16(mtu))
	}
	copy(p[28:], original[:ihl+8])
	binary.BigEndian.PutUint16(p[22:24], onesComplementSum(p[20:]))
	fixIPChecksum(p)
	return p
}

// Respect the negotiated receive limit on the return path as well. Feedback
// to an Internet sender must quote the packet BEFORE reverse NAT, otherwise
// its socket cannot associate the error with the actual flow.
func (t *L3Exit) sendClient(p, wire []byte) error {
	if n, ok := t.trans.(transport.PeerParameterProvider); ok {
		if limits, ready := n.PeerParameters(); ready && len(p) > limits.MaxPacketSize {
			if p[6]&0x40 != 0 {
				feedback := makeTooBig(wire, t.backend.EgressIP(), limits.MaxPacketSize)
				if feedback != nil {
					return t.backend.Send(feedback)
				}
				return &PacketTooBigError{MTU: limits.MaxPacketSize}
			}
			parts, err := fragmentIPv4(p, limits.MaxPacketSize)
			if err != nil {
				return err
			}
			for _, part := range parts {
				if err := t.trans.Send(part); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return t.trans.Send(p)
}

// translateICMP accepts only checksum-valid errors quoting a live outbound
// TCP/UDP flow. The quote can be truncated to the IP header + eight bytes.
// Redirects, echo traffic and unrelated host traffic are not forwarded.
func (t *L3Exit) translateICMP(p []byte) bool {
	ihl := int(p[0]&15) * 4
	if isFragmentedIPv4(p) || len(p) < ihl+8+28 || onesComplementSum(p[:ihl]) != 0 || onesComplementSum(p[ihl:]) != 0 {
		return false
	}
	typ, code := p[ihl], p[ihl+1]
	if !((typ == 3 && code <= 15) || (typ == 11 && code <= 1) || (typ == 12 && code <= 2)) {
		return false
	}
	q := p[ihl+8:]
	qh := int(q[0]&15) * 4
	if q[0]>>4 != 4 || qh < 20 || len(q) < qh+8 || int(binary.BigEndian.Uint16(q[2:4])) < qh+8 || binary.BigEndian.Uint16(q[6:8])&0x1fff != 0 || onesComplementSum(q[:qh]) != 0 {
		return false
	}
	k := flowKey{srcIP: binary.BigEndian.Uint32(q[12:16]), dstIP: binary.BigEndian.Uint32(q[16:20]), srcPort: binary.BigEndian.Uint16(q[qh : qh+2]), dstPort: binary.BigEndian.Uint16(q[qh+2 : qh+4]), proto: q[9]}
	if k.srcIP != ipU32(t.backend.EgressIP()) {
		return false
	}
	original := k
	if k.proto == 17 {
		t.udp.mu.Lock()
		m := t.udp.reverse[reverseKey(k)]
		if m == nil || t.udp.closed || t.udp.expired(m, time.Now()) {
			t.udp.mu.Unlock()
			return false
		}
		original = m.client
		t.udp.mu.Unlock()
	} else if k.proto == 6 && t.ct.Exists(k) {
		original.srcIP = ipU32(clientIPBytes)
	} else {
		return false
	}
	// Incrementally repair a quoted transport checksum, which may cover bytes
	// not included in the ICMP quote (RFC 1624). Preserve disabled UDP sums.
	offset := qh + 6
	if k.proto == 6 {
		offset = qh + 16
	}
	if len(q) >= offset+2 && (k.proto != 17 || binary.BigEndian.Uint16(q[offset:offset+2]) != 0) {
		sum := binary.BigEndian.Uint16(q[offset : offset+2])
		for _, pair := range [][2]uint16{{uint16(k.srcIP >> 16), uint16(original.srcIP >> 16)}, {uint16(k.srcIP), uint16(original.srcIP)}, {k.srcPort, original.srcPort}} {
			sum = adjustChecksum(sum, pair[0], pair[1])
		}
		if k.proto == 17 && sum == 0 {
			sum = 0xffff
		}
		binary.BigEndian.PutUint16(q[offset:offset+2], sum)
	}
	binary.BigEndian.PutUint32(q[12:16], original.srcIP)
	binary.BigEndian.PutUint16(q[qh:qh+2], original.srcPort)
	fixIPChecksum(q)
	binary.BigEndian.PutUint32(p[16:20], original.srcIP)
	p[ihl+2], p[ihl+3] = 0, 0
	binary.BigEndian.PutUint16(p[ihl+2:ihl+4], onesComplementSum(p[ihl:]))
	fixIPChecksum(p)
	return true
}

func adjustChecksum(sum, old, value uint16) uint16 {
	s := uint32(^sum) + uint32(^old) + uint32(value)
	for s>>16 != 0 {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}
