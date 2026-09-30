package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/p1neappleXpress/OpenFlux/network"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/phpbox"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// StreamDialer opens one TCP stream to host:port at the exit (the phpbox mux's Dial).
type StreamDialer func(ctx context.Context, host string, port int) (net.Conn, error)

// StreamNet is the stream mode's full tunnel: the device's IP packets (a TUN
// interface, an Android VpnService, an iOS packet tunnel) go into a local
// gVisor stack that terminates every TCP connection and opens a mux stream to
// the same destination at the exit. It is a transport.Transport, so anything
// that carries packets over a transport - the utun/Wintun clients, the mobile
// packet API - runs on it unchanged:
//
//   - Send(pkt) is a packet from the device, Receive(cb) gets the packets for it;
//   - DNS is answered here with fake addresses (fakedns.go) and the connection
//     to such an address is opened by name at the exit;
//   - TCP only, ports 80 and 443 (what the exit allows): everything else
//     (QUIC, other UDP, IPv6) is dropped, and browsers fall back to TCP.
type StreamNet struct {
	carrier phpbox.Carrier // nil when a dialer was given
	mux     *phpbox.Mux
	dial    StreamDialer
	dns     *fakeDNS

	stack *stack.Stack
	ep    *TunnelLinkEndpoint
	mtu   uint32

	recv      atomic.Pointer[func([]byte)]
	started   atomic.Bool
	startedAt time.Time
	stopOnce  sync.Once
	packetsTx atomic.Uint64
	packetsRx atomic.Uint64
	bytesTx   atomic.Uint64
	bytesRx   atomic.Uint64
}

// NewStreamNet builds the full tunnel over a carrier: Start brings the carrier
// and the mux up, Stop ends them.
func NewStreamNet(carrier phpbox.Carrier) *StreamNet {
	n := newStreamNet()
	n.carrier = carrier
	return n
}

// NewStreamNetDialer is StreamNet over any dialer (tests, other exits).
func NewStreamNetDialer(dial StreamDialer) *StreamNet {
	n := newStreamNet()
	n.dial = dial
	return n
}

func newStreamNet() *StreamNet {
	n := &StreamNet{dns: newFakeDNS(), mtu: 1280}
	n.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	// Each stream is bounded by the mux's window anyway; keep the stack's buffers modest (phones).
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4096, Default: 256 << 10, Max: 4 << 20}
	_ = n.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4096, Default: 256 << 10, Max: 4 << 20}
	_ = n.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)

	n.ep = NewTunnelLinkEndpoint()
	n.ep.SetMTU(n.mtu)
	n.ep.onOutgoingPacket = func(data []byte) { // the stack's packet for the device
		network.LogPacket("STREAMNET", network.DirInbound, data)
		n.packetsRx.Add(1)
		n.bytesRx.Add(uint64(len(data)))
		if cb := n.recv.Load(); cb != nil {
			(*cb)(data)
		}
	}
	const nic = tcpip.NICID(1)
	if err := n.stack.CreateNIC(nic, n.ep); err != nil {
		utils.Debugf("[STREAMNET] CreateNIC: %v", err)
	}
	n.stack.SetPromiscuousMode(nic, true) // accept a packet for any address...
	n.stack.SetSpoofing(nic, true)        // ...and answer as that address
	n.stack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nic})
	fwd := tcp.NewForwarder(n.stack, 0, 8192, n.handleTCP)
	n.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	udpFwd := udp.NewForwarder(n.stack, n.handleUDP)
	n.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
	return n
}

// --- transport.Transport ---------------------------------------------------

func (n *StreamNet) Start() error {
	if !n.started.CompareAndSwap(false, true) {
		return nil
	}
	n.startedAt = time.Now()
	if n.carrier != nil {
		n.mux = phpbox.NewMux(n.carrier)
		if err := n.mux.Start(); err != nil {
			n.started.Store(false)
			return fmt.Errorf("start carrier: %w", err)
		}
		n.dial = n.mux.Dial
	}
	return nil
}

