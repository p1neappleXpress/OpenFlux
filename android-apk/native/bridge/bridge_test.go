package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// A real loopback SOCKS5 server, with no Internet connections. Tests observe
// the exact CONNECT destination and supply the remote service's response.
func socksServer(t *testing.T, handler func(net.Conn, string)) (int, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requested := make(chan string, 64)
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	var workers sync.WaitGroup
	acceptedDone := make(chan struct{})
	go func() {
		defer close(acceptedDone)
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			connections[c] = true
			mu.Unlock()
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer c.Close()
				defer func() { mu.Lock(); delete(connections, c); mu.Unlock() }()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				var h [2]byte
				if _, e := io.ReadFull(c, h[:]); e != nil {
					return
				}
				if h[0] != 5 {
					t.Error("expected SOCKS5")
					return
				}
				methods := make([]byte, int(h[1]))
				if _, e := io.ReadFull(c, methods); e != nil {
					return
				}
				c.Write([]byte{5, 0})
				var req [4]byte
				if _, e := io.ReadFull(c, req[:]); e != nil {
					return
				}
				if req != [4]byte{5, 1, 0, 1} {
					t.Errorf("expected IPv4 CONNECT, got %v", req)
					return
				}
				var dst [6]byte
				if _, e := io.ReadFull(c, dst[:]); e != nil {
					return
				}
				address := net.JoinHostPort(net.IP(dst[:4]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(dst[4:]))))
				requested <- address
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				handler(c, address)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-acceptedDone
		mu.Lock()
		for c := range connections {
			c.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return listener.Addr().(*net.TCPAddr).Port, requested
}

// SOCK_DGRAM preserves TUN packet boundaries without CAP_NET_ADMIN or Android.
func packetSession(t *testing.T, port int) (*Session, *os.File) {
	t.Helper()
	return packetSessionAtMTU(t, port, MTU)
}

func packetSessionAtMTU(t *testing.T, port, mtu int) (*Session, *os.File) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(pair[0], port, mtu)
	unix.Close(pair[0]) // Proves the native bridge owns a duplicate, not Java's FD.
	if err != nil {
		unix.Close(pair[1])
		t.Fatal(err)
	}
	peer := os.NewFile(uintptr(pair[1]), "test-tun-peer")
	done := make(chan error, 1)
	go func() { done <- s.Run() }()
	t.Cleanup(func() {
		s.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("TUN session did not stop")
		}
		peer.Close()
	})
	return s, peer
}

func clientStack(t *testing.T, peer *os.File) *stack.Stack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	link := channel.New(512, MTU, "")
	if e := st.CreateNIC(1, link); e != nil {
		t.Fatal(e)
	}
	address := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{10, 77, 0, 1}), PrefixLen: 30}}
	if e := st.AddProtocolAddress(1, address, stack.AddressProperties{}); e != nil {
		t.Fatal(e)
	}
	st.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		for {
			p := link.ReadContext(ctx)
			if p == nil {
				return
			}
			v := p.ToView()
			_, err := peer.Write(v.AsSlice())
			v.Release()
			p.DecRef()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer pumps.Done()
		buf := make([]byte, 65536)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				return
			}
			pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
			link.InjectInbound(ipv4.ProtocolNumber, pb)
			pb.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		peer.Close()
		link.Close()
		st.Close()
		pumps.Wait()
		st.Wait()
	})
	return st
}

func TestTCPThroughSOCKSLargeTransferAndHalfClose(t *testing.T) {
	payload := bytes.Repeat([]byte("OpenFlux/TUN:1234567890\n"), 12000)
	port, requests := socksServer(t, func(c net.Conn, address string) {
		data, err := io.ReadAll(c) // Require half-close before replying.
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(data, payload) {
			t.Error("SOCKS destination did not receive full request")
		}
		c.Write(append([]byte("response:"), data...))
	})
	s, peer := packetSession(t, port)
	st := clientStack(t, peer)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, st, tcpip.FullAddress{NIC: 1,
		Addr: tcpip.AddrFrom4([4]byte{203, 0, 113, 7}), Port: 443}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err = io.Copy(c, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, append([]byte("response:"), payload...)) {
		t.Fatal("TCP reply corrupted or truncated")
	}
	if address := <-requests; address != "203.0.113.7:443" {
		t.Fatal(address)
	}
	if s.tcpOK.Load() != 1 || s.down.Load() <= uint64(len(payload)) {
		t.Fatal(s.Stats())
	}
}

func dnsQuery(t *testing.T) dnsPacket {
	t.Helper()
	name, err := dnsmessage.NewName("example.org.")
	if err != nil {
		t.Fatal(err)
	}
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: 0x1234, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	data, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return dnsPacket{src: [4]byte{10, 77, 0, 1}, dst: [4]byte{10, 77, 0, 2}, srcPort: 45123, query: data, message: m}
}

