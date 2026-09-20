package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"openflux/transport"
	"openflux/utils"
)

// ExitMode выбирает, как выходная нода общается с интернетом.
type ExitMode int

const (
	ExitModeL3 ExitMode = iota // L3: SNAT/DNAT без gVisor (Linux)
	ExitModeL4                 // L4: gVisor TCP-терминация + net.Dial (работает везде)
)

func (m ExitMode) String() string {
	switch m {
	case ExitModeL3:
		return "l3"
	default:
		return "l4"
	}
}

// ParseExitMode разбирает строку из флага --mode.
func ParseExitMode(s string) (ExitMode, error) {
	switch s {
	case "", "l4", "proxy":
		// "proxy" is a deprecated alias kept for one release.
		return ExitModeL4, nil
	case "l3":
		return ExitModeL3, nil
	default:
		return ExitModeL4, fmt.Errorf("unknown mode %q (want l3|l4)", s)
	}
}

func parseSOCKS5(proxyStr string) (string, *proxy.Auth, error) {
	if !strings.Contains(proxyStr, "://") {
		proxyStr = "socks5://" + proxyStr
	}
	u, err := url.Parse(proxyStr)
	if err != nil {
		return "", nil, err
	}
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{
			User: u.User.Username(),
		}
		if pass, ok := u.User.Password(); ok {
			auth.Password = pass
		}
	}
	host := u.Host
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return host, auth, nil
}

type TCPTunnel struct {
	gvisorStack   *stack.Stack
	tunnelEP      *TunnelLinkEndpoint
	transport     transport.Transport
	isExitNode    bool
	exitMode      ExitMode
	upstreamProxy string
	dialer        proxy.Dialer
	startTime     time.Time
	packetCount   atomic.Uint64
}

// TCP buffer size range for gvisor stacks.
var (
	TCPBufMin     = 4 * 1024 * 1024
	TCPBufDefault = 16 * 1024 * 1024
	TCPBufMax     = 64 * 1024 * 1024
)

// SetTCPBuffers applies the configured TCP send/receive buffer ranges to s.
func SetTCPBuffers(s *stack.Stack) {
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: TCPBufMin, Default: TCPBufDefault, Max: TCPBufMax}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv); err != nil {
		utils.Debugf("[TUNNEL] set recv buffer: %v", err)
	}
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: TCPBufMin, Default: TCPBufDefault, Max: TCPBufMax}
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd); err != nil {
		utils.Debugf("[TUNNEL] set send buffer: %v", err)
	}
}

func NewTCPTunnel(trans transport.Transport, isExitNode bool, upstreamProxy ...string) *TCPTunnel {
	proxy := ""
	if len(upstreamProxy) > 0 {
		proxy = upstreamProxy[0]
	}
	return NewTCPTunnelModeWithProxy(trans, isExitNode, ExitModeL4, proxy)
}

func NewTCPTunnelMode(trans transport.Transport, isExitNode bool, mode ExitMode) *TCPTunnel {
	return NewTCPTunnelModeWithProxy(trans, isExitNode, mode, "")
}

func NewTCPTunnelModeWithProxy(trans transport.Transport, isExitNode bool, mode ExitMode, upstreamProxy string) *TCPTunnel {
	t := &TCPTunnel{
		transport:     trans,
		isExitNode:    isExitNode,
		exitMode:      mode,
		upstreamProxy: upstreamProxy,
		startTime:     time.Now(),
	}

	utils.Debugf("[TUNNEL] Net stack init...")
	t.gvisorStack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	SetTCPBuffers(t.gvisorStack)

	tunnelEP := NewTunnelLinkEndpoint()
	tunnelEP.onOutgoingPacket = func(data []byte) {
		if err := trans.Send(data); err != nil {
			utils.Debugf("[TUNNEL] trans.Send error: %v", err)
		}
	}
	t.tunnelEP = tunnelEP

	tunnelNIC := tcpip.NICID(1)
	if err := t.gvisorStack.CreateNIC(tunnelNIC, tunnelEP); err != nil {
		utils.Debugf("[TUNNEL] CreateNIC tunnel error: %v", err)
	}

	if isExitNode {
		t.setupExitNodeProxy(tunnelNIC)
	} else {
		t.setupClient(tunnelNIC)
	}

	trans.Receive(func(data []byte) {
		tunnelEP.InjectInbound(data)
	})

	utils.SafeGo("tunnel.printStats", t.printStats)
	return t
}

// ---- exit node: proxy ----

