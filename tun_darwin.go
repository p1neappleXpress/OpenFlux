//go:build darwin

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"openflux/transport"
	"openflux/utils"
)

// TUNClient is a macOS utun-based L3 forwarder: no gVisor, no SOCKS5.
// IP packets flow straight between the system utun interface and the transport.
type TUNClient struct {
	trans transport.Transport
	fd    *os.File
	name  string

	inbound chan []byte

	packetsIn  atomic.Uint64
	packetsOut atomic.Uint64

	gateway     string
	bypassIPs   []string
	routesAdded bool

	// Saved default route, captured before we install any utun routes.
	savedIface string
	savedGw    string
	defaultSet bool
}

func NewTUNClient(trans transport.Transport, mtu int) (*TUNClient, error) {
	fd, name, err := openUtun()
	if err != nil {
		return nil, fmt.Errorf("open utun: %w", err)
	}

	c := &TUNClient{
		trans:   trans,
		fd:      fd,
		name:    name,
		inbound: make(chan []byte, 4096),
	}
	return c, nil
}

func (c *TUNClient) Name() string { return c.name }

// ConfigureInterface sets address + routes. Requires root. Called once at start.
func (c *TUNClient) ConfigureInterface(bypassHosts []string) error {
	// 1. Bring interface up.
	setup := [][]string{
		{"ifconfig", c.name, "10.10.10.2", "10.10.10.2", "up"},
		{"ifconfig", c.name, "mtu", "1280"},
	}
	for _, args := range setup {
		if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w (%s)", args, err, string(out))
		}
	}

	// 2. Find the real default gateway. On macOS, netstat often reports the
	// gateway as "link#N" (interface index) rather than an IP. In that case,
	// derive the router IP from the interface's subnet (first usable host).
	gw, iface, err := realGateway()
	if err != nil {
		return fmt.Errorf("find default gateway: %w", err)
	}
	c.gateway = gw
	utils.Debugf("[TUN] real gateway: %s (iface=%s)", gw, iface)

	// 3. Add /32 bypass routes BEFORE the default route, so transport
	// traffic never loops through the tunnel.
	for _, host := range bypassHosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		ips, err := net.LookupHost(host)
		if err != nil {
			utils.Debugf("[TUN] resolve %s failed: %v", host, err)
			continue
		}
		for _, ip := range ips {
			ipv4 := net.ParseIP(ip).To4()
			if ipv4 == nil {
				continue
			}
			ipStr := ipv4.String()
			args := []string{"route", "add", "-host", ipStr, "-gateway", gw}
			if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
				utils.Debugf("[TUN] bypass route %s (%s) failed: %v (%s)", host, ipStr, err, string(out))
				continue
			}
			c.bypassIPs = append(c.bypassIPs, ipStr)
			utils.Debugf("[TUN] bypass %s -> %s via %s", host, ipStr, gw)
		}
	}

	// 4. Route all remaining traffic through utun.
	defaults := [][]string{
		{"route", "add", "-net", "0.0.0.0/1", "-interface", c.name},
		{"route", "add", "-net", "128.0.0.0/1", "-interface", c.name},
	}
	for _, args := range defaults {
		if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
			// rollback bypass routes on failure
			c.removeRoutes()
			return fmt.Errorf("%v: %w (%s)", args, err, string(out))
		}
	}
	c.routesAdded = true
	return nil
}

