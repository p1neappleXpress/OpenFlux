//go:build windows

package netbind

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Adapter types (ipifcons.h) that stand for each network.
var adapterTypes = map[string][]uint32{
	NetworkWiFi:     {71},       // IF_TYPE_IEEE80211
	NetworkEthernet: {6},        // IF_TYPE_ETHERNET_CSMACD
	NetworkCellular: {243, 244}, // IF_TYPE_WWANPP, IF_TYPE_WWANPP2
}

func init() { SetNetworkBinder(bindAdapter) }

// bindAdapter binds fd to the first adapter of the network's type that is
// up and has a gateway (virtual adapters without one are skipped).
func bindAdapter(network string, fd uintptr) error {
	index, err := adapterIndex(network)
	if err != nil {
		return err
	}
	h := windows.Handle(fd)
	be := int(index>>24 | (index>>8)&0xff00 | (index<<8)&0xff0000 | index<<24)
	err4 := windows.SetsockoptInt(h, windows.IPPROTO_IP, ipUnicastIf, be)
	err6 := windows.SetsockoptInt(h, windows.IPPROTO_IPV6, ipv6UnicastIf, int(index))
	if err4 != nil && err6 != nil {
		return fmt.Errorf("привязка к адаптеру %d: %v", index, err4)
	}
	return nil
}

func adapterIndex(network string) (uint32, error) {
	types, ok := adapterTypes[network]
	if !ok {
		return 0, fmt.Errorf("неизвестная сеть")
	}
	size := uint32(16 * 1024)
	for {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_GATEWAYS, 0, first, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("список адаптеров: %w", err)
		}
		for a := first; a != nil; a = a.Next {
			if a.OperStatus != windows.IfOperStatusUp || a.FirstGatewayAddress == nil {
				continue
			}
			for _, t := range types {
				if a.IfType == t {
					if a.IfIndex != 0 {
						return a.IfIndex, nil
					}
					return a.Ipv6IfIndex, nil
				}
			}
		}
		return 0, fmt.Errorf("нет подключённого адаптера этого типа")
	}
}
