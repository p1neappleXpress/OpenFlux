package l3

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"openflux/transport"
)

type testReservation struct{ closed bool }

func (r *testReservation) Close() error { r.closed = true; return nil }

func testNAT(t *testing.T) (*udpNAT, *[]*testReservation) {
	t.Helper()
	n := newUDPNAT([4]byte{192, 0, 2, 10})
	var sockets []*testReservation
	n.reserve = func([4]byte) (uint16, io.Closer, error) {
		r := &testReservation{}
		sockets = append(sockets, r)
		return 40000 + uint16(len(sockets)), r, nil
	}
	t.Cleanup(n.Close)
	return n, &sockets
}

func sendNAT(t *testing.T, n *udpNAT, port uint16, dst [4]byte) flowKey {
	t.Helper()
	pkt := udpPacket(clientIPBytes, dst, port, 443, []byte("payload"))
	k, _ := extractFlowKey(pkt)
	if err := n.send(pkt, k, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	wire, ok := extractFlowKey(pkt)
	if !ok || !validUDPChecksums(pkt) {
		t.Fatal("invalid translated packet")
	}
	return wire
}

func TestUDPNATRoundTripAndEndpointIsolation(t *testing.T) {
	n, sockets := testNAT(t)
	dst := [4]byte{203, 0, 113, 7}
	wire := sendNAT(t, n, 5353, dst)
	if wire.srcPort == 5353 || wire.srcIP != ipU32(n.egress) {
		t.Fatal("source was not translated")
	}
	if again := sendNAT(t, n, 5353, dst); again != wire || len(*sockets) != 1 {
		t.Fatal("mapping was not reused")
	}
	other := sendNAT(t, n, 5353, [4]byte{203, 0, 113, 8})
	if other.srcPort == wire.srcPort {
		t.Fatal("different destinations share a reserved port")
	}
	reply := udpPacket(dst, n.egress, 443, wire.srcPort, []byte("reply"))
	rk, _ := extractFlowKey(reply)
	wrong := rk
	wrong.srcPort++
	if n.translateReply(reply, wrong) {
		t.Fatal("accepted reply from a different remote port")
	}
	wrong = rk
	wrong.srcIP++
	if n.translateReply(reply, wrong) {
		t.Fatal("accepted reply from a different remote address")
	}
	if !n.translateReply(reply, rk) {
		t.Fatal("reply not found")
	}
	restored, _ := extractFlowKey(reply)
	if restored.dstIP != ipU32(clientIPBytes) || restored.dstPort != 5353 || !validUDPChecksums(reply) {
		t.Fatal("bad reverse translation/checksum")
	}
	n.Close()
	for _, s := range *sockets {
		if !s.closed {
			t.Fatal("reservation leaked on Close")
		}
	}
	if n.translateReply(reply, rk) {
		t.Fatal("accepted packet after Close")
	}
}

func TestUDPNATExpiryCapacityAndSendFailure(t *testing.T) {
	n, sockets := testNAT(t)
	dst := [4]byte{203, 0, 113, 7}
	for i := 0; i < maxUDPMappings; i++ {
		sendNAT(t, n, uint16(1000+i), dst)
	}
	pkt := udpPacket(clientIPBytes, dst, 65500, 443, nil)
	k, _ := extractFlowKey(pkt)
	if n.send(pkt, k, func([]byte) error { return nil }) == nil {
		t.Fatal("mapping limit not enforced")
	}
	n.mu.Lock()
	for _, m := range n.forward {
		m.lastSeen = time.Now().Add(-ctTimeoutUDP - time.Second)
	}
	n.mu.Unlock()
	if err := n.send(pkt, k, func([]byte) error { return errors.New("send failed") }); err == nil {
		t.Fatal("send error lost")
	}
	if len(n.forward) != 0 || len(n.reverse) != 0 {
		t.Fatal("expired/failed mappings leaked")
	}
	for _, s := range *sockets {
		if !s.closed {
			t.Fatal("reservation not closed")
		}
	}
	n.reserve = func([4]byte) (uint16, io.Closer, error) { return 0, nil, errors.New("bind failed") }
	if n.send(pkt, k, func([]byte) error { t.Fatal("send despite bind failure"); return nil }) == nil {
		t.Fatal("bind error lost")
	}
}

func TestUDPNATRejectsExpiredDNSReply(t *testing.T) {
	n, sockets := testNAT(t)
	dst := [4]byte{203, 0, 113, 7}
	pkt := udpPacket(clientIPBytes, dst, 50000, 53, nil)
	k, _ := extractFlowKey(pkt)
	if err := n.send(pkt, k, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	wire, _ := extractFlowKey(pkt)
	n.mu.Lock()
	n.forward[k].lastSeen = time.Now().Add(-ctTimeoutDNS - time.Second)
	n.mu.Unlock()
	reply := udpPacket(dst, n.egress, 53, wire.srcPort, nil)
	rk, _ := extractFlowKey(reply)
	if n.translateReply(reply, rk) || !(*sockets)[0].closed {
		t.Fatal("expired DNS mapping remained live")
	}
}

func TestUDPReservationExcludesHostPortAndReleases(t *testing.T) {
	port, reservation, err := reserveUDPPort([4]byte{127, 0, 0, 1})
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Close()
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}
	if c, err := net.ListenUDP("udp4", addr); err == nil {
		c.Close()
		t.Fatal("reserved port can be rebound")
	}
	_ = reservation.Close()
	c, err := net.ListenUDP("udp4", addr)
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	c.Close()
}

func TestUDPNATConcurrentClose(t *testing.T) {
	n, _ := testNAT(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(port uint16) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				pkt := udpPacket(clientIPBytes, [4]byte{203, 0, 113, 7}, port, 443, nil)
				k, _ := extractFlowKey(pkt)
				_ = n.send(pkt, k, func([]byte) error { return nil })
			}
		}(uint16(1000 + i))
	}
	n.Close()
	wg.Wait()
	if len(n.forward) != 0 || len(n.reverse) != 0 {
		t.Fatal("close left live mappings")
	}
}

