package socks5

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type directDialer struct{}

func (directDialer) DialTCP(address string) (net.Conn, error) { return net.Dial("tcp", address) }
func (directDialer) DialUDP(address string) (net.Conn, error) { return net.Dial("udp", address) }

func TestParseUDPRequestIPv4(t *testing.T) {
	packet := []byte{0, 0, 0, 1, 1, 1, 1, 1, 0, 53, 0xaa, 0xbb}
	addr, payload, err := parseUDPRequest(packet)
	if err != nil {
		t.Fatalf("parseUDPRequest: %v", err)
	}
	if addr != "1.1.1.1:53" || !bytes.Equal(payload, []byte{0xaa, 0xbb}) {
		t.Fatalf("got addr=%q payload=%x", addr, payload)
	}
}

func TestParseUDPRequestRejectsFragments(t *testing.T) {
	packet := []byte{0, 0, 1, 1, 1, 1, 1, 1, 0, 53}
	if _, _, err := parseUDPRequest(packet); err == nil {
		t.Fatal("expected fragmented UDP request to be rejected")
	}
}

func TestMakeUDPResponse(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 443}
	got := makeUDPResponse(addr, []byte("x"))
	want := []byte{0, 0, 0, 1, 8, 8, 8, 8, 1, 187, 'x'}
	if !bytes.Equal(got, want) {
		t.Fatalf("response=%x want=%x", got, want)
	}
}

func TestIPv6AddressEncoding(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}
	packet := makeUDPResponse(addr, []byte("ipv6"))
	got, payload, err := parseUDPRequest(packet)
	if err != nil || got != "[2001:db8::1]:443" || string(payload) != "ipv6" {
		t.Fatalf("IPv6 roundtrip: %s %q %v", got, payload, err)
	}
	var reply bytes.Buffer
	if err := writeReply(&reply, 0, addr); err != nil {
		t.Fatal(err)
	}
	if reply.Bytes()[3] != 4 || reply.Len() != 22 {
		t.Fatalf("bad IPv6 reply: %x", reply.Bytes())
	}
}

type tcpOnlyDialer struct{}

func (tcpOnlyDialer) DialTCP(address string) (net.Conn, error) { return nil, net.ErrClosed }

func TestTCPOnlyDialerRejectsUDP(t *testing.T) {
	s := NewSOCKS5Server("", tcpOnlyDialer{})
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { defer close(done); s.handleConnection(server) }()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = client.Write([]byte{5, 1, 0})
	var method [2]byte
	if _, err := io.ReadFull(client, method[:]); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	var reply [10]byte
	if _, err := io.ReadFull(client, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 7 {
		t.Fatalf("reply=%x", reply)
	}
	<-done
}

func TestUDPAssociateRoundTrip(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	server := NewSOCKS5Server("127.0.0.1:0", directDialer{})
	if err := server.Bind(); err != nil {
		t.Fatalf("bind SOCKS: %v", err)
	}
	serverAddr := server.listener.Addr().String()
	go func() { _ = server.Start() }()
	defer server.Close()

	control, err := net.Dial("tcp", serverAddr)
	if err != nil {
		t.Fatalf("dial SOCKS: %v", err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(control, method); err != nil || !bytes.Equal(method, []byte{5, 0}) {
		t.Fatalf("method reply=%x err=%v", method, err)
	}
	if _, err := control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("UDP associate: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(control, reply); err != nil || reply[1] != 0 {
		t.Fatalf("associate reply=%x err=%v", reply, err)
	}
	relay := &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(reply[8])<<8 | int(reply[9])}
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen client: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	echoAddr := echo.LocalAddr().(*net.UDPAddr)
	request := []byte{0, 0, 0, 1, 127, 0, 0, 1, byte(echoAddr.Port >> 8), byte(echoAddr.Port)}
	request = append(request, []byte("socks-udp")...)
	if _, err := client.WriteToUDP(request, relay); err != nil {
		t.Fatalf("write relay: %v", err)
	}
	response := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(response)
	if err != nil {
		t.Fatalf("read relay: %v", err)
	}
	_, payload, err := parseUDPRequest(response[:n])
	if err != nil || string(payload) != "socks-udp" {
		t.Fatalf("response payload=%q err=%v", payload, err)
	}
}
