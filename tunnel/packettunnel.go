package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
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
// The transport is TCP-only, so raw UDP is not carried; UDP port 53 is special
// cased and proxied as DNS-over-TCP through the tunnel so name resolution works
// (and bypasses local DNS poisoning). Other UDP is dropped.
type PacketTunnel struct {
	stack  *stack.Stack
	ep     *channel.Endpoint
	dialer TCPDialer
	nicID  tcpip.NICID
}

// NewPacketTunnel builds the stack and installs TCP + DNS forwarders.
func NewPacketTunnel(dialer TCPDialer, mtu uint32) *PacketTunnel {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	SetTCPBuffers(s)

	ep := channel.New(256, mtu, "")
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

	tcpFwd := tcp.NewForwarder(s, 0, 2048, pt.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	udpFwd := udp.NewForwarder(s, pt.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
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

// handleUDP only serves DNS (port 53): the query is proxied as DNS-over-TCP
// through the tunnel. Any other UDP is dropped (TCP-only transport).
func (pt *PacketTunnel) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	if id.LocalPort != 53 {
		return false // not handled -> dropped (only DNS is supported)
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		utils.Debugf("[PKT] UDP CreateEndpoint: %v", err)
		return true
	}
	conn := gonet.NewUDPConn(&wq, ep)
	dest := fmt.Sprintf("%s:53", id.LocalAddress.String())

	utils.SafeGo("pkt.dns", func() {
		defer conn.Close()
		buf := make([]byte, 1500)
		for {
			conn.SetReadDeadline(time.Now().Add(8 * time.Second))
			n, err := conn.Read(buf)
			if err != nil || n == 0 {
				return
			}
			resp, err := pt.dnsOverTCP(dest, buf[:n])
			if err != nil {
				utils.Debugf("[PKT] DNS-over-TCP %s: %v", dest, err)
				return
			}
			if _, err := conn.Write(resp); err != nil {
				return
			}
		}
	})
	return true
}

// dnsOverTCP sends a DNS query to dest ("ip:53") over a TCP connection through
// the tunnel (RFC 7766 length-prefixed framing) and returns the response.
func (pt *PacketTunnel) dnsOverTCP(dest string, query []byte) ([]byte, error) {
	c, err := pt.dialer.DialTCP(dest)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))

	var lp [2]byte
	binary.BigEndian.PutUint16(lp[:], uint16(len(query)))
	if _, err := c.Write(append(lp[:], query...)); err != nil {
		return nil, err
	}

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(hdr))
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	return resp, nil
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
