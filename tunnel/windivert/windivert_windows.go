//go:build windows

package windivert

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/xjasonlyu/windivert-go"

	"universal-bypass-tool/network"
	"universal-bypass-tool/utils"
)

const recvBufSize = 65535

type Endpoint struct {
	handle windivert.Handle

	ourIP4 [4]byte
	mac    [6]byte

	gvisorPorts sync.Map // map[uint16]bool

	packetIn   atomic.Uint64
	packetOut  atomic.Uint64
	packetSkip atomic.Uint64
	rstDropped atomic.Uint64

	onPacket func([]byte)
	mu       sync.Mutex
	closed   atomic.Bool
}

func New() (*Endpoint, error) {
	ip, mac, err := findLocalIPMAC()
	if err != nil {
		return nil, fmt.Errorf("find ip: %w", err)
	}

	h, err := windivert.Open("tcp", windivert.LayerNetwork,
		windivert.PriorityDefault, windivert.FlagDefault)
	if err != nil {
		return nil, fmt.Errorf("windivert open: %w", err)
	}

	// Очередь побольше. Если не сработает — не критично.
	_ = h.SetParam(windivert.QueueLength, 8192)
	ep := &Endpoint{
		handle: h,
	}
	copy(ep.ourIP4[:], ip.To4())
	copy(ep.mac[:], mac)

	go ep.readLoop()

	utils.Debugf("[WD] Interface: IP=%s MAC=%s", ip, mac)
	return ep, nil
}

func (e *Endpoint) IP() [4]byte  { return e.ourIP4 }
func (e *Endpoint) MAC() [6]byte { return e.mac }

func (e *Endpoint) SetOnPacket(cb func([]byte)) {
	e.mu.Lock()
	e.onPacket = cb
	e.mu.Unlock()
}

func (e *Endpoint) readLoop() {
	buf := make([]byte, recvBufSize)

	for {
		if e.closed.Load() {
			return
		}

		var addr windivert.Address
		n, err := e.handle.Recv(buf, &addr)
		if err != nil {
			if e.closed.Load() {
				return
			}
			utils.Debugf("[WD] Recv error: %v", err)
			continue
		}
		if n < 20 {
			_, _ = e.handle.Send(buf[:n], &addr)
			continue
		}

		pkt := buf[:n]
		if pkt[0]>>4 != 4 || pkt[9] != 6 {
			_, _ = e.handle.Send(pkt, &addr)
			continue
		}

		ipHdrLen := int(pkt[0]&0x0f) * 4
		if len(pkt) < ipHdrLen+20 {
			_, _ = e.handle.Send(pkt, &addr)
			continue
		}
		tcpHdr := pkt[ipHdrLen:]
		srcPort := uint16(tcpHdr[0])<<8 | uint16(tcpHdr[1])
		dstPort := uint16(tcpHdr[2])<<8 | uint16(tcpHdr[3])
		flags := tcpHdr[13]

		if addr.Outbound() {
			// RST от Windows-стека для порта gVisor → дроп
			if flags&0x04 != 0 {
				if _, ok := e.gvisorPorts.Load(srcPort); ok {
					e.rstDropped.Add(1)
					utils.Debugf("[WD] Dropped outbound RST for port %d", srcPort)
					continue
				}
			}
			_, _ = e.handle.Send(pkt, &addr)
			continue
		}

		// Входящий из сети
		if _, ok := e.gvisorPorts.Load(dstPort); !ok {
			e.packetSkip.Add(1)
			_, _ = e.handle.Send(pkt, &addr)
			continue
		}

		// НАШ пакет. Отдаём копию в gVisor, но НЕ пропускаем в Windows-стек.
		pktCopy := make([]byte, len(pkt))
		copy(pktCopy, pkt)

		// DNAT: dst IP → 10.10.10.2
		copy(pktCopy[16:20], []byte{10, 10, 10, 2})
		recalcIPChecksum(pktCopy)
		recalcTCPChecksum(pktCopy)

		e.packetIn.Add(1)

		e.mu.Lock()
		cb := e.onPacket
		e.mu.Unlock()
		if cb != nil {
			cb(pktCopy)
		}
		// НЕ вызываем Send — пакет дропается
	}
}

