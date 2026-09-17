package tunnel

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"universal-bypass-tool/network"
	"universal-bypass-tool/utils"
)

type RawSocketEndpoint struct {
	dispatcher      stack.NetworkDispatcher
	sendFd          int
	recvFd          int
	udpRecvFd       int
	nicID           tcpip.NICID
	packetIn        atomic.Uint64
	packetOut       atomic.Uint64
	outgoingSYNs    sync.Map
	activePorts     sync.Map
	activeUDP       sync.Map // egress src port (uint16) -> last-seen unixnano
	sendToTransport func([]byte)
}

func NewRawSocketEndpoint(nicID tcpip.NICID) (*RawSocketEndpoint, error) {
	sendFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return nil, fmt.Errorf("send socket failed: %v (need root)", err)
	}

	if err := syscall.SetsockoptInt(sendFd, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 1); err != nil {
		syscall.Close(sendFd)
		return nil, fmt.Errorf("IP_HDRINCL: %v", err)
	}
	// Large send buffer: a SOCK_RAW socket with IP_HDRINCL gets no kernel
	// auto-tuning, so the default (~208 KiB) caps the bandwidth-delay product
	// and causes drops on high-BDP paths — exactly this exit node, which sits
	// ~100 ms from the covert-channel backend and pushes 30+ Mbps. (Best-effort:
	// the kernel clamps to net.core.wmem_max, so ignore any error.)
	syscall.SetsockoptInt(sendFd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 16*1024*1024)

	recvFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
	if err != nil {
		syscall.Close(sendFd)
		return nil, fmt.Errorf("recv socket failed: %v (need root)", err)
	}
	// Large receive buffer for the same reason (no auto-tuning on SOCK_RAW);
	// the default is not enough at ~100 ms RTT for 30+ Mbps. Clamped to
	// net.core.rmem_max by the kernel, so best-effort.
	syscall.SetsockoptInt(recvFd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 16*1024*1024)

	addr := &syscall.SockaddrInet4{
		Addr: [4]byte{0, 0, 0, 0},
		Port: 0,
	}
	if err := syscall.Bind(recvFd, addr); err != nil {
		syscall.Close(sendFd)
		syscall.Close(recvFd)
		return nil, fmt.Errorf("bind failed: %v", err)
	}

	// Separate raw socket to receive UDP replies (raw sockets are per-protocol).
	udpRecvFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_UDP)
	if err != nil {
		syscall.Close(sendFd)
		syscall.Close(recvFd)
		return nil, fmt.Errorf("udp recv socket failed: %v (need root)", err)
	}
	if err := syscall.Bind(udpRecvFd, addr); err != nil {
		syscall.Close(sendFd)
		syscall.Close(recvFd)
		syscall.Close(udpRecvFd)
		return nil, fmt.Errorf("udp bind failed: %v", err)
	}

	ep := &RawSocketEndpoint{
		sendFd:    sendFd,
		recvFd:    recvFd,
		udpRecvFd: udpRecvFd,
		nicID:     nicID,
	}

	go ep.readLoop()
	go ep.udpReadLoop()
	go ep.udpExpiryLoop()
	return ep, nil
}

// SendUDPOut forwards one client UDP packet to the internet: rewrite the source
// to the egress IP, recompute checksums, remember the source port so the reply
// can be matched, and send it via the IP_HDRINCL raw socket. This bypasses the
// gvisor stack (which only speaks TCP) — UDP is pure L3 NAT here.
func (e *RawSocketEndpoint) SendUDPOut(ipPacket []byte) {
	if len(ipPacket) < 28 { // 20 IP + 8 UDP minimum
		return
	}
	pktCopy := make([]byte, len(ipPacket))
	copy(pktCopy, ipPacket)

	localIP := getLocalIP()
	var localIPBytes [4]byte
	fmt.Sscanf(localIP, "%d.%d.%d.%d", &localIPBytes[0], &localIPBytes[1], &localIPBytes[2], &localIPBytes[3])
	copy(pktCopy[12:16], localIPBytes[:])

	ipHeaderLen := int(pktCopy[0]&0x0F) * 4
	if len(pktCopy) < ipHeaderLen+8 {
		return
	}
	pktCopy[10], pktCopy[11] = 0, 0
	ipck := network.IPChecksum(pktCopy[:ipHeaderLen])
	pktCopy[10] = byte(ipck >> 8)
	pktCopy[11] = byte(ipck & 0xFF)

	udp := pktCopy[ipHeaderLen:]
	srcPort := uint16(udp[0])<<8 | uint16(udp[1])
	srcIPBytes := [4]byte{pktCopy[12], pktCopy[13], pktCopy[14], pktCopy[15]}
	dstIPBytes := [4]byte{pktCopy[16], pktCopy[17], pktCopy[18], pktCopy[19]}
	udp[6], udp[7] = 0, 0
	ck := network.UDPChecksum(udp, srcIPBytes, dstIPBytes)
	udp[6] = byte(ck >> 8)
	udp[7] = byte(ck & 0xFF)

	e.activeUDP.Store(srcPort, time.Now().UnixNano())

	var dst [4]byte
	copy(dst[:], pktCopy[16:20])
	if err := syscall.Sendto(e.sendFd, pktCopy, 0, &syscall.SockaddrInet4{Addr: dst}); err != nil {
		utils.Debugf("[RAW-NIC%d] UDP Sendto failed: %v", e.nicID, err)
		return
	}
	e.packetOut.Add(1)
}

