//go:build linux

package l3

import (
	"bytes"
	"net"
	"os"
	"testing"
	"time"
)

// Run only in a disposable Linux network namespace with loopback up. No
// firewall changes, Internet access, or external network interfaces are needed.
func TestLinuxRawUDPNAT(t *testing.T) {
	if os.Getenv("OPENFLUX_L3_INTEGRATION") != "1" {
		t.Skip("requires opt-in Linux network namespace and CAP_NET_RAW")
	}
	loopback := net.IPv4(127, 0, 0, 1)
	host, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	hostPort := uint16(host.LocalAddr().(*net.UDPAddr).Port)
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	echoPort := uint16(echo.LocalAddr().(*net.UDPAddr).Port)
	icmp, err := net.ListenIP("ip4:icmp", &net.IPAddr{IP: loopback})
	if err != nil {
		t.Fatalf("ICMP capture (CAP_NET_RAW required): %v", err)
	}
	defer icmp.Close()
	if err := SetLocalIP("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer SetLocalIP("")
	r := &recordingTransport{packets: make(chan []byte, 64)}
	exit, err := New(r)
	if err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()
	exit.backend.Recv(exit.handleFromInternet)
	ports := make(chan uint16, 64)
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			ports <- uint16(from.Port)
			if _, err := echo.WriteToUDP(buf[:n], from); err != nil {
				return
			}
		}
	}()
	defer func() { echo.Close(); <-echoDone }()
	// Enough replies to fill the reservation's intentionally unread kernel
	// queue: raw receive must still work without kernel port-unreachable.
	for i := 0; i < 32; i++ {
		payload := bytes.Repeat([]byte{byte(i)}, 1200)
		if i == 0 {
			payload = nil
		}
		exit.handleFromTransport(udpPacket(clientIPBytes, [4]byte{127, 0, 0, 1}, hostPort, echoPort, payload))
		select {
		case p := <-r.packets:
			k, ok := extractFlowKey(p)
			if !ok || k.dstPort != hostPort || k.dstIP != ipU32(clientIPBytes) || !validUDPChecksums(p) || !bytes.Equal(p[28:], payload) {
				t.Fatalf("invalid raw UDP reply at iteration %d", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("raw UDP reply timed out at iteration %d", i)
		}
		if port := <-ports; port == hostPort {
			t.Fatal("NAT reused an occupied host port")
		}
	}
	_ = host.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := host.ReadFromUDP(make([]byte, 2048)); err == nil {
		t.Fatal("tunnel reply leaked into host socket")
	}
	_ = icmp.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 2048)
	for {
		n, _, err := icmp.ReadFromIP(buf)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				break
			}
			t.Fatal(err)
		}
		if n >= 2 && buf[0] == 3 && buf[1] == 3 {
			t.Fatal("kernel emitted ICMP port-unreachable during raw UDP flow")
		}
	}
	done := make(chan error, 1)
	go func() { done <- exit.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("raw backend shutdown blocked")
	}
}
