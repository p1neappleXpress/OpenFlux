package tunnel

import (
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

type ccTransport struct {
	pipeTransport
	stream bool
}

func (c *ccTransport) OnReliableStream() bool { return c.stream }

var _ transport.StreamCarrierer = (*ccTransport)(nil)

func TestWantedCCFollowsTheCarrier(t *testing.T) {
	t.Setenv("OPENFLUX_TCP_CC", "")
	if got := wantedCC(&ccTransport{stream: true}); got != "reno" {
		t.Fatalf("over a reliable stream: %s, want reno", got)
	}
	if got := wantedCC(&ccTransport{stream: false}); got != "cubic" {
		t.Fatalf("over a carrier that drops packets: %s, want cubic", got)
	}
	if got := wantedCC(&pipeTransport{}); got != "cubic" {
		t.Fatalf("a transport that says nothing: %s, want cubic", got)
	}
	t.Setenv("OPENFLUX_TCP_CC", "cubic")
	if got := wantedCC(&ccTransport{stream: true}); got != "cubic" {
		t.Fatalf("forced cubic: %s", got)
	}
	t.Setenv("OPENFLUX_TCP_CC", "reno")
	if got := wantedCC(&ccTransport{stream: false}); got != "reno" {
		t.Fatalf("forced reno: %s", got)
	}
}
