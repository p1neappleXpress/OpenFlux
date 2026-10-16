package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/p1neappleXpress/OpenFlux/network"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/tunnel/l3"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// ExitMode выбирает, как выходная нода общается с интернетом.
type ExitMode int

const (
	ExitModeL3 ExitMode = iota
	ExitModeL4
)

func (m ExitMode) String() string {
	switch m {
	case ExitModeL3:
		return "l3"
	default:
		return "l4"
	}
}

func ParseExitMode(s string) (ExitMode, error) {
	switch s {
	case "", "l4", "proxy":
		return ExitModeL4, nil
	case "l3":
		return ExitModeL3, nil
	default:
		return ExitModeL4, fmt.Errorf("unknown mode %q (want l3|l4)", s)
	}
}

type TCPTunnel struct {
	gvisorStack *stack.Stack
	tunnelEP    *TunnelLinkEndpoint
	transport   transport.Transport
	isExitNode  bool
	exitMode    ExitMode
	startTime   time.Time
	packetCount atomic.Uint64
	stopOnce    sync.Once
	cc          string // the congestion control new connections use
	ptbMu       sync.Mutex
	ptbLast     map[[12]byte]time.Time // the last ICMP fragmentation-needed per flow
	stopCh      chan struct{}
	udpFlows    atomic.Int32
	err         error
}

// Err reports why the tunnel could not be set up, or nil.
func (t *TCPTunnel) Err() error { return t.err }

var (
	TCPBufMin     = 4 * 1024 * 1024
	TCPBufDefault = 16 * 1024 * 1024
	TCPBufMax     = 64 * 1024 * 1024
)

// tcpMinRTO is the floor of the tunnel TCP's retransmission timeout. The
// carriers deliver everything, but not always on time - a relay that holds
// packets back for a second or three, a striped link waiting for its lanes to
// come back - and a timeout that fires while the data is merely late costs the
// connection its whole window: gVisor does not undo a spurious one, and after
// two or three of them a connection crawls at a few packets per round trip.
// Real losses (a queue drop) are repaired by SACK and tail loss probes long
// before this. OPENFLUX_TCP_MIN_RTO_MS.
var tcpMinRTO = func() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("OPENFLUX_TCP_MIN_RTO_MS")); err == nil && n > 0 {
		return time.Duration(n) * time.Millisecond
	}
	return 3 * time.Second
}()

// TuneTCP sets the tunnel TCP's buffers, retransmission timeout floor and loss
// detection.
func TuneTCP(s *stack.Stack) {
	minRTO := tcpip.TCPMinRTOOption(tcpMinRTO)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &minRTO); err != nil {
		utils.Debugf("[TUNNEL] set min RTO: %v", err)
	}
	// Loss detection by duplicate ACKs, not by time (RACK) and no tail loss
	// probes. A carrier hands packets over in order, only sometimes late, so
	// a real loss always shows up as duplicate ACKs - while RACK and TLP take
	// lateness for loss: at the start of every connection the handshake's
	// quick round trip set their clocks, the first data took a few times
	// longer, and the spurious "recovery" left CUBIC at a window of 4 for the
	// next ten seconds. OPENFLUX_TCP_RACK=1 restores gVisor's default.
	if os.Getenv("OPENFLUX_TCP_RACK") != "1" {
		recovery := tcpip.TCPRecovery(0)
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &recovery); err != nil {
			utils.Debugf("[TUNNEL] set loss recovery: %v", err)
		}
	}
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: TCPBufMin, Default: TCPBufDefault, Max: TCPBufMax}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv); err != nil {
		utils.Debugf("[TUNNEL] set recv buffer: %v", err)
	}
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: TCPBufMin, Default: TCPBufDefault, Max: TCPBufMax}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd); err != nil {
		utils.Debugf("[TUNNEL] set send buffer: %v", err)
	}
}

func NewTCPTunnel(trans transport.Transport, isExitNode bool) *TCPTunnel {
	return NewTCPTunnelMode(trans, isExitNode, ExitModeL4)
}

