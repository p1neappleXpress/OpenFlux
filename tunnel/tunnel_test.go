package tunnel

import (
	"context"
	"io"
	"sync"
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

	"openflux/transport"
)

// stallingPipe delivers packets in order after a fixed delay. With stallEvery
// set it also freezes delivery for stallFor at the start of every stallEvery
// period, the way the Yandex relay now and then holds a participant's messages
// for a few hundred milliseconds without losing or reordering them.
type stallingPipe struct {
	delay      time.Duration
	stallEvery time.Duration
	stallFor   time.Duration
	deliver    func([]byte)

	start time.Time
	queue chan timedPacket
}

type timedPacket struct {
	data []byte
	at   time.Time
}

func newStallingPipe(ctx context.Context, delay, stallEvery, stallFor time.Duration, deliver func([]byte)) *stallingPipe {
	p := &stallingPipe{
		delay:      delay,
		stallEvery: stallEvery,
		stallFor:   stallFor,
		deliver:    deliver,
		start:      time.Now(),
		queue:      make(chan timedPacket, 1<<16),
	}
	go p.run(ctx)
	return p
}

func (p *stallingPipe) send(data []byte) {
	p.queue <- timedPacket{append([]byte(nil), data...), time.Now().Add(p.delay)}
}

func (p *stallingPipe) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt := <-p.queue:
			time.Sleep(time.Until(pkt.at))
			if p.stallEvery > 0 {
				if phase := time.Since(p.start) % p.stallEvery; phase < p.stallFor {
					time.Sleep(p.stallFor - phase)
				}
			}
			p.deliver(pkt.data)
		}
	}
}

// pipeTransport stands in for the document transport on the client side.
type pipeTransport struct {
	up *stallingPipe

	mu   sync.Mutex
	recv func([]byte)
}

func (p *pipeTransport) Start() error                    { return nil }
func (p *pipeTransport) Stop() error                     { return nil }
func (p *pipeTransport) IsConnected() bool               { return true }
func (p *pipeTransport) Stats() transport.TransportStats { return transport.TransportStats{} }

func (p *pipeTransport) Send(data []byte) error {
	p.up.send(data)
	return nil
}

func (p *pipeTransport) Receive(cb func([]byte)) {
	p.mu.Lock()
	p.recv = cb
	p.mu.Unlock()
}

func (p *pipeTransport) deliver(data []byte) {
	p.mu.Lock()
	cb := p.recv
	p.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

// Upload through the client's gVisor stack over a channel that never loses a
// packet but whose ACK direction stalls for 120 ms every 300 ms: longer than
// the tail loss probe timeout (2*SRTT, ~20 ms here), shorter than the 200 ms
// minimum RTO. Nothing is lost, so the sender must not enter loss recovery;
// each recovery halves its window, which is what capped uploads through the
// Yandex relay at ~100 KB/s.
func TestClientUploadDoesNotEnterRecoveryOnACKStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerAddr := tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
	peer := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	peerLink := channel.New(4096, 1500, "")
	if err := peer.CreateNIC(1, peerLink); err != nil {
		t.Fatalf("peer CreateNIC: %v", err)
	}
	if err := peer.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: peerAddr.WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("peer AddProtocolAddress: %v", err)
	}
	peer.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})

	trans := &pipeTransport{}
	trans.up = newStallingPipe(ctx, 5*time.Millisecond, 0, 0, func(data []byte) {
		peerLink.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		}))
	})
	down := newStallingPipe(ctx, 5*time.Millisecond, 300*time.Millisecond, 120*time.Millisecond, trans.deliver)
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

	ln, err := gonet.ListenTCP(peer, tcpip.FullAddress{NIC: 1, Addr: peerAddr, Port: 9000}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("peer listen: %v", err)
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
		t.Fatalf("DialTCP: %v", err)
	}

	// Paced like a real upload: 64 KB every 50 ms for 2 s.
	const chunk, writes = 64 * 1024, 40
	buf := make([]byte, chunk)
	for i := 0; i < writes; i++ {
		if _, err := conn.Write(buf); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := conn.(*gonet.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	select {
	case n := <-received:
		if n != chunk*writes {
			t.Fatalf("peer received %d bytes, want %d", n, chunk*writes)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("upload did not finish in 30 s")
	}
	conn.Close()

	st := tun.gvisorStack.Stats().TCP
	t.Logf("sender: SACKRecovery=%d FastRecovery=%d TLPRecovery=%d Timeouts=%d SpuriousRecovery=%d Retransmits=%d",
		st.SACKRecovery.Value(), st.FastRecovery.Value(), st.TLPRecovery.Value(),
		st.Timeouts.Value(), st.SpuriousRecovery.Value(), st.Retransmits.Value())
	if n := st.SACKRecovery.Value() + st.FastRecovery.Value(); n != 0 {
		t.Errorf("sender entered loss recovery %d times on a lossless channel; each entry halves the congestion window", n)
	}
}
