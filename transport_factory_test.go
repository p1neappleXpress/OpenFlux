package main

import (
	"testing"

	"openflux/transport"
	"openflux/transport/control"
	"openflux/transport/cupsonline"
)

// A client's own cupsonline transport must join the exit's rooms, not
// create rooms as if it were the exit (#96); a transport the peer asks the
// exit to start stays on the exit side.
func TestCupsonlineFactoryUsesLocalRole(t *testing.T) {
	build := func(params map[string]interface{}) *cupsonline.CupsonlineTransport {
		t.Helper()
		raw, err := transportFactory(transport.DefaultConfig())(&control.TransportConfig{
			Name: "cupsonline", Type: "cupsonline", URL: "", Params: params,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw.(*cupsonline.CupsonlineTransport)
	}

	if !build(map[string]interface{}{"exit": false}).IsClient() {
		t.Fatal("client's own cupsonline transport was built as an exit")
	}
	if build(map[string]interface{}{"exit": true}).IsClient() {
		t.Fatal("exit's cupsonline transport was built as a client")
	}
	if build(nil).IsClient() {
		t.Fatal("peer-requested cupsonline transport must stay on the exit side")
	}
}
