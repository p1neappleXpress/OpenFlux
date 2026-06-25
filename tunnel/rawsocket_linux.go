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
	nicID           tcpip.NICID
	packetIn        atomic.Uint64
	packetOut       atomic.Uint64
	outgoingSYNs    sync.Map
	activePorts     sync.Map
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

	recvFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
	if err != nil {
		syscall.Close(sendFd)
		return nil, fmt.Errorf("recv socket failed: %v (need root)", err)
	}

	addr := &syscall.SockaddrInet4{
		Addr: [4]byte{0, 0, 0, 0},
		Port: 0,
	}
	if err := syscall.Bind(recvFd, addr); err != nil {
		syscall.Close(sendFd)
		syscall.Close(recvFd)
		return nil, fmt.Errorf("bind failed: %v", err)
	}

	ep := &RawSocketEndpoint{
		sendFd: sendFd,
		recvFd: recvFd,
		nicID:  nicID,
	}

	go ep.readLoop()
	return ep, nil
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
}
func (e *RawSocketEndpoint) SetMTU(uint32)                                {}
func (e *RawSocketEndpoint) SetLinkAddress(tcpip.LinkAddress)             {}
func (e *RawSocketEndpoint) ParseHeader(*stack.PacketBuffer) bool         { return true }
func (e *RawSocketEndpoint) SetOnCloseAction(func())                      {}
