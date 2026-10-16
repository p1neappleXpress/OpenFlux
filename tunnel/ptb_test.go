package tunnel

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// bigThenSmall declares a 16000-byte carrier but refuses anything above limit,
// as a session does when the carrier that can take large packets is gone and
// a smaller one has taken over.
type bigThenSmall struct {
	pipeTransport
	limit    int
	refusals atomic.Int64
}

func (b *bigThenSmall) CarrierMTU() int { return 16000 }

func (b *bigThenSmall) Send(data []byte) error {
	if len(data) > b.limit {
		b.refusals.Add(1)
		return &transport.PacketTooLargeError{Limit: b.limit}
	}
	return b.pipeTransport.Send(data)
}

// A connection opened with the large MSS of a big carrier must survive the
// traffic moving to a carrier that cannot take large packets: the stack is told
// with an ICMP "fragmentation needed" and goes on in smaller segments.
func TestConnectionSurvivesCarrierThatCannotTakeLargePackets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerAddr := tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
	peer := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	peerLink := channel.New(4096, 16000, "")
	if err := peer.CreateNIC(1, peerLink); err != nil {
		t.Fatal(err)
	}
	if err := peer.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: peerAddr.WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	peer.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})

	trans := &bigThenSmall{limit: 1500}
	trans.up = newStallingPipe(ctx, 2*time.Millisecond, 0, 0, func(data []byte) {
		peerLink.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		}))
	})
	down := newStallingPipe(ctx, 2*time.Millisecond, 0, 0, trans.deliver)
	go func() {
		for {
			pkt := peerLink.ReadContext(ctx)
			if pkt == nil {
				return
			}
			data := pkt.ToView().ToSlice()
			pkt.DecRef()
			down.send(data)
		}
	}()

	tun := NewTCPTunnelMode(trans, false, ExitModeL4)
	if got := tun.tunnelEP.MTU(); got != 16000 {
		t.Fatalf("tunnel MTU = %d, want the carrier's 16000", got)
	}

	ln, err := gonet.ListenTCP(peer, tcpip.FullAddress{NIC: 1, Addr: peerAddr, Port: 9000}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan int64, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			received <- -1
			return
		}
		n, _ := io.Copy(io.Discard, c)
		c.Close()
		received <- n
	}()

	conn, err := tun.DialTCP("10.0.0.1:9000")
	if err != nil {
		t.Fatal(err)
	}
	const total = 3 << 20
	buf := make([]byte, 64<<10)
	for sent := 0; sent < total; sent += len(buf) {
		if _, err := conn.Write(buf); err != nil {
			t.Fatalf("write at %d: %v", sent, err)
		}
	}
	if err := conn.(*gonet.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-received:
		if n != total {
			t.Fatalf("peer received %d bytes, want %d", n, total)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("transfer did not finish: the connection stalled on oversized packets")
	}
	conn.Close()
	if trans.refusals.Load() == 0 {
		t.Fatal("no packet was refused: the test did not exercise the path")
	}
	t.Logf("%d oversized packets refused, transfer completed", trans.refusals.Load())
}

func TestBuildFragNeededQuotesTheOriginal(t *testing.T) {
	orig := make([]byte, 100)
	h := header.IPv4(orig)
	h.Encode(&header.IPv4Fields{
		TotalLength: 100, TTL: 64, Protocol: uint8(header.TCPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4([4]byte{10, 10, 10, 2}), DstAddr: tcpip.AddrFrom4([4]byte{93, 184, 216, 34}),
	})
	h.SetChecksum(^h.CalculateChecksum())
	out := buildFragNeeded(orig, 1400)
	ip := header.IPv4(out)
	if !ip.IsValid(len(out)) || ip.SourceAddress() != h.DestinationAddress() || ip.DestinationAddress() != h.SourceAddress() {
		t.Fatalf("bad ICMP envelope: %v", ip)
	}
	if !ip.IsChecksumValid() {
		t.Fatal("IP checksum")
	}
	icmp := header.ICMPv4(ip.Payload())
	if icmp.Type() != header.ICMPv4DstUnreachable || icmp.Code() != header.ICMPv4FragmentationNeeded || icmp.MTU() != 1400 {
		t.Fatalf("type/code/mtu = %v/%v/%d", icmp.Type(), icmp.Code(), icmp.MTU())
	}
	if string(icmp.Payload()) != string(orig[:28]) {
		t.Fatal("the original header and 8 bytes are not quoted")
	}
}
