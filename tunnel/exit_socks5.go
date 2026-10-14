package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// An L4 exit can send what it fetches through an upstream SOCKS5 proxy
// instead of straight to the internet (--exit-socks5, ExitSocks5 in a .conf).
// The proxy is usually a local router - xray, sing-box, mihomo - listening on
// 127.0.0.1 that decides per destination whether to leave from this server or
// pass the connection on to another one. TCP goes through CONNECT, UDP through
// UDP ASSOCIATE (RFC 1928); username/password authentication (RFC 1929) is
// used when the address carries credentials.

// exitUpstream is the parsed --exit-socks5 value.
type exitUpstream struct {
	addr     string // host:port of the proxy
	username string
	password string
}

var exitSocks5 atomic.Pointer[exitUpstream]

// exitDialTimeout bounds one outgoing connection of the exit, the proxy's
// handshake included.
const exitDialTimeout = 10 * time.Second

// SetExitSocks5 routes the L4 exit's outgoing TCP and UDP through a SOCKS5
// proxy. spec is "host:port" or "socks5://[user:pass@]host:port"
// ("socks5h://" is accepted as a synonym); an empty spec restores direct
// connections. It has no effect on an L3 exit, which forwards raw packets.
func SetExitSocks5(spec string) error {
	if spec == "" {
		exitSocks5.Store(nil)
		return nil
	}
	up, err := parseExitSocks5(spec)
	if err != nil {
		return err
	}
	exitSocks5.Store(up)
	return nil
}

// ExitSocks5Addr reports the configured upstream proxy (host:port, without
// credentials), or "" when the exit connects directly.
func ExitSocks5Addr() string {
	if up := exitSocks5.Load(); up != nil {
		return up.addr
	}
	return ""
}