// udpReadLoop reads UDP replies from the internet and relays the ones matching a
// tracked egress port back to the client (dst rewritten to 10.10.10.2).
func (e *RawSocketEndpoint) udpReadLoop() {
	buf := make([]byte, 65535)
	for {
		n, _, err := syscall.Recvfrom(e.udpRecvFd, buf, 0)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			utils.Debugf("[RAW-NIC%d] UDP read error: %v", e.nicID, err)
			return
		}
		if n < 28 {
			continue
		}
		ipHeaderLen := int(buf[0]&0x0F) * 4
		if n < ipHeaderLen+8 {
			continue
		}
		if net.IP(buf[16:20]).String() != getLocalIP() {
			continue
		}
		// The reply's UDP dst port is the egress src port we sent from.
		dstPort := uint16(buf[ipHeaderLen+2])<<8 | uint16(buf[ipHeaderLen+3])
		if _, ok := e.activeUDP.Load(dstPort); !ok {
			continue
		}
		e.activeUDP.Store(dstPort, time.Now().UnixNano())

		pktCopy := make([]byte, n)
		copy(pktCopy, buf[:n])
		copy(pktCopy[16:20], []byte{10, 10, 10, 2})

		pktCopy[10], pktCopy[11] = 0, 0
		ipck := network.IPChecksum(pktCopy[:ipHeaderLen])
		pktCopy[10] = byte(ipck >> 8)
		pktCopy[11] = byte(ipck & 0xFF)

		udp := pktCopy[ipHeaderLen:]
		srcIPBytes := [4]byte{pktCopy[12], pktCopy[13], pktCopy[14], pktCopy[15]}
		dstIPBytes := [4]byte{pktCopy[16], pktCopy[17], pktCopy[18], pktCopy[19]}
		udp[6], udp[7] = 0, 0
		ck := network.UDPChecksum(udp, srcIPBytes, dstIPBytes)
		udp[6] = byte(ck >> 8)
		udp[7] = byte(ck & 0xFF)

		if e.sendToTransport != nil {
			e.sendToTransport(pktCopy)
		}
	}
}

// udpExpiryLoop drops idle UDP port mappings (UDP has no teardown signal).
func (e *RawSocketEndpoint) udpExpiryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-60 * time.Second).UnixNano()
		e.activeUDP.Range(func(k, v any) bool {
			if ts, ok := v.(int64); ok && ts < cutoff {
				e.activeUDP.Delete(k)
			}
			return true
		})
	}
}

func (e *RawSocketEndpoint) SetTransportSender(sendFunc func([]byte)) {
	e.sendToTransport = sendFunc
}

func (e *RawSocketEndpoint) readLoop() {
	buf := make([]byte, 65535)

	for {
		n, _, err := syscall.Recvfrom(e.recvFd, buf, 0)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			utils.Debugf("[RAW-NIC%d] Read error: %v", e.nicID, err)
			return
		}
		if n < 40 {
			continue
		}

		protocol := buf[9]
		flags := buf[33]
		dstIP := net.IP(buf[16:20])
		localIP := getLocalIP()

		if protocol == 6 && dstIP.String() == localIP {
			dstPort := uint16(buf[22])<<8 | uint16(buf[23])

			if _, active := e.activePorts.Load(dstPort); !active {
				continue
			}

			if flags == 0x12 {
				ackNum := uint32(buf[28])<<24 | uint32(buf[29])<<16 | uint32(buf[30])<<8 | uint32(buf[31])
				synSeq := ackNum - 1

				if _, ok := e.outgoingSYNs.Load(synSeq); !ok {
					continue
				}
				e.outgoingSYNs.Delete(synSeq)
			}

			pktCopy := make([]byte, n)
			copy(pktCopy, buf[:n])

			copy(pktCopy[16:20], []byte{10, 10, 10, 2})

			pktCopy[10] = 0
			pktCopy[11] = 0
			ipChecksumVal := network.IPChecksum(pktCopy[:20])
			pktCopy[10] = byte(ipChecksumVal >> 8)
			pktCopy[11] = byte(ipChecksumVal & 0xFF)

			ipHeaderLen := int(pktCopy[0]&0x0F) * 4
			tcpHeader := pktCopy[ipHeaderLen:]
			srcIPBytes := [4]byte{pktCopy[12], pktCopy[13], pktCopy[14], pktCopy[15]}
			dstIPBytes := [4]byte{pktCopy[16], pktCopy[17], pktCopy[18], pktCopy[19]}
			tcpHeader[16] = 0
			tcpHeader[17] = 0
			tcpChecksumVal := network.TCPChecksum(tcpHeader, srcIPBytes, dstIPBytes)
			tcpHeader[16] = byte(tcpChecksumVal >> 8)
			tcpHeader[17] = byte(tcpChecksumVal & 0xFF)

			if e.sendToTransport != nil {
				e.sendToTransport(pktCopy)
			}

			// RST tears the connection down: relay this one so the client
			// closes, then stop tracking the port. Otherwise a server RST
			// storm keeps getting forwarded and floods the low-bandwidth
			// transport channel instead of real data.
			if flags&0x04 != 0 {
				e.activePorts.Delete(dstPort)
			}
		}
	}
}

