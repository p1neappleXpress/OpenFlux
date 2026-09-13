package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type dnsPacket struct {
	src, dst [4]byte
	srcPort  uint16
	query    []byte
	message  dnsmessage.Message
}

func checksum(p []byte) uint16 {
	var sum uint32
	for len(p) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(p))
		p = p[2:]
	}
	if len(p) != 0 {
		sum += uint32(p[0]) << 8
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func udpChecksum(src, dst []byte, udp []byte) uint16 {
	p := make([]byte, 12+len(udp))
	copy(p[0:4], src)
	copy(p[4:8], dst)
	p[9] = 17
	binary.BigEndian.PutUint16(p[10:12], uint16(len(udp)))
	copy(p[12:], udp)
	return checksum(p)
}

func parseDNSPacket(p []byte) (q dnsPacket, ok bool) {
	if len(p) < 20 || p[0]>>4 != 4 || p[9] != 17 {
		return
	}
	hl := int(p[0]&15) * 4
	total := int(binary.BigEndian.Uint16(p[2:4]))
	// Drop fragmented UDP; the server cannot carry arbitrary UDP fragments.
	if hl < 20 || total > len(p) || total < hl+20 || binary.BigEndian.Uint16(p[6:8])&0x3fff != 0 {
		return
	}
	if checksum(p[:hl]) != 0 {
		return
	}
	udp := p[hl:total]
	ul := int(binary.BigEndian.Uint16(udp[4:6]))
	if ul < 20 || ul != len(udp) || binary.BigEndian.Uint16(udp[2:4]) != 53 {
		return
	}
	if binary.BigEndian.Uint16(udp[6:8]) != 0 && udpChecksum(p[12:16], p[16:20], udp) != 0 {
		return
	}
	copy(q.src[:], p[12:16])
	copy(q.dst[:], p[16:20])
	q.srcPort = binary.BigEndian.Uint16(udp[0:2])
	q.query = append([]byte(nil), udp[8:]...)
	if q.message.Unpack(q.query) != nil || q.message.Response || len(q.message.Questions) == 0 {
		return q, false
	}
	return q, true
}

func replyPacket(q dnsPacket, data []byte) []byte {
	p := make([]byte, 28+len(data))
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	p[8], p[9] = 64, 17
	copy(p[12:16], q.dst[:])
	copy(p[16:20], q.src[:])
	binary.BigEndian.PutUint16(p[10:12], checksum(p[:20]))
	udp := p[20:]
	binary.BigEndian.PutUint16(udp[0:2], 53)
	binary.BigEndian.PutUint16(udp[2:4], q.srcPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], data)
	c := udpChecksum(p[12:16], p[16:20], udp)
	if c == 0 {
		c = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], c)
	return p
}

func (s *Session) smallReply(q dnsPacket, code dnsmessage.RCode, truncated bool) []byte {
	m := dnsmessage.Message{
		Header: dnsmessage.Header{ID: q.message.ID, Response: true, OpCode: q.message.OpCode,
			RecursionDesired: q.message.RecursionDesired, RecursionAvailable: true,
			CheckingDisabled: q.message.CheckingDisabled, RCode: code, Truncated: truncated},
		Questions: q.message.Questions,
	}
	b, _ := m.Pack()
	if len(b) > s.mtu-28 {
		m.Questions = nil
		b, _ = m.Pack()
	}
	return b
}

func (s *Session) dnsExchange(q dnsPacket, address string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	defer cancel()
	c, err := s.dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if !s.track(c) {
		return nil, context.Canceled
	}
	defer s.release(c)
	deadline, _ := ctx.Deadline()
	c.SetDeadline(deadline)
	frame := make([]byte, 2+len(q.query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(q.query)))
	copy(frame[2:], q.query)
	if _, err = io.Copy(c, bytes.NewReader(frame)); err != nil {
		return nil, err
	}
	var size [2]byte
	if _, err = io.ReadFull(c, size[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(size[:]))
	if n < 12 {
		return nil, errors.New("short DNS response")
	}
	response := make([]byte, n)
	if _, err = io.ReadFull(c, response); err != nil {
		return nil, err
	}
	var m dnsmessage.Message
	if err = m.Unpack(response); err != nil {
		return nil, err
	}
	if !m.Response || m.ID != q.message.ID || m.OpCode != q.message.OpCode || len(m.Questions) != len(q.message.Questions) {
		return nil, errors.New("DNS response does not match query")
	}
	for i := range m.Questions {
		if m.Questions[i] != q.message.Questions[i] {
			return nil, errors.New("DNS question mismatch")
		}
	}
	limit := 512
	for _, rr := range q.message.Additionals {
		if rr.Header.Type == dnsmessage.TypeOPT {
			limit = int(rr.Header.Class)
			break
		}
	}
	if limit < 512 {
		limit = 512
	}
	if limit > s.mtu-28 {
		limit = s.mtu - 28
	}
	if len(response) > limit {
		response = s.smallReply(q, m.RCode, true)
	}
	return response, nil
}

func (s *Session) forwardDNS(q dnsPacket) {
	address := net.JoinHostPort(net.IP(q.dst[:]).String(), "53")
	fakeDNS := address == DNSAddress+":53"
	if fakeDNS {
		address = PrimaryDNS
	}
	response, err := s.dnsExchange(q, address)
	if err != nil && fakeDNS && s.ctx.Err() == nil {
		response, err = s.dnsExchange(q, SecondaryDNS)
	}
	if s.ctx.Err() != nil {
		return
	}
	if err != nil {
		s.noteError(fmt.Errorf("DNS through OpenFlux: %w", err))
		s.dnsFailed.Add(1)
		response = s.smallReply(q, dnsmessage.RCodeServerFailure, false)
	} else {
		s.dnsOK.Add(1)
	}
	s.sendPacket(replyPacket(q, response))
}
