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

	"openflux/network"
	"openflux/transport"
	"openflux/tunnel/l3"
	"openflux/utils"
)

// ExitMode выбирает, как выходная нода общается с интернетом.
type ExitMode int

const (
	ExitModeL3 ExitMode = iota
	ExitModeL4
)

func (m ExitMode) String() string {
	switch m {
	case ExitModeL3:
		return "l3"
	default:
		return "l4"
	}
}

func ParseExitMode(s string) (ExitMode, error) {
	switch s {
	case "", "l4", "proxy":
		return ExitModeL4, nil
	case "l3":
		return ExitModeL3, nil
	default:
		return ExitModeL4, fmt.Errorf("unknown mode %q (want l3|l4)", s)
	}
}

type TCPTunnel struct {
	gvisorStack *stack.Stack
	tunnelEP    *TunnelLinkEndpoint
	transport   transport.Transport
	isExitNode  bool
	exitMode    ExitMode
	startTime   time.Time
	packetCount atomic.Uint64
	stopOnce    sync.Once
	stopCh      chan struct{}
	udpFlows    atomic.Int32
}

var (
	TCPBufMin     = 4 * 1024 * 1024
	TCPBufDefault = 16 * 1024 * 1024
	TCPBufMax     = 64 * 1024 * 1024
)

func SetTCPBuffers(s *stack.Stack) {
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: TCPBufMin, Default: TCPBufDefault, Max: TCPBufMax}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv); err != nil {
		utils.Debugf("[TUNNEL] set recv buffer: %v", err)
	}
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: TCPBufMin, Default: TCPBufDefault, Max: TCPBufMax}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd); err != nil {
		utils.Debugf("[TUNNEL] set send buffer: %v", err)
	}
}

func NewTCPTunnel(trans transport.Transport, isExitNode bool) *TCPTunnel {
	return NewTCPTunnelMode(trans, isExitNode, ExitModeL4)
}

func NewTCPTunnelMode(trans transport.Transport, isExitNode bool, mode ExitMode) *TCPTunnel {
	t := &TCPTunnel{
		transport:  trans,
		isExitNode: isExitNode,
		exitMode:   mode,
		startTime:  time.Now(),
		stopCh:     make(chan struct{}),
	}

	utils.Debugf("[TUNNEL] Net stack init...")
	t.gvisorStack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	SetTCPBuffers(t.gvisorStack)

	tunnelEP := NewTunnelLinkEndpoint()
	if n, ok := trans.(transport.PeerParameterProvider); ok {
		if p, ready := n.PeerParameters(); ready {
			tunnelEP.SetMTU(uint32(min(1500, p.MaxPacketSize)))
		}
	}
	tunnelEP.onOutgoingPacket = func(data []byte) {
		// `->` : emitted by the local stack, going to the peer / internet.
		if utils.Level() >= 1 {
			utils.Debugf("[TUNNEL] %s", network.FormatPacket(network.DirOutbound, data))
		}
		if err := trans.Send(data); err != nil {
			utils.Debugf("[TUNNEL] trans.Send error: %v", err)
		}
	}
	t.tunnelEP = tunnelEP

	tunnelNIC := tcpip.NICID(1)
	if err := t.gvisorStack.CreateNIC(tunnelNIC, tunnelEP); err != nil {
		utils.Debugf("[TUNNEL] CreateNIC tunnel error: %v", err)
	}

	if isExitNode {
		t.setupExitNodeProxy(tunnelNIC)
	} else {
		t.setupClient(tunnelNIC)
	}

	trans.Receive(func(data []byte) {
		// `<-` : received from the peer / internet, going into the local stack.
		if utils.Level() >= 1 {
			utils.Debugf("[TUNNEL] %s", network.FormatPacket(network.DirInbound, data))
		}
		tunnelEP.InjectInbound(data)
	})

	utils.SafeGo("tunnel.printStats", t.printStats)
	return t
}

func (t *TCPTunnel) setupExitNodeProxy(tunnelNIC tcpip.NICID) {
	utils.Debugf("[TUNNEL] EXIT NODE - proxy mode (no raw sockets)")

	t.gvisorStack.SetPromiscuousMode(tunnelNIC, true)
	t.gvisorStack.SetSpoofing(tunnelNIC, true)
	t.gvisorStack.AddRoute(tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		NIC:         tunnelNIC,
	})

	fwd := tcp.NewForwarder(t.gvisorStack, 0, 8192, t.handleExitTCP)
	t.gvisorStack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	udpFwd := udp.NewForwarder(t.gvisorStack, t.handleExitUDP)
	t.gvisorStack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
}

const udpIdleTimeout = 2 * time.Minute

