//go:build windows

package tunnel

import (
	"fmt"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"universal-bypass-tool/network"
	"universal-bypass-tool/tunnel/windivert"
	"universal-bypass-tool/utils"
)

type RawSocketEndpoint struct {
	dispatcher stack.NetworkDispatcher
	ep         *windivert.Endpoint
	nicID      tcpip.NICID
	packetIn   atomic.Uint64
	packetOut  atomic.Uint64
	closeOnce  sync.Once
}

func NewRawSocketEndpoint(nicID tcpip.NICID) (*RawSocketEndpoint, error) {
	ep, err := windivert.New()
	if err != nil {
		return nil, fmt.Errorf("windivert: %w", err)
	}

	utils.Debugf("[WD-NIC%d] Interface: IP=%v MAC=%v",
		nicID, ep.IP(), ep.MAC())

	r := &RawSocketEndpoint{
		ep:    ep,
		nicID: nicID,
	}

	ep.SetOnPacket(func(data []byte) {
		r.packetIn.Add(1)
		utils.Debugf("[WD-NIC%d] <- %d bytes: %s",
			r.nicID, len(data), network.ParsePacketInfo(data))

		if r.dispatcher == nil {
			return
		}

		pktCopy := make([]byte, len(data))
		copy(pktCopy, data)

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(pktCopy),
		})
		defer pkt.DecRef()

		r.dispatcher.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkt)
	})

	return r, nil
}

func (e *RawSocketEndpoint) SetTransportSender(sendFunc func([]byte)) {}

func (e *RawSocketEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pkt := range pkts.AsSlice() {
		ipPacket := pkt.ToView().ToSlice()
		if len(ipPacket) < 20 {
			continue
		}

		pktCopy := make([]byte, len(ipPacket))
		copy(pktCopy, ipPacket)

		utils.Debugf("[WD-NIC%d] -> %d bytes: %s",
			e.nicID, len(pktCopy), network.ParsePacketInfo(pktCopy))

		if err := e.ep.Send(pktCopy); err != nil {
			utils.Debugf("[WD-NIC%d] Send error: %v", e.nicID, err)
			continue
		}
		e.packetOut.Add(1)
		n++
	}
	return n, nil
}

func (e *RawSocketEndpoint) MTU() uint32 { return 1500 }
func (e *RawSocketEndpoint) MaxHeaderLength() uint16 { return 0 }
func (e *RawSocketEndpoint) LinkAddress() tcpip.LinkAddress {
	mac := e.ep.MAC()
	return tcpip.LinkAddress(mac[:])
}
func (e *RawSocketEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}
func (e *RawSocketEndpoint) Attach(d stack.NetworkDispatcher) { e.dispatcher = d }
func (e *RawSocketEndpoint) IsAttached() bool { return e.dispatcher != nil }
func (e *RawSocketEndpoint) Wait() {}
func (e *RawSocketEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}
func (e *RawSocketEndpoint) AddHeader(*stack.PacketBuffer) {}
func (e *RawSocketEndpoint) Close() {
	e.closeOnce.Do(func() {
		e.ep.Close()
	})
}
func (e *RawSocketEndpoint) SetMTU(uint32) {}
func (e *RawSocketEndpoint) SetLinkAddress(tcpip.LinkAddress) {}
func (e *RawSocketEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }
func (e *RawSocketEndpoint) SetOnCloseAction(func()) {}
