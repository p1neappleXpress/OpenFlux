package l3

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const maxUDPMappings = 256

type udpMapping struct {
	client   flowKey
	wire     flowKey
	port     io.Closer
	lastSeen time.Time
}

// udpNAT owns a real kernel UDP socket for every translated flow. Raw receive
// sockets tap packets without consuming them from the kernel UDP handler; an
// unbound destination would otherwise cause an ICMP port-unreachable response.
// Port 0 lets the kernel choose a free port without racing host applications.
// Mapping is endpoint-dependent: only the exact remote IP:port may reply.
type udpNAT struct {
	mu      sync.Mutex
	forward map[flowKey]*udpMapping
	reverse map[flowKey]*udpMapping
	egress  [4]byte
	reserve func([4]byte) (uint16, io.Closer, error)
	closed  bool
	stop    chan struct{}
	done    chan struct{}
}

func newUDPNAT(egress [4]byte) *udpNAT {
	n := &udpNAT{
		forward: make(map[flowKey]*udpMapping),
		reverse: make(map[flowKey]*udpMapping),
		egress:  egress,
		reserve: reserveUDPPort,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(n.done)
		ticker := time.NewTicker(ctSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-n.stop:
				return
			case now := <-ticker.C:
				n.mu.Lock()
				n.expire(now)
				n.mu.Unlock()
			}
		}
	}()
	return n
}

func reserveUDPPort(ip [4]byte) (uint16, io.Closer, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IP(ip[:])})
	if err != nil {
		return 0, nil, err
	}
	// Raw sockets deliver the data. The kernel's duplicate UDP queue is never
	// read here; bound its memory instead of starting a goroutine per mapping.
	if err := conn.SetReadBuffer(2048); err != nil {
		_ = conn.Close()
		return 0, nil, err
	}
	return uint16(conn.LocalAddr().(*net.UDPAddr).Port), conn, nil
}

func (n *udpNAT) send(pkt []byte, key flowKey, send func([]byte) error) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return net.ErrClosed
	}
	now := time.Now()
	m := n.forward[key]
	if m != nil && n.expired(m, now) {
		n.remove(m)
		m = nil
	}
	created := m == nil
	if created {
		if len(n.forward) >= maxUDPMappings {
			n.expire(now)
			if len(n.forward) >= maxUDPMappings {
				return fmt.Errorf("l3: UDP mapping limit reached")
			}
		}
		port, socket, err := n.reserve(n.egress)
		if err != nil {
			return fmt.Errorf("l3: reserve UDP source port: %w", err)
		}
		wire := key
		wire.srcIP, wire.srcPort = ipU32(n.egress), port
		m = &udpMapping{client: key, wire: wire, port: socket, lastSeen: now}
		n.forward[key], n.reverse[reverseKey(wire)] = m, m
	}
	// Keep the reservation locked through send: expiry/Close must not release
	// the port between rewriting the packet and handing it to the raw socket.
	rewriteSNAT(pkt, n.egress)
	ihl := int(pkt[0]&0x0f) * 4
	binary.BigEndian.PutUint16(pkt[ihl:ihl+2], m.wire.srcPort)
	fixChecksums(pkt)
	if err := send(pkt); err != nil {
		if created {
			n.remove(m)
		}
		return err
	}
	m.lastSeen = time.Now()
	return nil
}

func (n *udpNAT) translateReply(pkt []byte, key flowKey) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return false
	}
	m := n.reverse[key]
	if m == nil {
		return false
	}
	now := time.Now()
	if n.expired(m, now) {
		n.remove(m)
		return false
	}
	m.lastSeen = now
	binary.BigEndian.PutUint32(pkt[16:20], m.client.srcIP)
	ihl := int(pkt[0]&0x0f) * 4
	binary.BigEndian.PutUint16(pkt[ihl+2:ihl+4], m.client.srcPort)
	fixChecksums(pkt)
	return true
}

// The caller holds mu for expiry and removal.
func (n *udpNAT) expired(m *udpMapping, now time.Time) bool {
	return now.Sub(m.lastSeen) > flowTimeout(m.client, &ctEntry{})
}

func (n *udpNAT) expire(now time.Time) {
	for _, m := range n.forward {
		if n.expired(m, now) {
			n.remove(m)
		}
	}
}

func (n *udpNAT) remove(m *udpMapping) {
	delete(n.forward, m.client)
	delete(n.reverse, reverseKey(m.wire))
	_ = m.port.Close()
}

func (n *udpNAT) Close() {
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.stop)
		for _, m := range n.forward {
			n.remove(m)
		}
	}
	n.mu.Unlock()
	<-n.done
}