func NewTCPTunnelMode(trans transport.Transport, isExitNode bool, mode ExitMode) *TCPTunnel {
	t := &TCPTunnel{
		transport:  trans,
		isExitNode: isExitNode,
		exitMode:   mode,
		startTime:  time.Now(),
		stopCh:     make(chan struct{}),
	}

	utils.Debugf("[TUNNEL] Net stack init...")
	t.gvisorStack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcpProtocol(), udp.NewProtocol},
	})

	TuneTCP(t.gvisorStack)

	// Detect loss only by duplicate ACKs and the RTO, without RACK-TLP. The
	// document relay delivers every message in order but sometimes holds them
	// for hundreds of milliseconds; RACK-TLP takes each hold for a loss and
	// halves the window, and gVisor never undoes that, which kept uploads
	// through the tunnel at ~100 KB/s.
	recovery := tcpip.TCPRecovery(0)
	if err := t.gvisorStack.SetTransportProtocolOption(tcp.ProtocolNumber, &recovery); err != nil {
		utils.Debugf("[TUNNEL] disable RACK-TLP: %v", err)
	}

	t.cc = wantedCC(trans)
	setCC(t.gvisorStack, t.cc)
	tunnelEP := NewTunnelLinkEndpoint()
	tunnelEP.SetMTU(uint32(tunnelMTU(trans)))
	// "->" points towards the internet and "<-" back towards the device on
	// both sides, as in the L3 and utun logs, so one flow reads the same in
	// the client's and the exit's log.
	toPeer, fromPeer := network.DirOutbound, network.DirInbound
	if isExitNode {
		toPeer, fromPeer = network.DirInbound, network.DirOutbound
	}
	tunnelEP.onOutgoingPacket = func(data []byte) {
		network.LogPacket("TUNNEL", toPeer, data)
		if err := trans.Send(data); err != nil {
			utils.Debugf("[TUNNEL] trans.Send error: %v", err)
			t.refused(err, data)
		}
	}
	if fs, ok := trans.(transport.FillSender); ok {
		tunnelEP.onOutgoingFill = func(size int, fill func(dst []byte)) {
			if utils.Level() >= utils.LevelPackets {
				// Packet logging wants the bytes: take the plain path.
				buf := make([]byte, size)
				fill(buf)
				tunnelEP.onOutgoingPacket(buf)
				return
			}
			var written []byte
			if err := fs.SendFill(size, func(dst []byte) { fill(dst); written = dst }); err != nil {
				utils.Debugf("[TUNNEL] trans.SendFill error: %v", err)
				t.refused(err, written)
			}
		}
	}
	t.tunnelEP = tunnelEP

	tunnelNIC := tcpip.NICID(1)
	if err := t.gvisorStack.CreateNIC(tunnelNIC, tunnelEP); err != nil {
		// Without the NIC nothing is forwarded; Err reports it so an exit
		// does not stay up looking healthy.
		t.err = fmt.Errorf("tunnel: create NIC: %v", err)
		utils.Infof("[TUNNEL] %v", t.err)
		return t
	}

	if isExitNode {
		t.setupExitNodeProxy(tunnelNIC)
	} else {
		t.setupClient(tunnelNIC)
	}

	trans.Receive(func(data []byte) {
		network.LogPacket("TUNNEL", fromPeer, data)
		tunnelEP.InjectInbound(data)
	})

	utils.SafeGo("tunnel.printStats", t.printStats)
	utils.SafeGo("tunnel.watchMTU", func() { t.watchMTU(trans) })
	return t
}

func (t *TCPTunnel) setupExitNodeProxy(tunnelNIC tcpip.NICID) {
	utils.Debugf("[TUNNEL] EXIT NODE - proxy mode (no raw sockets)")

	t.gvisorStack.SetPromiscuousMode(tunnelNIC, true)
	t.gvisorStack.SetSpoofing(tunnelNIC, true)
	t.gvisorStack.AddRoute(tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		NIC:         tunnelNIC,
	})

	fwd := tcp.NewForwarder(t.gvisorStack, 0, 8192, t.handleExitTCP)
	t.gvisorStack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	udpFwd := udp.NewForwarder(t.gvisorStack, t.handleExitUDP)
	t.gvisorStack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
}

const udpIdleTimeout = 2 * time.Minute