// Independent test packet encoder: the production encoder only creates replies.
func queryIPPacket(q dnsPacket, destinationPort uint16) []byte {
	p := make([]byte, 28+len(q.query))
	p[0], p[8], p[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:16], q.src[:])
	copy(p[16:20], q.dst[:])
	binary.BigEndian.PutUint16(p[10:12], checksum(p[:20]))
	binary.BigEndian.PutUint16(p[20:22], q.srcPort)
	binary.BigEndian.PutUint16(p[22:24], destinationPort)
	binary.BigEndian.PutUint16(p[24:26], uint16(len(p)-20))
	copy(p[28:], q.query)
	// Zero UDP checksum is legal for IPv4 input.
	return p
}

func readDNSReply(t *testing.T, peer *os.File, q dnsPacket) dnsmessage.Message {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 65536)
	n, err := peer.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	p := buf[:n]
	if n < 40 || n > MTU || int(binary.BigEndian.Uint16(p[2:4])) != n {
		t.Fatalf("bad IP length: %d", n)
	}
	if !bytes.Equal(p[12:16], q.dst[:]) || !bytes.Equal(p[16:20], q.src[:]) {
		t.Fatal("wrong response endpoints")
	}
	if checksum(p[:20]) != 0 || udpChecksum(p[12:16], p[16:20], p[20:]) != 0 {
		t.Fatal("bad reply checksums")
	}
	if binary.BigEndian.Uint16(p[20:22]) != 53 || binary.BigEndian.Uint16(p[22:24]) != q.srcPort {
		t.Fatal("wrong UDP ports")
	}
	var m dnsmessage.Message
	if err := m.Unpack(p[28:]); err != nil {
		t.Fatal(err)
	}
	if !m.Response || m.ID != q.message.ID || len(m.Questions) != 1 || m.Questions[0] != q.message.Questions[0] {
		t.Fatal("mismatched DNS response")
	}
	return m
}

func dnsResponder(t *testing.T, large bool) func(net.Conn, string) {
	return func(c net.Conn, address string) {
		var size [2]byte
		if _, e := io.ReadFull(c, size[:]); e != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, e := io.ReadFull(c, q); e != nil {
			return
		}
		var m dnsmessage.Message
		if err := m.Unpack(q); err != nil {
			t.Error(err)
			return
		}
		m.Response, m.RecursionAvailable = true, true
		m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{
			Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 9}}}}
		if large {
			m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{
				Name: m.Questions[0].Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60},
				Body: &dnsmessage.TXTResource{TXT: []string{strings.Repeat("a", 250), strings.Repeat("b", 250), strings.Repeat("c", 250)}}})
		}
		data, err := m.Pack()
		if err != nil {
			t.Error(err)
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(data)))
		c.Write(size[:])
		c.Write(data)
	}
}

func TestDNSUDPOverSOCKSTCP(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("truncated=%t", large), func(t *testing.T) {
			port, requests := socksServer(t, dnsResponder(t, large))
			s, peer := packetSession(t, port)
			q := dnsQuery(t)
			if _, err := peer.Write(queryIPPacket(q, 53)); err != nil {
				t.Fatal(err)
			}
			m := readDNSReply(t, peer, q)
			if m.Truncated != large || m.RCode != dnsmessage.RCodeSuccess {
				t.Fatal(m.Header)
			}
			if !large && len(m.Answers) != 1 {
				t.Fatal("missing A answer")
			}
			if address := <-requests; address != PrimaryDNS {
				t.Fatal(address)
			}
			if s.dnsOK.Load() != 1 {
				t.Fatal(s.Stats())
			}
		})
	}
}

func TestDNSExplicitResolverPreserved(t *testing.T) {
	port, requests := socksServer(t, dnsResponder(t, false))
	_, peer := packetSession(t, port)
	q := dnsQuery(t)
	q.dst = [4]byte{8, 8, 8, 8}
	peer.Write(queryIPPacket(q, 53))
	readDNSReply(t, peer, q)
	if address := <-requests; address != "8.8.8.8:53" {
		t.Fatal(address)
	}
}

func TestDNSTCPRetryMapsVirtualResolver(t *testing.T) {
	port, requests := socksServer(t, dnsResponder(t, false))
	_, peer := packetSession(t, port)
	st := clientStack(t, peer)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, st, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 77, 0, 2}), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	q := dnsQuery(t)
	frame := make([]byte, 2+len(q.query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(q.query)))
	copy(frame[2:], q.query)
	c.Write(frame)
	var size [2]byte
	if _, err = io.ReadFull(c, size[:]); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err = io.ReadFull(c, data); err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err = m.Unpack(data); err != nil || !m.Response || m.ID != q.message.ID {
		t.Fatal("invalid DNS TCP response", err)
	}
	if address := <-requests; address != PrimaryDNS {
		t.Fatal(address)
	}
}