func (t *TCPTunnel) setupExitNodeProxy(tunnelNIC tcpip.NICID) {
	baseDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	if t.upstreamProxy == "" || strings.ToLower(t.upstreamProxy) == "direct" {
		t.dialer = baseDialer
		utils.Debugf("[TUNNEL] EXIT NODE - proxy mode (direct connection, no upstream SOCKS5)")
	} else {
		proxyHost, auth, err := parseSOCKS5(t.upstreamProxy)
		if err != nil {
			utils.Debugf("[TUNNEL] Invalid upstream proxy %q: %v", t.upstreamProxy, err)
			t.dialer = baseDialer
		} else {
			d, err := proxy.SOCKS5("tcp", proxyHost, auth, baseDialer)
			if err != nil {
				utils.Debugf("[TUNNEL] Failed to create SOCKS5 dialer for %s: %v", proxyHost, err)
				t.dialer = baseDialer
			} else {
				t.dialer = d
				utils.Debugf("[TUNNEL] EXIT NODE - Routing via SOCKS5 proxy %s", proxyHost)
			}
		}
	}

	t.gvisorStack.SetPromiscuousMode(tunnelNIC, true)
	t.gvisorStack.SetSpoofing(tunnelNIC, true)
	t.gvisorStack.AddRoute(tcpip.Route{
		Destination: header.IPv4EmptySubnet,
		NIC:         tunnelNIC,
	})

	fwd := tcp.NewForwarder(t.gvisorStack, 0, 8192, t.handleExitTCP)
	t.gvisorStack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
}

func (t *TCPTunnel) handleExitTCP(r *tcp.ForwarderRequest) {
	reqID := r.ID()
	dest := net.JoinHostPort(reqID.LocalAddress.String(), strconv.Itoa(int(reqID.LocalPort)))

	utils.SafeGo("exit.flow", func() {
		dialer := t.dialer
		if dialer == nil {
			dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var remote net.Conn
		var err error
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			remote, err = cd.DialContext(ctx, "tcp", dest)
		} else {
			remote, err = dialer.Dial("tcp", dest)
		}

		if err != nil {
			utils.Debugf("[EXIT] dial %s failed: %v", dest, err)
			r.Complete(true)
			return
		}
		defer remote.Close()

		var wq waiter.Queue
		ep, tErr := r.CreateEndpoint(&wq)
		if tErr != nil {
			utils.Debugf("[EXIT] CreateEndpoint %s: %v", dest, tErr)
			r.Complete(true)
			return
		}
		r.Complete(false)

		local := gonet.NewTCPConn(&wq, ep)
		defer local.Close()

		if tc, ok := remote.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetReadBuffer(16 * 1024 * 1024)
			_ = tc.SetWriteBuffer(16 * 1024 * 1024)
		}
		utils.Debugf("[EXIT] %s connected", dest)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			buf := make([]byte, 256*1024)
			_, _ = io.CopyBuffer(remote, local, buf)
			if tc, ok := remote.(interface{ CloseWrite() error }); ok {
				_ = tc.CloseWrite()
			} else {
				remote.Close()
			}
		}()

		go func() {
			defer wg.Done()
			defer local.Close()
			buf := make([]byte, 256*1024)
			_, _ = io.CopyBuffer(local, remote, buf)
		}()

		wg.Wait()
	})
}

// ---- client ----

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

func (t *TCPTunnel) DialTCP(address string) (net.Conn, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}

	ip := tcpAddr.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("IPv6 not supported")
	}
	utils.Debugf("[TUNNEL] DialTCP %s -> %s:%d", address, ip.String(), tcpAddr.Port)

	nic := tcpip.NICID(1)
	if t.isExitNode && false {
		nic = tcpip.NICID(2)
	}

	conn, err := gonet.DialTCP(t.gvisorStack, tcpip.FullAddress{
		NIC:  nic,
		Addr: tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], ip[3]}),
		Port: uint16(tcpAddr.Port),
	}, ipv4.ProtocolNumber)

	return conn, err
}

func (t *TCPTunnel) ListenTCP(port uint16) (net.Listener, error) {
	return gonet.ListenTCP(t.gvisorStack, tcpip.FullAddress{
		NIC:  1,
		Port: port,
	}, ipv4.ProtocolNumber)
}

func (t *TCPTunnel) printStats() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		stats := t.gvisorStack.Stats()
		utils.Debugf("[STATS] uptime=%v mode=%s packets=%d connected=%d established=%d retrans=%d",
			time.Since(t.startTime).Round(time.Second),
			t.exitMode.String(),
			t.packetCount.Load(),
			stats.TCP.CurrentConnected.Value(),
			stats.TCP.CurrentEstablished.Value(),
			stats.TCP.Retransmits.Value(),
		)
	}
}

// ---- local IP helpers (only needed for raw mode) ----

// localIPOverride, when set, is the address the exit node uses as its egress
// IP (both for source rewriting and the return-packet filter).
var localIPOverride string

// SetLocalIP overrides the auto-detected egress IP for the exit node.
func SetLocalIP(ip string) { localIPOverride = ip }

func getLocalIP() string {
	if localIPOverride != "" {
		return localIPOverride
	}
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "192.168.1.100"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