func (t *TCPTunnel) handleExitUDP(r *udp.ForwarderRequest) bool {
	if t.udpFlows.Add(1) > 256 {
		t.udpFlows.Add(-1)
		return true
	}
	id := r.ID()
	dest := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort))

	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		t.udpFlows.Add(-1)
		utils.Debugf("[EXIT] UDP CreateEndpoint %s: %v", dest, tErr)
		return true
	}
	local := gonet.NewUDPConn(&wq, ep)
	noteUDPBypassesUpstream()
	remote, err := net.DialTimeout("udp", dest, 10*time.Second)
	if err != nil {
		utils.Debugf("[EXIT] UDP dial %s failed: %v", dest, err)
		local.Close()
		t.udpFlows.Add(-1)
		return true
	}

	utils.SafeGo("exit.udp-flow", func() {
		defer t.udpFlows.Add(-1)
		refresh := func() {
			deadline := time.Now().Add(udpIdleTimeout)
			_ = local.SetReadDeadline(deadline)
			_ = remote.SetReadDeadline(deadline)
		}
		refresh()
		var once sync.Once
		closeBoth := func() {
			_ = local.Close()
			_ = remote.Close()
		}
		pump := func(dst, src net.Conn) {
			defer once.Do(closeBoth)
			buf := make([]byte, 65535)
			for {
				n, err := src.Read(buf)
				if err != nil {
					return
				}
				refresh()
				_ = dst.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
		}
		done := make(chan struct{})
		go func() { defer close(done); pump(remote, local) }()
		pump(local, remote)
		<-done
	})
	return true
}

func (t *TCPTunnel) handleExitTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dest := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))

	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		utils.Debugf("[EXIT] CreateEndpoint %s: %v", dest, tErr)
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(&wq, ep)

	utils.SafeGo("exit.flow", func() {
		remote, err := dialExitTCP(dest)
		if err != nil {
			utils.Debugf("[EXIT] dial %s failed: %v", dest, err)
			local.Close()
			return
		}
		if tc, ok := remote.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			// No SO_RCVBUF / SO_SNDBUF: setting one turns the kernel's
			// buffer autotuning off for the socket and is cut to
			// net.core.[rw]mem_max - 208 KB on a stock Linux. A Telemost exit
			// that asked for 16 MB got 416 KB and fetched from the internet
			// at 1.4 MB/s over a 50 ms path (42 MB/s left alone), which capped
			// every download through it.
		}
		utils.Debugf("[EXIT] %s connected", dest)

		go func() {
			buf := make([]byte, 256*1024)
			io.CopyBuffer(remote, local, buf)
			remote.Close()
			local.Close()
		}()
		buf := make([]byte, 256*1024)
		io.CopyBuffer(local, remote, buf)
		local.Close()
		remote.Close()
	})
}

func (t *TCPTunnel) setupClient(tunnelNIC tcpip.NICID) {
	clientAddr := tcpip.AddrFrom4([4]byte{10, 10, 10, 2})
	t.gvisorStack.AddProtocolAddress(tunnelNIC, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   clientAddr,
			PrefixLen: 24,
		},
	}, stack.AddressProperties{})

	t.gvisorStack.AddRoute(tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		NIC:         tunnelNIC,
	})
}

var dialTimeout = 10 * time.Second

func (t *TCPTunnel) DialTCP(address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("bad port in %q", address)
	}

	ip, err := t.resolveIPv4(host)
	if err != nil {
		return nil, err
	}
	utils.Debugf("[TUNNEL] DialTCP %s -> %s:%d", address, ip.String(), port)

	nic := tcpip.NICID(1)
	if t.isExitNode && false {
		nic = tcpip.NICID(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, t.gvisorStack, tcpip.FullAddress{
		NIC:  nic,
		Addr: tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		Port: uint16(port),
	}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// resolveIPv4 resolves host to an IPv4 address using the local system
// resolver (for a literal IP this is just a parse, no lookup).
func (t *TCPTunnel) resolveIPv4(host string) (net.IP, error) {
	if literal := net.ParseIP(host); literal != nil {
		if ip4 := literal.To4(); ip4 != nil {
			return ip4, nil
		}
		return nil, fmt.Errorf("IPv6 not supported")
	}

	tcpAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	ip4 := tcpAddr.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("IPv6 not supported")
	}
	return ip4, nil
}

func (t *TCPTunnel) DialUDP(address string) (net.Conn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP: %w", err)
	}
	ip := udpAddr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("IPv6 not supported")
	}
	remote := &tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		Port: uint16(udpAddr.Port),
	}
	return gonet.DialUDP(t.gvisorStack, nil, remote, ipv4.ProtocolNumber)
}