func TestDNSFailureReturnsServfail(t *testing.T) {
	// Refuse both upstream DNS connections via an unused loopback port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	s, peer := packetSession(t, port)
	q := dnsQuery(t)
	peer.Write(queryIPPacket(q, 53))
	m := readDNSReply(t, peer, q)
	if m.RCode != dnsmessage.RCodeServerFailure || s.dnsFailed.Load() != 1 {
		t.Fatal(m.Header, s.Stats())
	}
}

func TestUnsupportedUDPIPv6AndMalformedDNSDoNotConnect(t *testing.T) {
	port, requests := socksServer(t, func(c net.Conn, address string) { t.Errorf("unsupported packet caused CONNECT %s", address) })
	s, peer := packetSession(t, port)
	q := dnsQuery(t)
	packets := [][]byte{queryIPPacket(q, 443), make([]byte, 48), queryIPPacket(q, 53), queryIPPacket(q, 53)}
	packets[1][0] = 0x60
	packets[2][10] ^= 0xff                      // Bad IPv4 checksum.
	packets[3][26], packets[3][27] = 0x12, 0x34 // Bad nonzero UDP checksum.
	for _, p := range packets {
		if _, err := peer.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for s.dropped.Load() < uint64(len(packets)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.dropped.Load() != uint64(len(packets)) {
		t.Fatal(s.Stats())
	}
	select {
	case request := <-requests:
		t.Fatal(request)
	default:
	}
}

func TestStopBeforeRunAndRepeatedSessions(t *testing.T) {
	for i := 0; i < 8; i++ {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		s, err := New(pair[0], 1080, MTU)
		unix.Close(pair[0])
		unix.Close(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		s.Stop()
		done := make(chan error, 1)
		go func() { done <- s.Run() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Stop-before-Run hung")
		}
	}
}

func TestStopDuringDNSResponse(t *testing.T) {
	port, requests := socksServer(t, func(c net.Conn, address string) { io.Copy(io.Discard, c) })
	s, peer := packetSession(t, port)
	peer.Write(queryIPPacket(dnsQuery(t), 53))
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("DNS did not reach SOCKS")
	}
	s.Stop() // Cleanup requires Run to finish within three seconds.
}

func TestStopDuringIncompleteTCPHandshake(t *testing.T) {
	s, peer := packetSession(t, 1080)
	// A SYN without the final ACK leaves ForwarderRequest.CreateEndpoint waiting.
	p := make([]byte, 40)
	p[0], p[8], p[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(p[2:4], 40)
	copy(p[12:16], []byte{10, 77, 0, 1})
	copy(p[16:20], []byte{203, 0, 113, 7})
	binary.BigEndian.PutUint16(p[10:12], checksum(p[:20]))
	binary.BigEndian.PutUint16(p[20:22], 54321)
	binary.BigEndian.PutUint16(p[22:24], 443)
	binary.BigEndian.PutUint32(p[24:28], 1)
	p[32], p[33], p[34], p[35] = 0x50, 2, 0xff, 0xff
	pseudo := make([]byte, 32)
	copy(pseudo[:8], p[12:20])
	pseudo[9], pseudo[11] = 6, 20
	copy(pseudo[12:], p[20:])
	binary.BigEndian.PutUint16(p[36:38], checksum(pseudo))
	peer.Write(p)
	peer.SetReadDeadline(time.Now().Add(time.Second))
	response := make([]byte, MTU)
	n, err := peer.Read(response)
	if err != nil || n < 40 || response[33]&0x12 != 0x12 {
		t.Fatal("no SYN-ACK", err)
	}
	s.Stop()
}

func TestDNSLargeEDNSReplyRespectsSelectedMTU(t *testing.T) {
	for _, mtu := range []int{576, 1500} {
		t.Run(strconv.Itoa(mtu), func(t *testing.T) {
			port, _ := socksServer(t, dnsResponder(t, true))
			_, peer := packetSessionAtMTU(t, port, mtu)
			q := dnsQuery(t)
			name, err := dnsmessage.NewName(".")
			if err != nil {
				t.Fatal(err)
			}
			q.message.Additionals = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeOPT, Class: dnsmessage.Class(4096)},
				Body:   &dnsmessage.OPTResource{},
			}}
			q.query, err = q.message.Pack()
			if err != nil {
				t.Fatal(err)
			}
			peer.Write(queryIPPacket(q, 53))
			response := readDNSReply(t, peer, q)
			if response.Truncated != (mtu == 576) {
				t.Fatalf("MTU=%d: unexpected TC=%t", mtu, response.Truncated)
			}
		})
	}
}
