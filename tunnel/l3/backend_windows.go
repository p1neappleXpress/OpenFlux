//go:build windows

package l3

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xjasonlyu/windivert-go"
	"golang.org/x/sys/windows"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Windows raw sockets cannot send TCP, so this backend uses WinDivert
// (WinDivert.dll and WinDivert64.sys next to the core; Administrator):
//
//   - in, a sniffing handle, copies inbound packets addressed to the egress
//     IP, like the raw receive sockets on Linux; the host's traffic is left
//     alone.
//   - out, a diverting handle, takes only the host's outbound TCP RSTs from
//     the egress IP. The Windows stack answers tunnel flows it never opened
//     with RST (unless its firewall drops them first); those RSTs are
//     dropped and all others reinjected: the per-flow version of the
//     iptables rule L3 needs on Linux. The backend's own packets go out
//     through this handle, and WinDivert never diverts a packet back to the
//     handle that injected it.
type divertBackend struct {
	in, out windivert.Handle
	egress  [4]byte
	mtu     int
	sent    *sentFlows

	resetsDropped atomic.Uint64

	// Send and the receive loops hold ioMu for reading around every handle
	// call, so Close never closes a handle another goroutine is using.
	ioMu      sync.RWMutex
	closeOnce sync.Once
	closed    chan struct{}
}

const (
	divertPriority = 1000
	// Longer than conntrack keeps an idle established flow, so a flow L3
	// still forwards is never forgotten here first.
	sentFlowTTL = ctTimeoutEstablished + time.Minute
)

func newBackend() (L3Backend, error) {
	// windivert-go loads the DLL with MustLoadDLL; load it here first so a
	// missing DLL is an error instead of a panic.
	if _, err := windows.LoadDLL("WinDivert.dll"); err != nil {
		return nil, fmt.Errorf("l3: WinDivert.dll: %w (it and WinDivert64.sys must be next to the core)", err)
	}
	egress, err := egressIPv4()
	if err != nil {
		return nil, err
	}
	ip := net.IP(egress[:]).String()

	in, err := windivert.Open(
		fmt.Sprintf("inbound and ip and ip.DstAddr == %s and (tcp or udp or icmp)", ip),
		windivert.LayerNetwork, divertPriority, windivert.FlagSniff|windivert.FlagRecvOnly)
	if err != nil {
		return nil, fmt.Errorf("l3: WinDivert: %w", err)
	}
	// Sniffed copies that do not fit the queue are lost to L3 only; a deeper
	// queue rides out bursts while the transport is slow.
	_ = in.SetParam(windivert.QueueLength, 8192)
	_ = in.SetParam(windivert.QueueSize, 16<<20)

	out, err := windivert.Open(
		fmt.Sprintf("outbound and ip and ip.SrcAddr == %s and tcp and tcp.Rst", ip),
		windivert.LayerNetwork, divertPriority, windivert.FlagDefault)
	if err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("l3: WinDivert: %w", err)
	}

	b := &divertBackend{
		in:     in,
		out:    out,
		egress: egress,
		mtu:    interfaceMTU(egress),
		sent:   newSentFlows(sentFlowTTL),
		closed: make(chan struct{}),
	}
	go b.dropHostResets()
	go b.expireLoop()
	utils.Debugf("[L3/windows] WinDivert backend ready, egress=%s mtu=%d", ip, b.mtu)
	return b, nil
}

func (b *divertBackend) EgressIP() [4]byte { return b.egress }

func (b *divertBackend) Send(pkt []byte) error {
	pkt, ok := sliceIPv4(pkt)
	if !ok {
		return fmt.Errorf("l3: invalid IPv4 packet")
	}
	// WinDivert injects what it is given; an oversized packet would just
	// vanish on the wire, so report it the way Linux's EMSGSIZE is.
	if len(pkt) > b.mtu {
		return &PacketTooBigError{MTU: b.mtu}
	}
	if f, ok := outboundTCPFlow(pkt); ok {
		b.sent.record(f, time.Now())
	}

	var addr windivert.Address
	addr.SetLayer(windivert.LayerNetwork)
	addr.SetOutbound()
	// L3 computes every checksum itself; mark them valid so they are not
	// left to checksum offload.
	addr.SetIPChecksum()
	addr.SetTCPChecksum()
	addr.SetUDPChecksum()
	return b.inject(pkt, &addr)
}