func (e *Endpoint) Send(ipPacket []byte) error {
	if len(ipPacket) < 20 {
		return fmt.Errorf("packet too short: %d", len(ipPacket))
	}
	if ipPacket[0]>>4 != 4 {
		return fmt.Errorf("not ipv4")
	}

	pktCopy := make([]byte, len(ipPacket))
	copy(pktCopy, ipPacket)

	// SNAT
	copy(pktCopy[12:16], e.ourIP4[:])

	// IP checksum
	pktCopy[10] = 0
	pktCopy[11] = 0
	ipCk := network.IPChecksum(pktCopy[:20])
	pktCopy[10] = byte(ipCk >> 8)
	pktCopy[11] = byte(ipCk & 0xff)

	if pktCopy[9] == 6 {
		ipHdrLen := int(pktCopy[0]&0x0f) * 4
		if len(pktCopy) >= ipHdrLen+20 {
			tcpHdr := pktCopy[ipHdrLen:]
			srcPort := uint16(tcpHdr[0])<<8 | uint16(tcpHdr[1])
			flags := tcpHdr[13]

			if flags&0x02 != 0 && flags&0x10 == 0 {
				e.gvisorPorts.Store(srcPort, true)
			}
			if flags&0x01 != 0 || flags&0x04 != 0 {
				e.gvisorPorts.Delete(srcPort)
			}

			tcpHdr[16] = 0
			tcpHdr[17] = 0
			srcBytes := [4]byte{pktCopy[12], pktCopy[13], pktCopy[14], pktCopy[15]}
			dstBytes := [4]byte{pktCopy[16], pktCopy[17], pktCopy[18], pktCopy[19]}
			tcpCk := network.TCPChecksum(tcpHdr, srcBytes, dstBytes)
			tcpHdr[16] = byte(tcpCk >> 8)
			tcpHdr[17] = byte(tcpCk & 0xff)
		}
	}

	addr := &windivert.Address{}
	addr.SetLayer(windivert.LayerNetwork)
	addr.SetOutbound()

	if _, err := e.handle.Send(pktCopy, addr); err != nil {
		return fmt.Errorf("windivert send: %w", err)
	}

	e.packetOut.Add(1)
	return nil
}

func (e *Endpoint) Close() {
	if !e.closed.CompareAndSwap(false, true) {
		return
	}
	_ = e.handle.Close()
}

func (e *Endpoint) Stats() (in, out, skip, rstDropped uint64) {
	return e.packetIn.Load(),
		e.packetOut.Load(),
		e.packetSkip.Load(),
		e.rstDropped.Load()
}

func findLocalIPMAC() (net.IP, net.HardwareAddr, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, nil, fmt.Errorf("dial udp: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	ip := localAddr.IP.To4()
	if ip == nil {
		return nil, nil, fmt.Errorf("no ipv4")
	}

	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.Equal(ip) {
				return ip, i.HardwareAddr, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no mac for %s", ip)
}

func recalcIPChecksum(pkt []byte) {
	if len(pkt) < 20 {
		return
	}
	pkt[10] = 0
	pkt[11] = 0
	ck := network.IPChecksum(pkt[:20])
	pkt[10] = byte(ck >> 8)
	pkt[11] = byte(ck & 0xff)
}

func recalcTCPChecksum(pkt []byte) {
	if len(pkt) < 20 || pkt[9] != 6 {
		return
	}
	ipHdrLen := int(pkt[0]&0x0f) * 4
	if len(pkt) < ipHdrLen+20 {
		return
	}
	tcpHdr := pkt[ipHdrLen:]
	tcpHdr[16] = 0
	tcpHdr[17] = 0
	srcBytes := [4]byte{pkt[12], pkt[13], pkt[14], pkt[15]}
	dstBytes := [4]byte{pkt[16], pkt[17], pkt[18], pkt[19]}
	ck := network.TCPChecksum(tcpHdr, srcBytes, dstBytes)
	tcpHdr[16] = byte(ck >> 8)
	tcpHdr[17] = byte(ck & 0xff)
}