type recordingTransport struct {
	transport.Transport
	packets chan []byte
}

func (r *recordingTransport) Send(p []byte) error { r.packets <- append([]byte(nil), p...); return nil }

type recordingBackend struct{ sent []byte }

func (r *recordingBackend) EgressIP() [4]byte   { return [4]byte{192, 0, 2, 10} }
func (r *recordingBackend) Send(p []byte) error { r.sent = append([]byte(nil), p...); return nil }
func (r *recordingBackend) Recv(func([]byte))   {}
func (r *recordingBackend) Close() error        { return nil }

func TestL3UDPTranslationPath(t *testing.T) {
	n, _ := testNAT(t)
	b := &recordingBackend{}
	r := &recordingTransport{packets: make(chan []byte, 1)}
	exit := &L3Exit{backend: b, trans: r, ct: newConntrack(), udp: n}
	defer exit.Stop()
	dst := [4]byte{203, 0, 113, 7}
	bad := udpPacket(clientIPBytes, dst, 12345, 443, []byte("corrupt"))
	bad[28] ^= 1
	exit.handleFromTransport(bad)
	exit.handleFromTransport(udpPacket([4]byte{10, 10, 10, 99}, dst, 12345, 443, nil))
	if b.sent != nil {
		t.Fatal("forwarded corrupt/spoofed packet")
	}
	exit.handleFromTransport(udpPacket(clientIPBytes, dst, 12345, 443, []byte("ok")))
	wire, ok := extractFlowKey(b.sent)
	if !ok || wire.srcPort == 12345 {
		t.Fatal("L3 bypassed UDP NAT")
	}
	exit.handleFromInternet(udpPacket(dst, b.EgressIP(), 443, wire.srcPort, []byte("response")))
	select {
	case p := <-r.packets:
		if binary.BigEndian.Uint16(p[22:24]) != 12345 || !validUDPChecksums(p) {
			t.Fatal("bad reply restoration")
		}
	default:
		t.Fatal("reply not delivered")
	}
}