// inject sends through the out handle, never after Close released it.
func (b *divertBackend) inject(pkt []byte, addr *windivert.Address) error {
	b.ioMu.RLock()
	defer b.ioMu.RUnlock()
	select {
	case <-b.closed:
		return net.ErrClosed
	default:
	}
	_, err := b.out.Send(pkt, addr)
	return err
}

func (b *divertBackend) Recv(cb func([]byte)) {
	go func() {
		buf := make([]byte, 65535)
		var addr windivert.Address
		for {
			n, ok := b.recv(b.in, buf, &addr)
			if !ok {
				return
			}
			if n == 0 {
				continue
			}
			cp := make([]byte, n)
			copy(cp, buf[:n])
			cb(cp)
		}
	}()
}

// dropHostResets drops the host's RSTs for flows this backend sent and
// reinjects every other RST unchanged.
func (b *divertBackend) dropHostResets() {
	buf := make([]byte, 65535)
	var addr windivert.Address
	for {
		n, ok := b.recv(b.out, buf, &addr)
		if !ok {
			return
		}
		if n == 0 {
			continue
		}
		if f, ok := outboundTCPFlow(buf[:n]); ok && b.sent.contains(f, time.Now()) {
			if b.resetsDropped.Add(1)%1000 == 1 {
				utils.Debugf("[L3/windows] dropped host RST for tunnel flow :%d -> %s:%d (%d so far)",
					f.srcPort, ipStr(f.dstIP), f.dstPort, b.resetsDropped.Load())
			}
			continue
		}
		if err := b.inject(buf[:n], &addr); err != nil && err != net.ErrClosed {
			utils.Debugf("[L3/windows] reinject host RST: %v", err)
		}
	}
}

// recv reads one packet from h; ok is false once the backend is closed, and
// n is 0 after a transient error.
func (b *divertBackend) recv(h windivert.Handle, buf []byte, addr *windivert.Address) (n int, ok bool) {
	b.ioMu.RLock()
	select {
	case <-b.closed:
		b.ioMu.RUnlock()
		return 0, false
	default:
	}
	n, err := h.Recv(buf, addr)
	b.ioMu.RUnlock()
	if err == nil {
		return n, true
	}
	select {
	case <-b.closed:
		return 0, false
	default:
	}
	utils.Debugf("[L3/windows] recv: %v", err)
	time.Sleep(10 * time.Millisecond)
	return 0, true
}

func (b *divertBackend) expireLoop() {
	tick := time.NewTicker(ctSweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-b.closed:
			return
		case now := <-tick.C:
			b.sent.expire(now)
		}
	}
}

func (b *divertBackend) Close() error {
	b.closeOnce.Do(func() {
		close(b.closed)
		// Shutdown wakes the blocked Recv calls, which then release ioMu.
		_ = b.in.Shutdown(windivert.ShutdownBoth)
		_ = b.out.Shutdown(windivert.ShutdownBoth)
		b.ioMu.Lock()
		defer b.ioMu.Unlock()
		_ = b.in.Close()
		_ = b.out.Close()
	})
	return nil
}

// interfaceMTU is the MTU of the interface that owns ip, 1500 when unknown.
func interfaceMTU(ip [4]byte) int {
	ifaces, err := net.Interfaces()
	if err != nil {
		return 1500
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || [4]byte(n.IP.To4()) != ip {
				continue
			}
			if iface.MTU >= 576 {
				return iface.MTU
			}
			return 1500
		}
	}
	return 1500
}