func (t *TCPTunnel) ListenTCP(port uint16) (net.Listener, error) {
	return gonet.ListenTCP(t.gvisorStack, tcpip.FullAddress{
		NIC:  1,
		Port: port,
	}, ipv4.ProtocolNumber)
}

func (t *TCPTunnel) Close() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
		t.gvisorStack.Close()
	})
}

func (t *TCPTunnel) printStats() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var lastRetrans, lastQueueWaits, lastSendRetries uint64

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			stats := t.gvisorStack.Stats()
			carrier := t.transport.Stats()
			retrans := stats.TCP.Retransmits.Value()
			message := fmt.Sprintf("[STATS] uptime=%v mode=%s packets=%d connected=%d established=%d retrans=%d queue_waits=%d send_retries=%d",
				time.Since(t.startTime).Round(time.Second),
				t.exitMode.String(),
				t.packetCount.Load(),
				stats.TCP.CurrentConnected.Value(),
				stats.TCP.CurrentEstablished.Value(),
				retrans, carrier.QueueWaits, carrier.SendRetries,
			)
			if utils.DebugEnabled() {
				tc := stats.TCP
				utils.Debugf("[STATS] tcp: sent=%d recv=%d invalid=%d retrans=%d fastRetrans=%d fastRecovery=%d sackRecovery=%d tlp=%d timeouts=%d slowStartRetrans=%d resets=%d/%d sendErrors=%d | drops=%d nic-rx-drops=%d",
					tc.SegmentsSent.Value(), tc.ValidSegmentsReceived.Value(), tc.InvalidSegmentsReceived.Value(),
					tc.Retransmits.Value(), tc.FastRetransmit.Value(), tc.FastRecovery.Value(), tc.SACKRecovery.Value(),
					tc.TLPRecovery.Value(), tc.Timeouts.Value(), tc.SlowStartRetransmits.Value(),
					tc.ResetsSent.Value(), tc.ResetsReceived.Value(), tc.SegmentSendErrors.Value(),
					stats.DroppedPackets.Value(), stats.NICs.Rx.Packets.Value())
			}
			if retrans > lastRetrans || carrier.QueueWaits > lastQueueWaits || carrier.SendRetries > lastSendRetries {
				utils.Infof("%s", message)
			} else {
				utils.Debugf("%s", message)
			}
			lastRetrans, lastQueueWaits, lastSendRetries = retrans, carrier.QueueWaits, carrier.SendRetries
			t.logBusiestFlow()
		}
	}
}

// logBusiestFlow logs the congestion state of the connection with the
// largest window - the one a download runs on.
func (t *TCPTunnel) logBusiestFlow() {
	var best tcpip.TCPInfoOption
	for _, te := range t.gvisorStack.RegisteredEndpoints() {
		ep, ok := te.(tcpip.Endpoint)
		if !ok {
			continue
		}
		var info tcpip.TCPInfoOption
		if ep.GetSockOpt(&info) == nil && info.SndCwnd > best.SndCwnd {
			best = info
		}
	}
	if best.SndCwnd > 0 {
		utils.Packetf("[STATS] busiest flow: cwnd=%d ssthresh=%d rtt=%s rttvar=%s rto=%s",
			best.SndCwnd, best.SndSsthresh, best.RTT.Round(time.Millisecond),
			best.RTTVar.Round(time.Millisecond), best.RTO.Round(time.Millisecond))
	}
}

func SetLocalIP(ip string) error { return l3.SetLocalIP(ip) }

// tunnelMTU is the MTU of the tunnel's interface: what the carriers can move
// (transport.CarrierMTUer; 1500 when they say nothing) within the packet limit
// the peer accepts. Every layer between the stack and the wire works per
// packet, so a carrier that moves large packets as easily as small ones makes
// the whole pipeline several times cheaper per byte. Each side advertises its
// own MSS, so a peer that keeps 1500 simply keeps getting 1460-byte segments.
// OPENFLUX_TUNNEL_MTU forces a value.
func tunnelMTU(trans transport.Transport) int {
	if n, err := strconv.Atoi(os.Getenv("OPENFLUX_TUNNEL_MTU")); err == nil && n >= 1280 && n <= 65000 {
		return n
	}
	mtu := transport.DefaultCarrierMTU
	if c, ok := trans.(transport.CarrierMTUer); ok {
		mtu = c.CarrierMTU()
	}
	if n, ok := trans.(transport.PeerParameterProvider); ok {
		if p, ready := n.PeerParameters(); ready {
			mtu = min(mtu, p.MaxPacketSize)
		}
	}
	return max(min(mtu, 65000), 1280)
}

