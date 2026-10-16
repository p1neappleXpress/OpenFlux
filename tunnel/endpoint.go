package tunnel

import (
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type TunnelLinkEndpoint struct {
	mu               sync.RWMutex
	dispatcher       stack.NetworkDispatcher
	onOutgoingPacket func([]byte)
	// onOutgoingFill, when set, takes a packet by size and a function that
	// copies it into the buffer the sender built (transport.FillSender):
	// the packet is copied once from the stack, not twice.
	onOutgoingFill func(size int, fill func(dst []byte))
	packetIn         atomic.Uint64
	packetOut        atomic.Uint64
	mtu              atomic.Uint32
}

func NewTunnelLinkEndpoint() *TunnelLinkEndpoint {
	return &TunnelLinkEndpoint{}
}

func (e *TunnelLinkEndpoint) InjectInbound(data []byte) {
	e.mu.RLock()
	dispatcher := e.dispatcher
	e.mu.RUnlock()
	if dispatcher == nil {
		return
	}
	e.packetIn.Add(1)
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		// MakeWithData copies data into the stack's own pooled chunk, so the
		// caller keeps its buffer and no copy of ours is needed.
		Payload: buffer.MakeWithData(data),
	})
	defer pkt.DecRef()
	dispatcher.DeliverNetworkPacket(ipv4.ProtocolNumber, pkt)
}

func (e *TunnelLinkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pkt := range pkts.AsSlice() {
		e.packetOut.Add(1)
		if e.onOutgoingFill != nil {
			vl, skip := pkt.AsViewList()
			e.onOutgoingFill(pkt.Size(), func(dst []byte) {
				copyViews(dst, vl, skip)
			})
			n++
			continue
		}
		view := pkt.ToView()
		data := view.ToSlice()
		if e.onOutgoingPacket != nil {
			e.onOutgoingPacket(data)
		}
		view.Release()
		n++
	}
	return n, nil
}

func (e *TunnelLinkEndpoint) MTU() uint32 {
	if m := e.mtu.Load(); m != 0 {
		return m
	}
	return 1500
}
func (e *TunnelLinkEndpoint) MaxHeaderLength() uint16        { return 0 }
func (e *TunnelLinkEndpoint) LinkAddress() tcpip.LinkAddress { return "\x02\x00\x00\x00\x00\x01" }
func (e *TunnelLinkEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}
func (e *TunnelLinkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = dispatcher
}
func (e *TunnelLinkEndpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dispatcher != nil
}
func (e *TunnelLinkEndpoint) Wait()                                   {}
func (e *TunnelLinkEndpoint) ARPHardwareType() header.ARPHardwareType { return header.ARPHardwareNone }
func (e *TunnelLinkEndpoint) AddHeader(*stack.PacketBuffer)           {}
func (e *TunnelLinkEndpoint) Close()                                  {}
func (e *TunnelLinkEndpoint) SetMTU(m uint32) {
	if m >= 1280 && m <= 65000 {
		e.mtu.Store(m)
	}
}
func (e *TunnelLinkEndpoint) SetLinkAddress(tcpip.LinkAddress)     {}
func (e *TunnelLinkEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }
func (e *TunnelLinkEndpoint) SetOnCloseAction(func())              {}

// copyViews copies the bytes of a packet's view list, after its first skip
// bytes (the header room the stack reserved), into dst.
func copyViews(dst []byte, vl buffer.ViewList, skip int) {
	for v := vl.Front(); v != nil && len(dst) > 0; v = v.Next() {
		b := v.AsSlice()
		if skip >= len(b) {
			skip -= len(b)
			continue
		}
		n := copy(dst, b[skip:])
		dst = dst[n:]
		skip = 0
	}
}
