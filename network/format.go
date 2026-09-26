package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// PacketDirection is the arrow shown before the packet summary.
type PacketDirection int

const (
	// DirInbound is data arriving at the observer: "<-" or "<".
	// Used on the exit for packets coming from the network, on the client
	// for packets coming from the tunnel back to the device.
	DirInbound PacketDirection = iota
	// DirOutbound is data leaving the observer: "->" or ">".
	// Used on the exit for packets going to the network, on the client for
	// packets going into the tunnel.
	DirOutbound
)

// Arrow returns the ASCII arrow for d, followed by a space.
func (d PacketDirection) Arrow() string {
	if d == DirInbound {
		return "<- "
	}
	return "-> "
}

// FormatPacket renders a raw IPv4 packet in the canonical one-line log form:
//
//	<- 40 bytes - TCP 10.10.10.2:53725 -> 216.58.205.163:443 [RST] seq=... ack=0 win=0 len=40 ttl=64
//	-> 52 bytes - UDP 10.10.10.2:53000 -> 8.8.8.8:53 len=42 ttl=64
//	<- 84 bytes - ICMP 203.0.113.1 -> 10.10.10.2 type=3 code=4 len=84 ttl=55
//	-> 40 bytes - IP58 10.0.0.1 -> 10.0.0.2 len=40 ttl=64
//
// The first number is the packet length as passed in; the "TCP/UDP/..."
// token is the transport tag; the trailing part is the same summary
// tcpdump-style output the project used before.
func FormatPacket(dir PacketDirection, pkt []byte) string {
	return fmt.Sprintf("%s%d bytes - %s", dir.Arrow(), len(pkt), summarize(pkt))
}

// summarize is the shared body of DescribeIPv4 and FormatPacket.
func summarize(pkt []byte) string {
	if len(pkt) < 20 {
		return fmt.Sprintf("short (%d bytes)", len(pkt))
	}
	if pkt[0]>>4 != 4 {
		return fmt.Sprintf("non-IPv4 version=%d len=%d", pkt[0]>>4, len(pkt))
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return fmt.Sprintf("bad IHL=%d len=%d", ihl, len(pkt))
	}
	src := net.IP(pkt[12:16])
	dst := net.IP(pkt[16:20])
	proto := pkt[9]
	totalLen := int(binary.BigEndian.Uint16(pkt[2:4]))
	ttl := pkt[8]

	fragFlags := binary.BigEndian.Uint16(pkt[6:8])
	fragOff := int(fragFlags&0x1fff) * 8
	fragMore := fragFlags&0x2000 != 0
	fragStr := ""
	if fragOff != 0 || fragMore {
		fragStr = fmt.Sprintf(" frag(off=%d more=%v)", fragOff, fragMore)
	}

	switch proto {
	case 6:
		if len(pkt) < ihl+20 {
			return fmt.Sprintf("TCP %s -> %s truncated (ihl=%d len=%d)", src, dst, ihl, len(pkt))
		}
		tcp := pkt[ihl:]
		srcPort := binary.BigEndian.Uint16(tcp[0:2])
		dstPort := binary.BigEndian.Uint16(tcp[2:4])
		seq := binary.BigEndian.Uint32(tcp[4:8])
		ack := binary.BigEndian.Uint32(tcp[8:12])
		flags := tcp[13]
		win := binary.BigEndian.Uint16(tcp[14:16])
		return fmt.Sprintf("TCP %s:%d -> %s:%d [%s] seq=%d ack=%d win=%d len=%d ttl=%d%s",
			src, srcPort, dst, dstPort, tcpFlags(flags), seq, ack, win, totalLen, ttl, fragStr)

	case 17:
		if len(pkt) < ihl+8 {
			return fmt.Sprintf("UDP %s -> %s truncated (ihl=%d len=%d)", src, dst, ihl, len(pkt))
		}
		udp := pkt[ihl:]
		srcPort := binary.BigEndian.Uint16(udp[0:2])
		dstPort := binary.BigEndian.Uint16(udp[2:4])
		udpLen := binary.BigEndian.Uint16(udp[4:6])
		return fmt.Sprintf("UDP %s:%d -> %s:%d len=%d ttl=%d%s",
			src, srcPort, dst, dstPort, udpLen, ttl, fragStr)

	case 1:
		if len(pkt) < ihl+4 {
			return fmt.Sprintf("ICMP %s -> %s truncated", src, dst)
		}
		typ, code := pkt[ihl], pkt[ihl+1]
		return fmt.Sprintf("ICMP %s -> %s type=%d code=%d len=%d ttl=%d%s",
			src, dst, typ, code, totalLen, ttl, fragStr)

	default:
		return fmt.Sprintf("IP%d %s -> %s len=%d ttl=%d%s",
			proto, src, dst, totalLen, ttl, fragStr)
	}
}

// ShortProto returns a compact protocol tag: "TCP", "UDP", "ICMP", "IP58".
func ShortProto(pkt []byte) string {
	if len(pkt) < 10 {
		return "??"
	}
	switch pkt[9] {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	case 1:
		return "ICMP"
	default:
		return fmt.Sprintf("IP%d", pkt[9])
	}
}

// DescribeIPv4 is a compatibility shim for callers that only want the body
// without an arrow, e.g. when the direction is already known from context.
func DescribeIPv4(pkt []byte) string { return summarize(pkt) }

func tcpFlags(f byte) string {
	var b strings.Builder
	if f&0x02 != 0 {
		b.WriteString("SYN ")
	}
	if f&0x10 != 0 {
		b.WriteString("ACK ")
	}
	if f&0x01 != 0 {
		b.WriteString("FIN ")
	}
	if f&0x04 != 0 {
		b.WriteString("RST ")
	}
	if f&0x08 != 0 {
		b.WriteString("PSH ")
	}
	if f&0x20 != 0 {
		b.WriteString("URG ")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "-"
	}
	return out
}