// watchMTU follows the carrier set: a carrier added later that cannot take
// large packets lowers the MTU for the connections opened from then on, and
// the peer's limit becomes known once the handshake is done.
func (t *TCPTunnel) watchMTU(trans transport.Transport) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-tick.C:
			if want := uint32(tunnelMTU(trans)); want != t.tunnelEP.MTU() {
				utils.Infof("[TUNNEL] MTU %d -> %d", t.tunnelEP.MTU(), want)
				t.tunnelEP.SetMTU(want)
			}
			if want := wantedCC(trans); want != t.cc {
				utils.Infof("[TUNNEL] TCP congestion control %s -> %s for new connections", t.cc, want)
				t.cc = want
				setCC(t.gvisorStack, want)
			}
		}
	}
}

// refused answers a packet the transport would not take because no live carrier
// moves one that large (transport.PacketTooLargeError): the stack gets an ICMP
// "fragmentation needed" for it, as from a router with a smaller MTU, and the
// connection retransmits in smaller segments. orig is the packet as the stack
// wrote it; it is only read here.
func (t *TCPTunnel) refused(err error, orig []byte) {
	var big *transport.PacketTooLargeError
	if !errors.As(err, &big) || len(orig) < header.IPv4MinimumSize {
		return
	}
	icmp := buildFragNeeded(orig, big.Limit)
	if icmp == nil {
		return
	}
	if !t.ptbDue(orig) {
		return
	}
	utils.Debugf("[TUNNEL] packet of %d bytes does not fit the live carriers (%d): telling the stack", len(orig), big.Limit)
	// Not on this goroutine: the stack holds the connection's lock while it
	// writes the packet, and the ICMP is delivered to that connection.
	go t.tunnelEP.InjectInbound(icmp)
}

// buildFragNeeded builds the ICMPv4 destination-unreachable / fragmentation
// needed that a router with the given MTU would answer orig with: from the
// packet's destination to its source, quoting its header and first 8 bytes.
func buildFragNeeded(orig []byte, mtu int) []byte {
	ip := header.IPv4(orig)
	ihl := int(ip.HeaderLength())
	if orig[0]>>4 != 4 || ihl < header.IPv4MinimumSize || len(orig) < ihl {
		return nil
	}
	quote := orig[:min(len(orig), ihl+8)]
	out := make([]byte, header.IPv4MinimumSize+header.ICMPv4MinimumSize+len(quote))
	h := header.IPv4(out)
	h.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(out)),
		TTL:         64,
		Protocol:    uint8(header.ICMPv4ProtocolNumber),
		SrcAddr:     ip.DestinationAddress(),
		DstAddr:     ip.SourceAddress(),
	})
	h.SetChecksum(^h.CalculateChecksum())
	icmp := header.ICMPv4(h.Payload())
	icmp.SetType(header.ICMPv4DstUnreachable)
	icmp.SetCode(header.ICMPv4FragmentationNeeded)
	icmp.SetMTU(uint16(min(mtu, 65535)))
	copy(icmp.Payload(), quote)
	icmp.SetChecksum(0)
	icmp.SetChecksum(header.ICMPv4Checksum(icmp, 0))
	return out
}

// ptbDue limits the answers to a flow: its first refused packet is answered at
// once, the rest of that window (hundreds of segments) only every 20 ms, which
// the stack needs once to lower its segment size.
func (t *TCPTunnel) ptbDue(orig []byte) bool {
	var key [12]byte
	copy(key[:8], orig[12:20])
	if ihl := int(orig[0]&15) * 4; ihl >= 20 && len(orig) >= ihl+4 {
		copy(key[8:], orig[ihl:ihl+4])
	}
	now := time.Now()
	t.ptbMu.Lock()
	defer t.ptbMu.Unlock()
	if t.ptbLast == nil || len(t.ptbLast) > 1024 {
		t.ptbLast = make(map[[12]byte]time.Time)
	}
	if now.Sub(t.ptbLast[key]) < 20*time.Millisecond {
		return false
	}
	t.ptbLast[key] = now
	return true
}