func parseExitSocks5(spec string) (*exitUpstream, error) {
	up := &exitUpstream{}
	hostport := spec
	if u, err := url.Parse(spec); err == nil && u.Scheme != "" && u.Host != "" {
		switch u.Scheme {
		case "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("exit-socks5: unsupported scheme %q (want socks5://)", u.Scheme)
		}
		hostport = u.Host
		if u.User != nil {
			up.username = u.User.Username()
			up.password, _ = u.User.Password()
		}
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("exit-socks5: %q: want host:port or socks5://[user:pass@]host:port", spec)
	}
	if host == "" {
		return nil, fmt.Errorf("exit-socks5: %q: missing host", spec)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("exit-socks5: %q: bad port", spec)
	}
	if len(up.username) > 255 || len(up.password) > 255 {
		return nil, fmt.Errorf("exit-socks5: username and password must be at most 255 bytes")
	}
	up.addr = net.JoinHostPort(host, port)
	return up, nil
}

// exitDialTCP opens the exit's outgoing TCP connection to dest (an IP:port),
// directly or through the upstream proxy.
func exitDialTCP(dest string) (net.Conn, error) {
	up := exitSocks5.Load()
	if up == nil {
		return net.DialTimeout("tcp", dest, exitDialTimeout)
	}
	conn, err := up.handshake(cmdConnect, dest)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// exitDialUDP opens the exit's outgoing UDP flow to dest (an IP:port),
// directly or through the upstream proxy's UDP relay.
func exitDialUDP(dest string) (net.Conn, error) {
	up := exitSocks5.Load()
	if up == nil {
		return net.DialTimeout("udp", dest, exitDialTimeout)
	}
	return up.associate(dest)
}

const (
	socksVersion   = 0x05
	cmdConnect     = 0x01
	cmdAssociate   = 0x03
	atypIPv4       = 0x01
	atypDomain     = 0x03
	atypIPv6       = 0x04
	methodNoAuth   = 0x00
	methodUserPass = 0x02
	methodNone     = 0xFF
)

var errSocksReply = errors.New("exit-socks5: proxy refused")

// handshake connects to the proxy, authenticates and sends cmd for target.
// On success the returned conn is ready: for CONNECT it carries the target's
// bytes, for UDP ASSOCIATE it is the control connection and bound is the
// relay's address.
func (up *exitUpstream) handshake(cmd byte, target string) (net.Conn, error) {
	conn, _, err := up.handshakeBound(cmd, target)
	return conn, err
}

func (up *exitUpstream) handshakeBound(cmd byte, target string) (net.Conn, *net.UDPAddr, error) {
	conn, err := net.DialTimeout("tcp", up.addr, exitDialTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("exit-socks5: dial %s: %w", up.addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(exitDialTimeout))
	bound, err := up.negotiate(conn, cmd, target)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	if tc, ok := conn.(*net.TCPConn); ok && cmd == cmdConnect {
		_ = tc.SetNoDelay(true)
	}
	return conn, bound, nil
}

func (up *exitUpstream) negotiate(conn net.Conn, cmd byte, target string) (*net.UDPAddr, error) {
	// Greeting: offer user/pass only when we have credentials.
	greeting := []byte{socksVersion, 1, methodNoAuth}
	if up.username != "" {
		greeting = []byte{socksVersion, 2, methodNoAuth, methodUserPass}
	}
	if _, err := conn.Write(greeting); err != nil {
		return nil, fmt.Errorf("exit-socks5: greeting: %w", err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return nil, fmt.Errorf("exit-socks5: greeting reply: %w", err)
	}
	if sel[0] != socksVersion {
		return nil, fmt.Errorf("exit-socks5: %s is not a SOCKS5 proxy (version %d)", up.addr, sel[0])
	}
	switch sel[1] {
	case methodNoAuth:
	case methodUserPass:
		if up.username == "" {
			return nil, fmt.Errorf("exit-socks5: %s wants a username and password", up.addr)
		}
		req := make([]byte, 0, 3+len(up.username)+len(up.password))
		req = append(req, 0x01, byte(len(up.username)))
		req = append(req, up.username...)
		req = append(req, byte(len(up.password)))
		req = append(req, up.password...)
		if _, err := conn.Write(req); err != nil {
			return nil, fmt.Errorf("exit-socks5: auth: %w", err)
		}
		var res [2]byte
		if _, err := io.ReadFull(conn, res[:]); err != nil {
			return nil, fmt.Errorf("exit-socks5: auth reply: %w", err)
		}
		if res[1] != 0x00 {
			return nil, fmt.Errorf("exit-socks5: %s rejected the username or password", up.addr)
		}
	case methodNone:
		return nil, fmt.Errorf("exit-socks5: %s accepts none of the offered auth methods", up.addr)
	default:
		return nil, fmt.Errorf("exit-socks5: %s chose unknown auth method %d", up.addr, sel[1])
	}

	addr, err := encodeSocksAddr(target)
	if err != nil {
		return nil, err
	}
	req := append([]byte{socksVersion, cmd, 0x00}, addr...)
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("exit-socks5: request: %w", err)
	}
	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, fmt.Errorf("exit-socks5: reply: %w", err)
	}
	host, port, err := readSocksAddr(conn)
	if err != nil {
		return nil, fmt.Errorf("exit-socks5: reply address: %w", err)
	}
	if head[0] != socksVersion {
		return nil, fmt.Errorf("exit-socks5: bad reply version %d", head[0])
	}
	if head[1] != 0x00 {
		return nil, fmt.Errorf("%w %s: %s", errSocksReply, target, socksReplyText(head[1]))
	}
	if cmd != cmdAssociate {
		return nil, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("exit-socks5: relay address %q: %v", host, err)
		}
		ip = ips[0]
	}
	// A relay bound to 0.0.0.0 / :: is reached at the proxy's own address.
	if ip.IsUnspecified() {
		if ph, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			ip = net.ParseIP(ph)
		}
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}

// encodeSocksAddr is ATYP + address + port for host:port.
func encodeSocksAddr(hostport string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("exit-socks5: target %q: %w", hostport, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("exit-socks5: target %q: bad port", hostport)
	}
	var out []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			out = append([]byte{atypIPv4}, v4...)
		} else {
			out = append([]byte{atypIPv6}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("exit-socks5: target %q: host too long", hostport)
		}
		out = append([]byte{atypDomain, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(out, uint16(port)), nil
}

// readSocksAddr reads ATYP + address + port.
func readSocksAddr(r io.Reader) (string, int, error) {
	var atyp [1]byte
	if _, err := io.ReadFull(r, atyp[:]); err != nil {
		return "", 0, err
	}
	var host string
	switch atyp[0] {
	case atypIPv4:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(b[:]).String()
	case atypIPv6:
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(b[:]).String()
	case atypDomain:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", 0, err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", 0, err
		}
		host = string(b)
	default:
		return "", 0, fmt.Errorf("unknown address type %d", atyp[0])
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(p[:])), nil
}

func socksReplyText(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("reply code %d", code)
	}
}

// associate sets up a UDP ASSOCIATE for one flow to dest and returns a
// net.Conn that reads and writes dest's datagrams without the SOCKS header.
func (up *exitUpstream) associate(dest string) (net.Conn, error) {
	header, err := encodeSocksAddr(dest)
	if err != nil {
		return nil, err
	}
	// We send from an address the proxy cannot know yet; 0.0.0.0:0 says so.
	control, relay, err := up.handshakeBound(cmdAssociate, "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	udp, err := net.DialUDP("udp", nil, relay)
	if err != nil {
		control.Close()
		return nil, fmt.Errorf("exit-socks5: UDP relay %s: %w", relay, err)
	}
	c := &socksUDPConn{
		UDPConn: udp,
		control: control,
		header:  append([]byte{0, 0, 0}, header...),
		dest:    dest,
	}
	// The association lives as long as the control connection: when the
	// proxy closes it, the relay is gone, so the flow ends too.
	go func() {
		_, _ = io.Copy(io.Discard, control)
		c.Close()
	}()
	return c, nil
}

// socksUDPConn is one UDP flow through a SOCKS5 relay.
type socksUDPConn struct {
	*net.UDPConn
	control   net.Conn
	header    []byte // RSV RSV FRAG + dest address
	rbuf      []byte // one flow is read by one goroutine
	dest      string
	closeOnce sync.Once
}

func (c *socksUDPConn) Write(p []byte) (int, error) {
	pkt := make([]byte, 0, len(c.header)+len(p))
	pkt = append(pkt, c.header...)
	pkt = append(pkt, p...)
	if _, err := c.UDPConn.Write(pkt); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *socksUDPConn) Read(p []byte) (int, error) {
	if c.rbuf == nil {
		c.rbuf = make([]byte, 65535)
	}
	for {
		n, err := c.UDPConn.Read(c.rbuf)
		if err != nil {
			return 0, err
		}
		payload, ok := stripSocksUDPHeader(c.rbuf[:n])
		if !ok {
			continue // fragmented or malformed: RFC 1928 lets us drop it
		}
		return copy(p, payload), nil
	}
}

func (c *socksUDPConn) RemoteAddr() net.Addr {
	if a, err := net.ResolveUDPAddr("udp", c.dest); err == nil {
		return a
	}
	return c.UDPConn.RemoteAddr()
}

func (c *socksUDPConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.UDPConn.Close()
		_ = c.control.Close()
	})
	return err
}

// stripSocksUDPHeader returns the payload of a relay datagram, or false for
// one that is fragmented or malformed.
func stripSocksUDPHeader(pkt []byte) ([]byte, bool) {
	if len(pkt) < 4 || pkt[2] != 0x00 {
		return nil, false
	}
	off := 4
	switch pkt[3] {
	case atypIPv4:
		off += 4
	case atypIPv6:
		off += 16
	case atypDomain:
		if len(pkt) < 5 {
			return nil, false
		}
		off += 1 + int(pkt[4])
	default:
		return nil, false
	}
	off += 2
	if len(pkt) < off {
		return nil, false
	}
	return pkt[off:], true
}