// removeRoutes undoes everything ConfigureInterface added. Safe to call
// multiple times. Ignores errors (best-effort cleanup).
func (c *TUNClient) removeRoutes() {
	if !c.routesAdded && len(c.bypassIPs) == 0 {
		return
	}
	// default routes first
	exec.Command("sudo", "route", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("sudo", "route", "delete", "-net", "128.0.0.0/1").Run()
	// bypass /32 routes
	for _, ip := range c.bypassIPs {
		exec.Command("sudo", "route", "delete", "-host", ip).Run()
	}
	c.bypassIPs = nil
	c.routesAdded = false
}

// Start kicks off the two forwarding loops.
func (c *TUNClient) Start() {
	go c.readFromTun()
	go c.writeToTun()

	// Transport -> utun
	c.trans.Receive(func(pkt []byte) {
		cp := make([]byte, len(pkt))
		copy(cp, pkt)
		select {
		case c.inbound <- cp:
		default:
			utils.Debugf("[TUN] inbound queue full, dropping")
		}
	})
}

func (c *TUNClient) readFromTun() {
	utils.Debugf("[TUN] readFromTun started, fd=%v name=%s", c.fd.Fd(), c.name)
	buf := make([]byte, 2048)
	for {
		n, err := c.fd.Read(buf)
		utils.Debugf("[TUN] readFromTun: n=%d err=%v", n, err)
		if err != nil {
			utils.Debugf("[TUN] read: %v", err)
			return
		}
		if n < 4 {
			continue
		}
		pkt := make([]byte, n-4)
		copy(pkt, buf[4:n])
		if len(pkt) < 20 || pkt[0]>>4 != 4 {
			utils.Debugf("[TUN] not IPv4, skipping (%d bytes)", len(pkt))
			continue
		}
		c.packetsOut.Add(1)
		utils.Debugf("[TUN] -> %d bytes proto=%d %d.%d.%d.%d -> %d.%d.%d.%d",
			len(pkt), pkt[9],
			pkt[12], pkt[13], pkt[14], pkt[15],
			pkt[16], pkt[17], pkt[18], pkt[19])
		if err := c.trans.Send(pkt); err != nil {
			utils.Debugf("[TUN] trans.Send FAIL: %v", err)
		}
	}
}

func (c *TUNClient) writeToTun() {
	for pkt := range c.inbound {
		out := make([]byte, 4+len(pkt))
		out[3] = 2 // AF_INET
		copy(out[4:], pkt)
		if _, err := c.fd.Write(out); err != nil {
			utils.Debugf("[TUN] write: %v", err)
			return
		}
		c.packetsIn.Add(1)
	}
}

func (c *TUNClient) Close() error {
	c.removeRoutes()
	exec.Command("sudo", "ifconfig", c.name, "down").Run()
	return c.fd.Close()
}

// openUtun creates a new utun interface via the PF_SYSTEM control socket.
func openUtun() (*os.File, string, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2 /* SYSPROTO_CONTROL */)
	if err != nil {
		return nil, "", fmt.Errorf("socket AF_SYSTEM: %w", err)
	}

	// Resolve the control id for com.apple.net.utun_control via CTLIOCGINFO.
	var info unix.CtlInfo
	copy(info.Name[:], "com.apple.net.utun_control")
	if err := unix.IoctlCtlInfo(fd, &info); err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("IoctlCtlInfo: %w", err)
	}

	// sc_unit = 0 lets the kernel pick the next free unit.
	sa := &unix.SockaddrCtl{
		ID:   info.Id,
		Unit: 0,
	}
	if err := unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("connect PF_SYSTEM: %w", err)
	}

	// Ask the kernel for the actual interface name (utunN).
	name, err := unix.GetsockoptString(fd, 2 /* SYSPROTO_CONTROL */, 2 /* UTUN_OPT_IFNAME */)
	if err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("getsockopt UTUN_OPT_IFNAME: %w", err)
	}

	return os.NewFile(uintptr(fd), name), name, nil
}

// Silence unused import when building only on darwin.
var _ = net.IPv4len

// realGateway returns the default router IP and the interface it is on.
// It handles macOS's habit of reporting "link#N" instead of an IP address:
// in that case, the gateway is the first usable host in the interface's
// subnet (e.g. 192.168.1.1 for 192.168.1.x/24).
func realGateway() (string, string, error) {
	// Find a PHYSICAL interface (en0, en1, ...) with IPv4. We deliberately
	// don't use "route get default" because on macOS with another VPN/tun
	// active, that returns the *other* tunnel's interface, not the physical
	// uplink.
	out, err := exec.Command("sh", "-c",
		"ifconfig -l").Output()
	if err != nil {
		return "", "", fmt.Errorf("ifconfig -l: %w", err)
	}
	names := strings.Fields(string(out))
	for _, name := range names {
		if !strings.HasPrefix(name, "en") {
			continue
		}
		ifout, err := exec.Command("ifconfig", name).Output()
		if err != nil {
			continue
		}
		ip, mask := parseIfconfigIPv4(string(ifout))
		if ip == nil || mask == nil {
			continue
		}
		gw := firstUsableHost(ip, mask)
		if gw == "" {
			continue
		}
		return gw, name, nil
	}
	return "", "", fmt.Errorf("no physical interface with IPv4 found")
}

// parseIfconfigIPv4 extracts the first inet + netmask from ifconfig output.
// Handles macOS's hex netmask form (0xffffff00).
func parseIfconfigIPv4(s string) (net.IP, net.IPMask) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "inet ") {
			continue
		}
		fields := strings.Fields(line)
		var ip net.IP
		var mask net.IPMask
		for i := 0; i < len(fields); i++ {
			switch fields[i] {
			case "inet":
				if i+1 < len(fields) {
					ip = net.ParseIP(fields[i+1]).To4()
				}
			case "netmask":
				if i+1 < len(fields) {
					mask = parseMask(fields[i+1])
				}
			}
		}
		if ip != nil && mask != nil {
			return ip, mask
		}
	}
	return nil, nil
}

