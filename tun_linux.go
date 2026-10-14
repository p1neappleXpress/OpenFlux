//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/p1neappleXpress/OpenFlux/network"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

const (
	linuxTunAddr = "10.10.10.2/24"
	linuxTunMTU  = 1280
)

// TUNClient is a Linux /dev/net/tun full-tunnel client: IP packets flow
// straight between the system TUN interface and the transport, like the
// macOS utun and Windows Wintun clients. Requires root and iproute2.
// The core's own sockets stay out of the tunnel through netbind
// (SO_BINDTODEVICE on the default-route interface), so no bypass host
// routes are needed.
type TUNClient struct {
	trans transport.Transport
	fd    *os.File
	name  string
	mtu   int

	inbound chan []byte

	packetsIn  atomic.Uint64
	packetsOut atomic.Uint64
	closed     atomic.Bool

	gateway   string
	iface     string
	routesSet bool
}

func NewTUNClient(trans transport.Transport, mtu int) (*TUNClient, error) {
	if mtu <= 0 {
		mtu = linuxTunMTU
	}
	fd, name, err := openTun()
	if err != nil {
		return nil, err
	}
	return &TUNClient{
		trans:   trans,
		fd:      fd,
		name:    name,
		mtu:     mtu,
		inbound: make(chan []byte, 4096),
	}, nil
}

func (c *TUNClient) Name() string { return c.name }

// Gateway names the default-route gateway the carriers leave through.
func (c *TUNClient) Gateway() string { return c.gateway }

// SaveDefault remembers the current default route (gateway + interface)
// so the carriers can be pinned to it and it can be restored on exit.
func (c *TUNClient) SaveDefault() error {
	gw, iface, err := defaultRoute()
	if err != nil {
		return err
	}
	c.gateway = gw
	c.iface = iface
	utils.Debugf("[TUN] saved default: gw=%s iface=%s", gw, iface)
	return nil
}

// SetupInterface addresses the TUN device and brings it up. Requires root.
func (c *TUNClient) SetupInterface() error {
	if err := ip("link", "set", "dev", c.name, "up", "mtu", strconv.Itoa(c.mtu)); err != nil {
		return err
	}
	if err := ip("addr", "add", linuxTunAddr, "dev", c.name); err != nil {
		return err
	}
	return nil
}

// ConfigureDefault takes the default route: two /1 routes beat 0.0.0.0/0
// without touching it, so the saved default stays for the bound sockets.
func (c *TUNClient) ConfigureDefault() error {
	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := ip("route", "add", prefix, "dev", c.name); err != nil {
			c.removeRoutes()
			return err
		}
	}
	c.routesSet = true
	return nil
}

func (c *TUNClient) removeRoutes() {
	if !c.routesSet {
		return
	}
	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		_ = ip("route", "del", prefix, "dev", c.name)
	}
	c.routesSet = false
}

// RestoreDefault drops the tunnel routes; the real default was never touched.
func (c *TUNClient) RestoreDefault() { c.removeRoutes() }

// Close removes the tunnel routes and deletes the TUN device.
func (c *TUNClient) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.removeRoutes()
	_ = ip("link", "del", "dev", c.name)
	return c.fd.Close()
}

func (c *TUNClient) purgeStaleHostRoutes() {}

// Start runs the two forwarding loops.
func (c *TUNClient) Start() {
	c.trans.Receive(func(pkt []byte) {
		if c.closed.Load() {
			return
		}
		cp := make([]byte, len(pkt))
		copy(cp, pkt)
		select {
		case c.inbound <- cp:
		default:
			utils.Debugf("[TUN] inbound queue full, dropping")
		}
	})
	go c.readFromTun()
	go c.writeToTun()
}

func (c *TUNClient) readFromTun() {
	buf := make([]byte, 65535)
	for !c.closed.Load() {
		n, err := c.fd.Read(buf)
		if err != nil {
			if !c.closed.Load() {
				utils.Debugf("[TUN] read: %v", err)
			}
			return
		}
		if n < 20 || buf[0]>>4 != 4 {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		c.packetsOut.Add(1)
		network.LogPacket("TUN", network.DirOutbound, pkt)
		if err := c.trans.Send(pkt); err != nil {
			utils.Debugf("[TUN] trans.Send FAIL: %v", err)
		}
	}
}

func (c *TUNClient) writeToTun() {
	for pkt := range c.inbound {
		c.packetsIn.Add(1)
		network.LogPacket("TUN", network.DirInbound, pkt)
		if _, err := c.fd.Write(pkt); err != nil {
			utils.Debugf("[TUN] write: %v", err)
			return
		}
	}
}

func openTun() (*os.File, string, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/net/tun: %w (need root and a kernel with TUN)", err)
	}
	var ifr [unix.IFNAMSIZ + 24]byte
	copy(ifr[:], "oflux%d")
	binary.LittleEndian.PutUint16(ifr[unix.IFNAMSIZ:], unix.IFF_TUN|unix.IFF_NO_PI)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr[0])))
	if errno != 0 {
		unix.Close(fd)
		return nil, "", fmt.Errorf("TUNSETIFF: %w", errno)
	}
	name := unix.ByteSliceToString(ifr[:unix.IFNAMSIZ])
	return os.NewFile(uintptr(fd), name), name, nil
}

// defaultRoute parses the kernel default route via iproute2.
func defaultRoute() (string, string, error) {
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return "", "", fmt.Errorf("ip route show default: %w (need iproute2)", err)
	}
	gw, iface, err := parseDefaultRoute(string(out))
	if err != nil {
		return "", "", err
	}
	return gw, iface, nil
}

func parseDefaultRoute(s string) (string, string, error) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		var gw, iface string
		for i := 0; i+1 < len(fields); i++ {
			switch fields[i] {
			case "via":
				gw = fields[i+1]
			case "dev":
				iface = fields[i+1]
			}
		}
		if gw == "" || net.ParseIP(gw).To4() == nil {
			continue
		}
		if iface == "" {
			continue
		}
		return gw, iface, nil
	}
	return "", "", fmt.Errorf("no usable IPv4 default route")
}

func ip(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SocketWatcher fires at once on Linux: the carriers' sockets are bound to
// the real interface (netbind), so the default route can move immediately,
// like on Windows.
type SocketWatcher struct{ onStable func() }

func NewSocketWatcher(gateway string, onStable func()) *SocketWatcher {
	return &SocketWatcher{onStable: onStable}
}

func (w *SocketWatcher) Start(interval time.Duration) { go w.onStable() }
func (w *SocketWatcher) Stop()                        {}
