//go:build linux

package main

import "testing"

func TestParseDefaultRoute(t *testing.T) {
	gw, iface, err := parseDefaultRoute("default via 192.168.1.1 dev eth0 proto dhcp metric 100\n")
	if err != nil {
		t.Fatal(err)
	}
	if gw != "192.168.1.1" || iface != "eth0" {
		t.Fatalf("got gw=%q iface=%q", gw, iface)
	}
}

func TestParseDefaultRouteSkipsUnusable(t *testing.T) {
	out := "default via fe80::1 dev eth0 metric 1024\n" +
		"default via 10.0.0.1 dev ens3 proto static metric 50\n"
	gw, iface, err := parseDefaultRoute(out)
	if err != nil {
		t.Fatal(err)
	}
	if gw != "10.0.0.1" || iface != "ens3" {
		t.Fatalf("got gw=%q iface=%q", gw, iface)
	}
}

func TestParseDefaultRouteNone(t *testing.T) {
	if _, _, err := parseDefaultRoute("10.0.0.0/8 dev eth0 proto kernel scope link\n"); err == nil {
		t.Fatal("expected error without a default route")
	}
	if _, _, err := parseDefaultRoute(""); err == nil {
		t.Fatal("expected error on empty output")
	}
}
