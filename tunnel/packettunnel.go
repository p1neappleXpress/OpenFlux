package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"universal-bypass-tool/utils"
)

// TCPDialer originates a TCP connection to address ("host:port") through some
// upstream (here: the OpenFlux transport tunnel to the exit node).
type TCPDialer interface {
	DialTCP(address string) (net.Conn, error)
}

// PacketTunnel is a userspace TCP/IP stack (tun2socks) for an iOS
// NEPacketTunnelProvider: it accepts raw IP packets from the device, terminates
// TCP locally and forwards each flow through the given dialer. Outbound packets
// (stack -> device) are read back with ReadOutbound.
//
// NOTE: only TCP is handled. The OpenFlux transport is TCP-only, so UDP
// (including plain DNS) is not carried; DNS must be provided over TCP by the
// extension.
type PacketTunnel struct {
	stack  *stack.Stack
	ep     *channel.Endpoint
	dialer TCPDialer
	nicID  tcpip.NICID
}

// NewPacketTunnel builds the stack and installs a TCP forwarder.
func NewPacketTunnel(dialer TCPDialer, mtu uint32) *PacketTunnel {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	ep := channel.New(1024, mtu, "")
	nicID := tcpip.NICID(1)
	if err := s.CreateNIC(nicID, ep); err != nil {
		utils.Debugf("[PKT] CreateNIC: %v", err)
	}
	// Accept packets addressed to any destination and let the stack answer
	// with any source address (we are terminating arbitrary device traffic).
	s.SetPromiscuousMode(nicID, true)
	s.SetSpoofing(nicID, true)
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicID})

	pt := &PacketTunnel{stack: s, ep: ep, dialer: dialer, nicID: nicID}

	fwd := tcp.NewForwarder(s, 0, 2048, pt.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	return pt
}

func (pt *PacketTunnel) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dest := fmt.Sprintf("%s:%d", id.LocalAddress.String(), id.LocalPort)

	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		utils.Debugf("[PKT] CreateEndpoint %s: %v", dest, tErr)
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(&wq, ep)

	utils.SafeGo("pkt.flow", func() {
		remote, err := pt.dialer.DialTCP(dest)
		if err != nil {
			utils.Debugf("[PKT] dial %s failed: %v", dest, err)
			local.Close()
			return
		}
		// Splice both directions; close when either side ends.
		go func() {
			io.Copy(remote, local)
			remote.Close()
			local.Close()
		}()
		io.Copy(local, remote)
		local.Close()
		remote.Close()
	})
}

// WriteInbound injects one IPv4 packet coming from the device into the stack.
func (pt *PacketTunnel) WriteInbound(ipPacket []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(append([]byte{}, ipPacket...)),
	})
	pt.ep.InjectInbound(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()
}

// ReadOutbound blocks until the stack has a packet to deliver to the device,
// returning its bytes, or nil if ctx is cancelled / the tunnel is closed.
func (pt *PacketTunnel) ReadOutbound(ctx context.Context) []byte {
	p := pt.ep.ReadContext(ctx)
	if p == nil {
		return nil
	}
	data := p.ToView().ToSlice()
	p.DecRef()
	return data
}

func (pt *PacketTunnel) Close() {
	pt.ep.Close()
	pt.stack.Close()
}

