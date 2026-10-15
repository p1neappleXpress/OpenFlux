package tunnel

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

type socks5UDPConn struct {
	tcpControl net.Conn
	udpConn    *net.UDPConn
	header     []byte
}

func dialSOCKS5UDP(socksAddr, dest string) (net.Conn, error) {
	dHost, dPortStr, err := net.SplitHostPort(dest)
	if err != nil {
		return nil, err
	}
	dPort, err := strconv.Atoi(dPortStr)
	if err != nil {
		return nil, err
	}
	dip := net.ParseIP(dHost).To4()
	if dip == nil {
		return nil, fmt.Errorf("only IPv4 supported for UDP")
	}

	tcpConn, err := net.DialTimeout("tcp", socksAddr, 10*time.Second)
	if err != nil {
		return nil, err
	}

	// SOCKS5 auth: no auth
	if _, err := tcpConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		tcpConn.Close()
		return nil, err
	}
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(tcpConn, authResp); err != nil || authResp[0] != 0x05 || authResp[1] != 0x00 {
		tcpConn.Close()
		return nil, fmt.Errorf("socks5 auth failed: %v", authResp)
	}

	// UDP ASSOCIATE
	if _, err := tcpConn.Write([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		tcpConn.Close()
		return nil, err
	}
	resp := make([]byte, 10)
	if _, err := io.ReadFull(tcpConn, resp); err != nil || resp[1] != 0x00 {
		tcpConn.Close()
		return nil, fmt.Errorf("udp associate failed, resp: %v", resp)
	}

	sHost, _, _ := net.SplitHostPort(socksAddr)
	relayIP := net.IP(resp[4:8])
	if relayIP.IsUnspecified() || relayIP.String() == "0.0.0.0" {
		relayIP = net.ParseIP(sHost)
	}
	relayPort := binary.BigEndian.Uint16(resp[8:10])
	relayAddr := &net.UDPAddr{IP: relayIP, Port: int(relayPort)}

	uConn, err := net.DialUDP("udp", nil, relayAddr)
	if err != nil {
		tcpConn.Close()
		return nil, err
	}

	hdr := make([]byte, 10)
	hdr[0] = 0 // RSV
	hdr[1] = 0 // RSV
	hdr[2] = 0 // FRAG
	hdr[3] = 1 // ATYP IPv4
	copy(hdr[4:8], dip)
	binary.BigEndian.PutUint16(hdr[8:10], uint16(dPort))

	return &socks5UDPConn{
		tcpControl: tcpConn,
		udpConn:    uConn,
		header:     hdr,
	}, nil
}

func (c *socks5UDPConn) Read(b []byte) (int, error) {
	buf := make([]byte, 65535)
	n, err := c.udpConn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n < 10 {
		return 0, fmt.Errorf("socks5 udp packet too short")
	}
	atyp := buf[3]
	headerLen := 10
	if atyp == 3 {
		domainLen := int(buf[4])
		headerLen = 5 + domainLen + 2
	} else if atyp == 4 {
		headerLen = 22
	}
	if n < headerLen {
		return 0, fmt.Errorf("socks5 udp packet too short for header")
	}
	payload := buf[headerLen:n]
	copied := copy(b, payload)
	return copied, nil
}

func (c *socks5UDPConn) Write(b []byte) (int, error) {
	pkt := make([]byte, len(c.header)+len(b))
	copy(pkt, c.header)
	copy(pkt[len(c.header):], b)
	_, err := c.udpConn.Write(pkt)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *socks5UDPConn) Close() error {
	_ = c.tcpControl.Close()
	return c.udpConn.Close()
}

func (c *socks5UDPConn) LocalAddr() net.Addr                { return c.udpConn.LocalAddr() }
func (c *socks5UDPConn) RemoteAddr() net.Addr               { return c.udpConn.RemoteAddr() }
func (c *socks5UDPConn) SetDeadline(t time.Time) error      { return c.udpConn.SetDeadline(t) }
func (c *socks5UDPConn) SetReadDeadline(t time.Time) error  { return c.udpConn.SetReadDeadline(t) }
func (c *socks5UDPConn) SetWriteDeadline(t time.Time) error { return c.udpConn.SetWriteDeadline(t) }