func (t *TCPTunnel) handleExitUDP(r *udp.ForwarderRequest) bool {
	if t.udpFlows.Add(1) > 256 {
		t.udpFlows.Add(-1)
		return true
	}
	id := r.ID()
	dest := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort))

	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		t.udpFlows.Add(-1)
		utils.Debugf("[EXIT] UDP CreateEndpoint %s: %v", dest, tErr)
		return true
	}
	local := gonet.NewUDPConn(&wq, ep)
	remote, err := net.DialTimeout("udp", dest, 10*time.Second)
	if err != nil {
		utils.Debugf("[EXIT] UDP dial %s failed: %v", dest, err)
		local.Close()
		t.udpFlows.Add(-1)
		return true
	}

	utils.SafeGo("exit.udp-flow", func() {
		defer t.udpFlows.Add(-1)
		refresh := func() {
			deadline := time.Now().Add(udpIdleTimeout)
			_ = local.SetReadDeadline(deadline)
			_ = remote.SetReadDeadline(deadline)
		}
		refresh()
		var once sync.Once
		closeBoth := func() {
			_ = local.Close()
			_ = remote.Close()
		}
		pump := func(dst, src net.Conn) {
			defer once.Do(closeBoth)
			buf := make([]byte, 65535)
			for {
				n, err := src.Read(buf)
				if err != nil {
					return
				}
				refresh()
				_ = dst.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
		}
		done := make(chan struct{})
		go func() { defer close(done); pump(remote, local) }()
		pump(local, remote)
		<-done
	})
	return true
}

func (t *TCPTunnel) handleExitTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dest := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort))

	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		utils.Debugf("[EXIT] CreateEndpoint %s: %v", dest, tErr)
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(&wq, ep)

	utils.SafeGo("exit.flow", func() {
		remote, err := net.DialTimeout("tcp", dest, 10*time.Second)
		if err != nil {
			utils.Debugf("[EXIT] dial %s failed: %v", dest, err)
			local.Close()
			return
		}
		if tc, ok := remote.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(16 * 1024 * 1024)
			_ = tc.SetWriteBuffer(16 * 1024 * 1024)
		}
		utils.Debugf("[EXIT] %s connected", dest)

		go func() {
			buf := make([]byte, 256*1024)
			io.CopyBuffer(remote, local, buf)
			remote.Close()
			local.Close()
		}()
		buf := make([]byte, 256*1024)
		io.CopyBuffer(local, remote, buf)
		local.Close()
		remote.Close()
	})
}

func (t *TCPTunnel) setupClient(tunnelNIC tcpip.NICID) {
	clientAddr := tcpip.AddrFrom4([4]byte{10, 10, 10, 2})
	t.gvisorStack.AddProtocolAddress(tunnelNIC, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   clientAddr,
			PrefixLen: 24,
		},
	}, stack.AddressProperties{})

	t.gvisorStack.AddRoute(tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		NIC:         tunnelNIC,
	})
}

var dialTimeout = 10 * time.Second

func (t *TCPTunnel) DialTCP(address string) (net.Conn, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}

	ip := tcpAddr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("IPv6 not supported")
	}
	utils.Debugf("[TUNNEL] DialTCP %s -> %s:%d", address, ip.String(), tcpAddr.Port)

	nic := tcpip.NICID(1)
	if t.isExitNode && false {
		nic = tcpip.NICID(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, t.gvisorStack, tcpip.FullAddress{
		NIC:  nic,
		Addr: tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		Port: uint16(tcpAddr.Port),
	}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (t *TCPTunnel) DialUDP(address string) (net.Conn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP: %w", err)
	}
	ip := udpAddr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("IPv6 not supported")
	}
	remote := &tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		Port: uint16(udpAddr.Port),
	}
	return gonet.DialUDP(t.gvisorStack, nil, remote, ipv4.ProtocolNumber)
}

func (t *TCPTunnel) ListenTCP(port uint16) (net.Listener, error) {
	return gonet.ListenTCP(t.gvisorStack, tcpip.FullAddress{
		NIC:  1,
		Port: port,
	}, ipv4.ProtocolNumber)
}

func (t *TCPTunnel) Close() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
		t.gvisorStack.Close()
	})
}

func (t *TCPTunnel) printStats() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			stats := t.gvisorStack.Stats()
			utils.Debugf("[STATS] uptime=%v mode=%s packets=%d connected=%d established=%d retrans=%d",
				time.Since(t.startTime).Round(time.Second),
				t.exitMode.String(),
				t.packetCount.Load(),
				stats.TCP.CurrentConnected.Value(),
				stats.TCP.CurrentEstablished.Value(),
				stats.TCP.Retransmits.Value(),
			)
		}
	}
}

func SetLocalIP(ip string) error { return l3.SetLocalIP(ip) }
