//go:build linux

package netbind

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var boundDevice string

// Device is the interface sockets are bound to, "" when none.
func Device() string { return boundDevice }

// BindDefault binds every new socket of the core to the interface that
// carries the default route now, before the tunnel moves it. Returns that
// interface's index. Needs root (SO_BINDTODEVICE).
func BindDefault() (uint32, error) {
	out, err := exec.Command("ip", "-o", "route", "show", "default").Output()
	if err != nil {
		return 0, fmt.Errorf("ip route show default: %w (need iproute2)", err)
	}
	dev := defaultDevice(string(out))
	if dev == "" {
		return 0, fmt.Errorf("no usable IPv4 default route")
	}
	iface, err := net.InterfaceByName(dev)
	if err != nil {
		return 0, fmt.Errorf("interface %s: %w", dev, err)
	}
	BindDevice(dev)
	return uint32(iface.Index), nil
}

// BindDevice binds new sockets to the named interface.
func BindDevice(dev string) {
	boundDevice = dev
	install(func(network, address string, c syscall.RawConn) error {
		if host, _, err := net.SplitHostPort(address); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return nil
			}
		}
		var err error
		cerr := c.Control(func(fd uintptr) {
			err = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, dev)
		})
		if cerr != nil {
			return cerr
		}
		return err
	})
}

func defaultDevice(s string) string {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "dev" {
				return fields[i+1]
			}
		}
	}
	return ""
}
