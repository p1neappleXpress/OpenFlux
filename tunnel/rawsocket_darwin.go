//go:build darwin

package tunnel

import (
	"fmt"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type RawSocketEndpoint struct {
	packetIn  atomic.Uint64
	packetOut atomic.Uint64
}

func NewRawSocketEndpoint(nicID tcpip.NICID) (*RawSocketEndpoint, error) {
	return nil, fmt.Errorf("raw socket mode is not supported on darwin; use --mode proxy")
}

func (e *RawSocketEndpoint) SetTransportSender(func([]byte))              {}
func (e *RawSocketEndpoint) WritePackets(stack.PacketBufferList) (int, tcpip.Error) { return 0, nil }
func (e *RawSocketEndpoint) MTU() uint32                                  { return 1500 }
func (e *RawSocketEndpoint) MaxHeaderLength() uint16                      { return 0 }
func (e *RawSocketEndpoint) LinkAddress() tcpip.LinkAddress               { return "" }
func (e *RawSocketEndpoint) Capabilities() stack.LinkEndpointCapabilities { return stack.CapabilityNone }
func (e *RawSocketEndpoint) Attach(stack.NetworkDispatcher)               {}
func (e *RawSocketEndpoint) IsAttached() bool                             { return false }
func (e *RawSocketEndpoint) Wait()                                        {}
func (e *RawSocketEndpoint) ARPHardwareType() header.ARPHardwareType      { return header.ARPHardwareNone }
func (e *RawSocketEndpoint) AddHeader(*stack.PacketBuffer)                {}
func (e *RawSocketEndpoint) Close()                                       {}
func (e *RawSocketEndpoint) SetMTU(uint32)                                {}
func (e *RawSocketEndpoint) SetLinkAddress(tcpip.LinkAddress)             {}
func (e *RawSocketEndpoint) ParseHeader(*stack.PacketBuffer) bool         { return true }
func (e *RawSocketEndpoint) SetOnCloseAction(func())                      {}
