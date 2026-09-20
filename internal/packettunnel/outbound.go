package packettunnel

import "encoding/binary"

type OutboundAction uint8

const (
	Drop OutboundAction = iota
	Forward
	ResolveDNS
	RejectUDP
)

// ClassifyIPv4 validates a device packet before the iOS bridge borrows its
// buffer. Only complete, unfragmented UDP is supported; DNS stays local.
func ClassifyIPv4(packet []byte, udpEnabled bool) (OutboundAction, []byte) {
	if len(packet) < 20 || len(packet) > MaxPacketSize || packet[0]>>4 != 4 {
		return Drop, nil
	}
	ihl := int(packet[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(packet[2:4]))
	if ihl < 20 || total < ihl || total > len(packet) {
		return Drop, nil
	}
	packet = packet[:total]
	switch packet[9] {
	case 6: // TCP
		return Forward, packet
	case 17: // UDP
		if len(packet) < ihl+8 || binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
			return Drop, nil
		}
		udpLen := int(binary.BigEndian.Uint16(packet[ihl+4 : ihl+6]))
		if udpLen < 8 || udpLen > len(packet)-ihl {
			return Drop, nil
		}
		packet = packet[:ihl+udpLen]
		if binary.BigEndian.Uint16(packet[ihl+2:ihl+4]) == 53 {
			return ResolveDNS, packet
		}
		if udpEnabled {
			return Forward, packet
		}
		return RejectUDP, packet
	default:
		return Drop, nil
	}
}
