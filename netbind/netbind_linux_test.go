//go:build linux

package netbind

import "testing"

func TestDefaultDevice(t *testing.T) {
	dev := defaultDevice("default via 192.168.1.1 dev eth0 proto dhcp metric 100\n")
	if dev != "eth0" {
		t.Fatalf("got %q", dev)
	}
	if dev := defaultDevice(""); dev != "" {
		t.Fatalf("got %q", dev)
	}
}
