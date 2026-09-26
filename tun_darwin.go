//go:build darwin

package main

import (
	"encoding/hex"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"openflux/network"
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
	setup := [][]string{
		{"ifconfig", c.name, "10.10.10.2", "10.10.10.2", "up"},
		{"ifconfig", c.name, "mtu", "1280"},
	}
	for _, args := range setup {
		if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w (%s)", args, err, string(out))
		}
	}

	gw, iface, err := realGateway()
	if err != nil {
		return fmt.Errorf("find default gateway: %w", err)
	}
	c.gateway = gw
	utils.Debugf("[TUN] real gateway: %s (iface=%s)", gw, iface)

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

	defaults := [][]string{
		{"route", "add", "-net", "0.0.0.0/1", "-interface", c.name},
		{"route", "add", "-net", "128.0.0.0/1", "-interface", c.name},
	}
	for _, args := range defaults {
		if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
			c.removeRoutes()
			return fmt.Errorf("%v: %w (%s)", args, err, string(out))
		}
	}
	c.routesAdded = true
	return nil
}

func (c *TUNClient) removeRoutes() {
	if !c.routesAdded && len(c.bypassIPs) == 0 {
		return
	}
	exec.Command("sudo", "route", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("sudo", "route", "delete", "-net", "128.0.0.0/1").Run()
	for _, ip := range c.bypassIPs {
		exec.Command("sudo", "route", "delete", "-host", ip).Run()
	}
	c.bypassIPs = nil
	c.routesAdded = false
}

func (c *TUNClient) Start() {
	go c.readFromTun()
	go c.writeToTun()

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

		// -> : packet leaves the device towards the tunnel / exit.
		if utils.Level() >= 1 {
			utils.Debugf("[TUN] %s", network.FormatPacket(network.DirOutbound, pkt))
		}
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[TUN] ->net payload hexdump:\n%s", hex.Dump(pkt))
		}

		if err := c.trans.Send(pkt); err != nil {
			utils.Debugf("[TUN] trans.Send FAIL: %v", err)
		}
	}
}

func (c *TUNClient) writeToTun() {
	for pkt := range c.inbound {
		c.packetsIn.Add(1)

		// <- : packet arrives from the tunnel / exit towards the device.
		if utils.Level() >= 1 {
			utils.Debugf("[TUN] %s", network.FormatPacket(network.DirInbound, pkt))
		}
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[TUN] <-net payload hexdump:\n%s", hex.Dump(pkt))
		}

		out := make([]byte, 4+len(pkt))
		out[3] = 2 // AF_INET
		copy(out[4:], pkt)
		if _, err := c.fd.Write(out); err != nil {
			utils.Debugf("[TUN] write: %v", err)
			return
		}
	}
}

func (c *TUNClient) Close() error {
	c.removeRoutes()
	exec.Command("sudo", "ifconfig", c.name, "down").Run()
	return c.fd.Close()
}

func openUtun() (*os.File, string, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
	if err != nil {
		return nil, "", fmt.Errorf("socket AF_SYSTEM: %w", err)
	}

	var info unix.CtlInfo
	copy(info.Name[:], "com.apple.net.utun_control")
	if err := unix.IoctlCtlInfo(fd, &info); err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("IoctlCtlInfo: %w", err)
	}

	sa := &unix.SockaddrCtl{
		ID:   info.Id,
		Unit: 0,
	}
	if err := unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("connect PF_SYSTEM: %w", err)
	}

	name, err := unix.GetsockoptString(fd, 2, 2)
	if err != nil {
		unix.Close(fd)
		return nil, "", fmt.Errorf("getsockopt UTUN_OPT_IFNAME: %w", err)
	}

	return os.NewFile(uintptr(fd), name), name, nil
}

var _ = net.IPv4len

func realGateway() (string, string, error) {
	out, err := exec.Command("sh", "-c", "ifconfig -l").Output()
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

func (c *TUNClient) Gateway() string { return c.gateway }

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

func (c *TUNClient) SetupInterface() error {
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

func (c *TUNClient) RestoreDefault() {
	if !c.defaultSet && c.savedIface == "" {
		return
	}
	exec.Command("sudo", "route", "delete", "-net", "0.0.0.0/1").Run()
	exec.Command("sudo", "route", "delete", "-net", "128.0.0.0/1").Run()
	c.routesAdded = false

	if c.savedIface == "" {
		return
	}
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