func (n *StreamNet) Stop() error {
	n.stopOnce.Do(func() {
		n.stack.Close()
		if n.mux != nil {
			_ = n.mux.Close()
		}
	})
	return nil
}

// Send takes a packet from the device.
func (n *StreamNet) Send(data []byte) error {
	network.LogPacket("STREAMNET", network.DirOutbound, data)
	n.packetsTx.Add(1)
	n.bytesTx.Add(uint64(len(data)))
	n.ep.InjectInbound(data)
	return nil
}

// Receive registers where the packets for the device go.
func (n *StreamNet) Receive(cb func([]byte)) { n.recv.Store(&cb) }

// IsConnected reports whether the carrier has joined (a carrier that cannot say counts as joined).
func (n *StreamNet) IsConnected() bool {
	if !n.started.Load() {
		return false
	}
	if c, ok := n.carrier.(interface{ IsConnected() bool }); ok {
		return c.IsConnected()
	}
	return true
}

func (n *StreamNet) Stats() transport.TransportStats {
	up := time.Duration(0)
	if n.started.Load() {
		up = time.Since(n.startedAt)
	}
	return transport.TransportStats{
		BytesSent: n.bytesTx.Load(), BytesReceived: n.bytesRx.Load(),
		PacketsSent: n.packetsTx.Load(), PacketsRecv: n.packetsRx.Load(),
		Connected: n.IsConnected(), Uptime: up,
	}
}

// SetMTU sets the MTU of the stack's interface (the device's, at least 1280).
func (n *StreamNet) SetMTU(m uint32) { n.mtu = m; n.ep.SetMTU(m) }

// --- TCP -------------------------------------------------------------------

const streamDialTimeout = 20 * time.Second

func (n *StreamNet) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	ip := id.LocalAddress.As4()
	host := net.IP(ip[:]).String()
	if name, ok := n.dns.nameFor(ip); ok {
		host = name
	}
	port := int(id.LocalPort)
	// Dial first, then answer the SYN: an app whose destination cannot be reached
	// hears a refusal at once instead of a connection that opens and dies.
	utils.SafeGo("streamnet.flow", func() {
		if n.dial == nil {
			r.Complete(true)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), streamDialTimeout)
		remote, err := n.dial(ctx, host, port)
		cancel()
		if err != nil {
			utils.Debugf("[STREAMNET] %s:%d: %v", host, port, err)
			r.Complete(true)
			return
		}
		var wq waiter.Queue
		ep, tErr := r.CreateEndpoint(&wq)
		if tErr != nil {
			utils.Debugf("[STREAMNET] CreateEndpoint %s:%d: %v", host, port, tErr)
			r.Complete(true)
			_ = remote.Close()
			return
		}
		r.Complete(false)
		local := gonet.NewTCPConn(&wq, ep)
		utils.Debugf("[STREAMNET] %s:%d connected", host, port)
		done := make(chan struct{})
		go func() {
			buf := make([]byte, 64<<10)
			_, _ = io.CopyBuffer(remote, local, buf)
			_ = remote.Close()
			_ = local.Close()
			close(done)
		}()
		buf := make([]byte, 64<<10)
		_, _ = io.CopyBuffer(local, remote, buf)
		_ = local.Close()
		_ = remote.Close()
		<-done
	})
}

// --- UDP: only DNS ---------------------------------------------------------

func (n *StreamNet) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	if id.LocalPort != 53 {
		return true // QUIC and the rest: dropped, the app falls back to TCP
	}
	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		return true
	}
	local := gonet.NewUDPConn(&wq, ep)
	utils.SafeGo("streamnet.dns", func() {
		defer local.Close()
		buf := make([]byte, 1500)
		for {
			_ = local.SetReadDeadline(time.Now().Add(30 * time.Second))
			nr, err := local.Read(buf)
			if err != nil {
				return
			}
			if resp := n.dns.answer(buf[:nr]); resp != nil {
				utils.Debugf("[STREAMNET] dns answered (%d bytes)", len(resp))
				_, _ = local.Write(resp)
			}
		}
	})
	return true
}
