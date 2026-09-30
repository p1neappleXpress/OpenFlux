//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// The Windows full tunnel: a Wintun adapter takes the default route and
// its IPv4 packets go straight to the transport, like the macOS utun client
// and the Android VpnService. The core's own sockets are bound to the real
// interface (netbind), so the carriers never loop into the tunnel and no
// host routes are needed. IPv6 is routed into the adapter too and dropped,
// so programs fall back to IPv4 instead of leaking past the tunnel. The
// routes live only as long as the adapter, which Wintun removes when the
// process ends, however it ends.

const (
	adapterName = "OpenFlux"
	localIPv4   = "10.10.10.2"
	localMask   = "255.255.255.0"
	peerIPv4    = "10.10.10.1"
	localIPv6   = "fd0f:1f10:ec5::2/64"
)

// A fixed GUID: Windows keeps seeing the same adapter instead of a new
// "Network N" every time.
var adapterGUID = windows.GUID{Data1: 0x0f1f10ec, Data2: 0x0de5, Data3: 0x4b3a, Data4: [8]byte{0x9c, 0x4e, 0x0f, 0x1f, 0x10, 0xec, 0x5d, 0x01}}

type TUNClient struct {
	trans transport.Transport
	dev   tun.Device
	name  string
	index uint32
	mtu   int

	physical uint32

	packetsIn  atomic.Uint64
	packetsOut atomic.Uint64
	dropped6   atomic.Uint64
	closed     atomic.Bool
}

func NewTUNClient(trans transport.Transport, mtu int) (*TUNClient, error) {
	dev, err := tun.CreateTUNWithRequestedGUID(adapterName, &adapterGUID, mtu)
	if err != nil {
		return nil, fmt.Errorf("не удалось создать адаптер Wintun (нужны права администратора и wintun.dll рядом с ядром): %w", err)
	}
	name, _ := dev.Name()
	c := &TUNClient{trans: trans, dev: dev, name: name, mtu: mtu}
	if native, ok := dev.(*tun.NativeTun); ok {
		row := windows.MibIfRow2{InterfaceLuid: native.LUID()}
		if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormal, &row); err != nil {
			dev.Close()
			return nil, fmt.Errorf("адаптер Wintun: %w", err)
		}
		c.index = row.InterfaceIndex
	}
	if c.index == 0 {
		dev.Close()
		return nil, fmt.Errorf("адаптер Wintun без номера интерфейса")
	}
	return c, nil
}

func (c *TUNClient) Name() string { return fmt.Sprintf("%s (интерфейс %d)", c.name, c.index) }

// Gateway names the real interface the carriers leave through.
func (c *TUNClient) Gateway() string { return "интерфейс " + strconv.Itoa(int(c.physical)) }

// SaveDefault binds the core's sockets to the real interface, unless main
// did so before the transports started (it should: sockets opened before
// the binding would follow the default route into the tunnel).
func (c *TUNClient) SaveDefault() error {
	if netbind.Bound() {
		c.physical = netbind.Index()
		return nil
	}
	index, err := netbind.BindDefault()
	c.physical = index
	return err
}

// SetupInterface gives the adapter its addresses and MTU.
func (c *TUNClient) SetupInterface() error {
	idx := strconv.Itoa(int(c.index))
	steps := [][]string{
		{"interface", "ipv4", "set", "address", "name=" + idx, "source=static", "address=" + localIPv4, "mask=" + localMask, "store=active"},
		{"interface", "ipv4", "set", "subinterface", idx, "mtu=" + strconv.Itoa(c.mtu), "store=active"},
		{"interface", "ipv4", "set", "interface", idx, "metric=1"},
	}
	for _, args := range steps {
		if err := netsh(args...); err != nil {
			return err
		}
	}
	// IPv6 only to drop it; a system without IPv6 is fine as well.
	_ = netsh("interface", "ipv6", "add", "address", "interface="+idx, "address="+localIPv6, "store=active")
	return nil
}

// ConfigureDefault takes the default route: two /1 routes beat 0.0.0.0/0
// without touching it, so the real default stays for the bound sockets.
func (c *TUNClient) ConfigureDefault() error {
	idx := strconv.Itoa(int(c.index))
	for _, prefix := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if err := netsh("interface", "ipv4", "add", "route", "prefix="+prefix, "interface="+idx, "nexthop="+peerIPv4, "metric=1", "store=active"); err != nil {
			return err
		}
	}
	for _, prefix := range []string{"::/1", "8000::/1"} {
		_ = netsh("interface", "ipv6", "add", "route", "prefix="+prefix, "interface="+idx, "metric=1", "store=active")
	}
	return nil
}

// Start runs the two forwarding loops.
func (c *TUNClient) Start() {
	c.trans.Receive(func(pkt []byte) {
		if c.closed.Load() {
			return
		}
		// Write copies the packet into Wintun's ring before it returns.
		if _, err := c.dev.Write([][]byte{pkt}, 0); err != nil {
			utils.Debugf("[TUN] write: %v", err)
			return
		}
		c.packetsIn.Add(1)
	})
	go c.readFromTun()
	go c.report()
}

func (c *TUNClient) readFromTun() {
	bufs := [][]byte{make([]byte, 65535)}
	sizes := []int{0}
	for !c.closed.Load() {
		n, err := c.dev.Read(bufs, sizes, 0)
		if err != nil {
			if !c.closed.Load() {
				utils.Debugf("[TUN] read: %v", err)
			}
			return
		}
		if n == 0 || sizes[0] < 20 {
			continue
		}
		pkt := bufs[0][:sizes[0]]
		if pkt[0]>>4 != 4 {
			c.dropped6.Add(1) // IPv6: routed here only to be dropped
			continue
		}
		out := make([]byte, len(pkt))
		copy(out, pkt)
		c.packetsOut.Add(1)
		if err := c.trans.Send(out); err != nil {
			utils.Debugf("[TUN] trans.Send: %v", err)
		}
	}
}

func (c *TUNClient) report() {
	for !c.closed.Load() {
		time.Sleep(time.Minute)
		utils.Debugf("[TUN] packets out=%d in=%d ipv6 dropped=%d", c.packetsOut.Load(), c.packetsIn.Load(), c.dropped6.Load())
	}
}

// Close removes the adapter, and with it its addresses and routes.
func (c *TUNClient) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.dev.Close()
}

// RestoreDefault has nothing left to do: the routes went with the adapter.
func (c *TUNClient) RestoreDefault() {}

func (c *TUNClient) purgeStaleHostRoutes() {}

// SocketWatcher waits for nothing on Windows: the carriers' sockets are
// bound to the real interface, so the default route can move at once.
type SocketWatcher struct{ onStable func() }

func NewSocketWatcher(gateway string, onStable func()) *SocketWatcher {
	return &SocketWatcher{onStable: onStable}
}

func (w *SocketWatcher) Start(interval time.Duration) { go w.onStable() }
func (w *SocketWatcher) Stop()                        {}

func netsh(args ...string) error {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh %s: %v: %s", strings.Join(args[:4], " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
