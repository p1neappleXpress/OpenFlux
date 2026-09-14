package network

import (
	"fmt"
	"net"
	"strings"
)

func TCPChecksum(tcpData []byte, srcIP, dstIP [4]byte) uint16 {
	pseudoHeader := []byte{
		srcIP[0], srcIP[1], srcIP[2], srcIP[3],
		dstIP[0], dstIP[1], dstIP[2], dstIP[3],
		0, 6,
		0, 0,
	}

	tcpLen := len(tcpData)
	pseudoHeader[10] = byte(tcpLen >> 8)
	pseudoHeader[11] = byte(tcpLen & 0xff)

	all := make([]byte, 0, len(pseudoHeader)+tcpLen)
	all = append(all, pseudoHeader...)
	all = append(all, tcpData...)

	sum := uint32(0)
	for i := 0; i < len(all)-1; i += 2 {
		sum += uint32(all[i])<<8 | uint32(all[i+1])
	}
	if len(all)%2 == 1 {
		sum += uint32(all[len(all)-1]) << 8
	}

	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}

	return uint16(^sum)
}

// UDPChecksum computes the UDP checksum over the pseudo-header + UDP datagram.
// The caller must zero the checksum field (bytes 6-7 of udpData) beforehand.
// Per RFC 768 a computed value of 0 is transmitted as 0xFFFF (0 means "none").
func UDPChecksum(udpData []byte, srcIP, dstIP [4]byte) uint16 {
	udpLen := len(udpData)
	pseudoHeader := []byte{
		srcIP[0], srcIP[1], srcIP[2], srcIP[3],
		dstIP[0], dstIP[1], dstIP[2], dstIP[3],
		0, 17,
		byte(udpLen >> 8), byte(udpLen & 0xff),
	}

	all := make([]byte, 0, len(pseudoHeader)+udpLen)
	all = append(all, pseudoHeader...)
	all = append(all, udpData...)

	sum := uint32(0)
	for i := 0; i < len(all)-1; i += 2 {
		sum += uint32(all[i])<<8 | uint32(all[i+1])
	}
	if len(all)%2 == 1 {
		sum += uint32(all[len(all)-1]) << 8
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	csum := uint16(^sum)
	if csum == 0 {
		return 0xFFFF
	}
	return csum
}

func IPChecksum(b []byte) uint16 {
	sum := uint32(0)
	for i := 0; i < len(b)-1; i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	sum = (sum >> 16) + (sum & 0xFFFF)
	sum += sum >> 16
	return uint16(^sum)
}

func ParsePacketInfo(data []byte) string {
	if len(data) < 20 {
		return fmt.Sprintf("short packet (%d bytes)", len(data))
	}
	srcIP := net.IP(data[12:16])
	dstIP := net.IP(data[16:20])
	protocol := data[9]
	ttl := data[8]
	totalLen := uint16(data[2])<<8 | uint16(data[3])

	if protocol == 6 && len(data) >= 40 {
		srcPort := uint16(data[20])<<8 | uint16(data[21])
		dstPort := uint16(data[22])<<8 | uint16(data[23])
		flags := data[33]
		seq := uint32(data[24])<<24 | uint32(data[25])<<16 | uint32(data[26])<<8 | uint32(data[27])
		ack := uint32(data[28])<<24 | uint32(data[29])<<16 | uint32(data[30])<<8 | uint32(data[31])
		window := uint16(data[34])<<8 | uint16(data[35])

		flagStr := ""
		if flags&0x02 != 0 {
			flagStr += "SYN "
		}
		if flags&0x10 != 0 {
			flagStr += "ACK "
		}
		if flags&0x01 != 0 {
			flagStr += "FIN "
		}
		if flags&0x04 != 0 {
			flagStr += "RST "
		}
		if flags&0x08 != 0 {
			flagStr += "PSH "
		}

		return fmt.Sprintf("TCP %s:%d -> %s:%d [%s] seq=%d ack=%d win=%d len=%d ttl=%d",
			srcIP, srcPort, dstIP, dstPort, strings.TrimSpace(flagStr), seq, ack, window, totalLen, ttl)
	}
	return fmt.Sprintf("IP proto=%d %s -> %s len=%d ttl=%d", protocol, srcIP, dstIP, totalLen, ttl)
}
