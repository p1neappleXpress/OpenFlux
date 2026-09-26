//go:build linux

package l3

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"testing"
	"time"
)

// The CI namespace sets loopback MTU to 1280. No public network is involved.
func TestLinuxRawICMPAndMTU(t *testing.T) {
	if os.Getenv("OPENFLUX_L3_INTEGRATION") != "1" {
		t.Skip("requires isolated Linux namespace, loopback MTU 1280 and CAP_NET_RAW")
	}
	if err := SetLocalIP("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	defer SetLocalIP("")
	out := &recordingTransport{packets: make(chan []byte, 16)}
	ex, err := New(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Stop()
	ex.backend.Recv(ex.handleFromInternet)
	dst := [4]byte{127, 0, 0, 1}
	if mtu := ex.backend.(*rawBackend).routeMTU(dst); mtu != 1280 {
		t.Fatalf("namespace loopback MTU=%d, want 1280", mtu)
	}
	read := func() []byte {
		t.Helper()
		select {
		case p := <-out.packets:
			return p
		case <-time.After(3 * time.Second):
			t.Fatal("no raw reply")
			return nil
		}
	}
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	port := uint16(server.LocalAddr().(*net.UDPAddr).Port)
	payload := bytes.Repeat([]byte{42}, 2000)
	p := udpPacket(clientIPBytes, dst, 50123, port, payload)
	p[6] = 0x40
	fixIPChecksum(p)
	original := append([]byte(nil), p...)
	ex.handleFromTransport(p)
	got := read()
	if got[9] != 1 || got[20] != 3 || got[21] != 4 || binary.BigEndian.Uint16(got[26:28]) != 1280 || !bytes.Equal(got[28:], original[:28]) {
		t.Fatalf("bad real EMSGSIZE feedback: %x", got)
	}
	// The same datagram without DF must be fragmented by the raw backend and
	// reassembled by the receiving kernel, preserving the translated checksum.
	ex.handleFromTransport(udpPacket(clientIPBytes, dst, 50123, port, payload))
	_ = server.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, peer, err := server.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatal("fragmented payload corrupted")
	}
	if _, err := server.WriteToUDP([]byte("ok"), peer); err != nil {
		t.Fatal(err)
	}
	got = read()
	if !validUDPChecksums(got) || !bytes.Equal(got[28:], []byte("ok")) {
		t.Fatal("fragmented flow response not restored")
	}
	// A real kernel error for a closed UDP port must return through the NAT.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	original = udpPacket(clientIPBytes, dst, 50123, port, []byte("closed"))
	ex.handleFromTransport(append([]byte(nil), original...))
	got = read()
	// IP_HDRINCL may replace a zero IPv4 ID. Match the original flow and UDP
	// checksum, not that kernel-owned ID (and its corresponding IP checksum).
	if len(got) < 56 || got[9] != 1 || got[20] != 3 || got[21] != 3 || !bytes.Equal(got[40:48], original[12:20]) || !bytes.Equal(got[48:56], original[20:28]) || onesComplementSum(got[28:48]) != 0 || onesComplementSum(got[20:]) != 0 {
		t.Fatal("kernel port-unreachable quote was not restored")
	}
}