// parseMask accepts both dotted-quad (255.255.255.0) and hex (0xffffff00).
func parseMask(s string) net.IPMask {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		var m uint32
		if _, err := fmt.Sscanf(s, "0x%x", &m); err == nil {
			return net.IPv4Mask(byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
		}
		return nil
	}
	if ip := net.ParseIP(s).To4(); ip != nil {
		return net.IPMask(ip)
	}
	return nil
}

// firstUsableHost returns network+1 as a dotted-quad string.
func firstUsableHost(ip net.IP, mask net.IPMask) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	network := ip4.Mask(mask)
	if len(network) == 4 {
		network[3]++
	}
	ones, bits := mask.Size()
	if bits == 0 || ones >= 31 {
		return ""
	}
	return network.String()
}

// Gateway returns the physical gateway IP resolved at SetupInterface time.
func (c *TUNClient) Gateway() string { return c.gateway }

// SetupInterface brings utun up with an address and MTU, and resolves the
// physical gateway. It does NOT touch the default route.

// ConfigureDefault installs 0.0.0.0/1 and 128.0.0.0/1 through utun.

// SaveDefault records the current default route (interface + gateway) so we
// can restore it on exit. Must be called BEFORE any tunnel routes are
// installed. On macOS with another VPN active, the default might already be
// a utun; we save exactly that and restore it later, so we never leave the
// user with a broken default.
func (c *TUNClient) SaveDefault() error {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return fmt.Errorf("route get default: %w", err)
	}
	var iface, gw string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "interface:") {
			iface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
		if strings.HasPrefix(line, "gateway:") {
			gw = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
	}
	c.savedIface = iface
	c.savedGw = gw
	utils.Debugf("[TUN] saved default: iface=%s gw=%s", iface, gw)
	if iface == "" {
		return fmt.Errorf("could not parse default interface from route output")
	}
	return nil
}

// SetupInterface brings utun up, purges leftover tunnel routes from a
// previous crashed run, and resolves the physical gateway used for bypass
// routes. It does NOT install the default route.
func (c *TUNClient) SetupInterface() error {
	// Purge leftover default-override routes from a previous run.
	exec.Command("sudo", "route", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("sudo", "route", "delete", "-net", "128.0.0.0/1").Run()

	for _, args := range [][]string{
		{"ifconfig", c.name, "10.10.10.2", "10.10.10.2", "up"},
		{"ifconfig", c.name, "mtu", "1280"},
	} {
		if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w (%s)", args, err, string(out))
		}
	}

	// Prefer the saved default's gateway for bypass routes, so they use
	// exactly the same path that worked before we started. Fall back to
	// scanning physical interfaces if it was not an IP.
	if c.savedGw != "" && net.ParseIP(c.savedGw) != nil {
		c.gateway = c.savedGw
		utils.Debugf("[TUN] bypass gateway = saved default gw %s", c.gateway)
		return nil
	}
	gw, iface, err := realGateway()
	if err != nil {
		return err
	}
	c.gateway = gw
	utils.Debugf("[TUN] bypass gateway = realGateway() %s (iface=%s)", gw, iface)
	return nil
}

// ConfigureDefault installs 0.0.0.0/1 and 128.0.0.0/1 through utun, saving
// the current default in c.saved* for later restore.

// RestoreDefault puts the default route back the way it was before we
// started (interface and gateway captured by SaveDefault).
func (c *TUNClient) RestoreDefault() {
	if !c.defaultSet && c.savedIface == "" {
		return
	}
	// Remove our overrides first.
	exec.Command("sudo", "route", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("sudo", "route", "delete", "-net", "128.0.0.0/1").Run()
	c.routesAdded = false

	if c.savedIface == "" {
		return
	}
	// Re-add the default we saved.
	if c.savedGw != "" && net.ParseIP(c.savedGw) != nil {
		exec.Command("sudo", "route", "add", "default", c.savedGw).Run()
	} else {
		exec.Command("sudo", "route", "add", "default", "-interface", c.savedIface).Run()
	}
	utils.Debugf("[TUN] default restored: iface=%s gw=%s", c.savedIface, c.savedGw)
	c.defaultSet = false
}

func (c *TUNClient) ConfigureDefault() error {
	for _, args := range [][]string{
		{"route", "add", "-net", "0.0.0.0/1", "-interface", c.name},
		{"route", "add", "-net", "128.0.0.0/1", "-interface", c.name},
	} {
		full := append([]string{"sudo"}, args...)
		out, err := exec.Command(full[0], full[1:]...).CombinedOutput()
		utils.Debugf("[TUN] exec %v -> err=%v out=%q", args, err, string(out))
		if err != nil {
			c.removeRoutes()
			return fmt.Errorf("%v: %w (%s)", args, err, string(out))
		}
	}
	c.routesAdded = true
	c.defaultSet = true
	utils.Debugf("[TUN] default routes installed")
	return nil
}
