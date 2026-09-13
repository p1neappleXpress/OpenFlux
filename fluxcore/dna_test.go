package fluxcore

import "testing"

func TestDNAIDStableAcrossParamOrder(t *testing.T) {
	a := RouteDNA{Transport: "yandex", Exit: "fra", Params: map[string]string{"a": "1", "b": "2"}}
	b := RouteDNA{Transport: "yandex", Exit: "fra", Params: map[string]string{"b": "2", "a": "1"}}
	if a.ID() != b.ID() {
		t.Fatalf("param order must not change DNA ID: %s vs %s", a.ID(), b.ID())
	}
}

func TestDNAIDDistinguishesRoutes(t *testing.T) {
	a := RouteDNA{Transport: "yandex", Exit: "fra"}
	b := RouteDNA{Transport: "oneme", Exit: "fra"}
	if a.ID() == b.ID() {
		t.Fatal("different transports must produce different IDs")
	}
}

func TestDNALabel(t *testing.T) {
	d := RouteDNA{Transport: "yandex", Exit: "exit-fra"}
	if got := d.Label(); got != "yandex→exit-fra" {
		t.Fatalf("unexpected label %q", got)
	}
}
