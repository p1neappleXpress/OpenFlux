package main

import (
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

func TestFactoryBindsACarrierToItsNetwork(t *testing.T) {
	f := transportFactory(transport.DefaultConfig(), false)
	raw, err := f(&control.TransportConfig{Type: "direct", Params: map[string]interface{}{"dial": "192.0.2.1:9", "network": "wifi"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := raw.(*transport.DirectTransport).GetConfig().Network; got != "wifi" {
		t.Fatalf("carrier network %q", got)
	}
	raw, _ = f(&control.TransportConfig{Type: "direct", Params: map[string]interface{}{"dial": "192.0.2.1:9"}})
	if got := raw.(*transport.DirectTransport).GetConfig().Network; got != "" {
		t.Fatalf("carrier without a network got %q", got)
	}
	if !validNetwork("cellular") || validNetwork("lte") {
		t.Fatal("network names")
	}
}