func (e *RawSocketEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pkt := range pkts.AsSlice() {
		ipPacket := pkt.ToView().ToSlice()
		if len(ipPacket) < 40 {
			continue
		}

		pktCopy := make([]byte, len(ipPacket))
		copy(pktCopy, ipPacket)

		localIP := getLocalIP()
		var localIPBytes [4]byte
		fmt.Sscanf(localIP, "%d.%d.%d.%d", &localIPBytes[0], &localIPBytes[1], &localIPBytes[2], &localIPBytes[3])
		copy(pktCopy[12:16], localIPBytes[:])

		pktCopy[10] = 0
		pktCopy[11] = 0
		ipChecksumVal := network.IPChecksum(pktCopy[:20])
		pktCopy[10] = byte(ipChecksumVal >> 8)
		pktCopy[11] = byte(ipChecksumVal & 0xFF)

		ipHeaderLen := int(pktCopy[0]&0x0F) * 4
		tcpHeader := pktCopy[ipHeaderLen:]
		srcIPBytes := [4]byte{pktCopy[12], pktCopy[13], pktCopy[14], pktCopy[15]}
		dstIPBytes := [4]byte{pktCopy[16], pktCopy[17], pktCopy[18], pktCopy[19]}
		tcpHeader[16] = 0
		tcpHeader[17] = 0
		tcpChecksumVal := network.TCPChecksum(tcpHeader, srcIPBytes, dstIPBytes)
		tcpHeader[16] = byte(tcpChecksumVal >> 8)
		tcpHeader[17] = byte(tcpChecksumVal & 0xFF)

		srcPort := uint16(tcpHeader[0])<<8 | uint16(tcpHeader[1])

		if tcpHeader[13]&0x02 != 0 {
			seqNum := uint32(tcpHeader[4])<<24 | uint32(tcpHeader[5])<<16 | uint32(tcpHeader[6])<<8 | uint32(tcpHeader[7])
			e.outgoingSYNs.Store(seqNum, true)
			e.activePorts.Store(srcPort, true)
		}

		if tcpHeader[13]&0x01 != 0 || tcpHeader[13]&0x04 != 0 {
			dstPort := uint16(tcpHeader[2])<<8 | uint16(tcpHeader[3])
			e.activePorts.Delete(dstPort)
		}

		var dst [4]byte
		copy(dst[:], pktCopy[16:20])

		addr := &syscall.SockaddrInet4{
			Addr: dst,
			Port: 0,
		}

		if err := syscall.Sendto(e.sendFd, pktCopy, 0, addr); err != nil {
			utils.Debugf("[RAW-NIC%d] Sendto failed: %v", e.nicID, err)
			continue
		}

		e.packetOut.Add(1)
		n++
	}
	return n, nil
}

func (e *RawSocketEndpoint) MTU() uint32                                 { return 1500 }
func (e *RawSocketEndpoint) MaxHeaderLength() uint16                      { return 0 }
func (e *RawSocketEndpoint) LinkAddress() tcpip.LinkAddress               { return "" }
func (e *RawSocketEndpoint) Capabilities() stack.LinkEndpointCapabilities { return stack.CapabilityNone }
func (e *RawSocketEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatcher = dispatcher
}
func (e *RawSocketEndpoint) IsAttached() bool                             { return e.dispatcher != nil }
func (e *RawSocketEndpoint) Wait()                                        {}
func (e *RawSocketEndpoint) ARPHardwareType() header.ARPHardwareType      { return header.ARPHardwareNone }
func (e *RawSocketEndpoint) AddHeader(*stack.PacketBuffer)                {}
func (e *RawSocketEndpoint) Close() {
	syscall.Close(e.sendFd)
	syscall.Close(e.recvFd)
	syscall.Close(e.udpRecvFd)
}
func (e *RawSocketEndpoint) SetMTU(uint32)                                {}
func (e *RawSocketEndpoint) SetLinkAddress(tcpip.LinkAddress)             {}
func (e *RawSocketEndpoint) ParseHeader(*stack.PacketBuffer) bool         { return true }
func (e *RawSocketEndpoint) SetOnCloseAction(func())                      {}
