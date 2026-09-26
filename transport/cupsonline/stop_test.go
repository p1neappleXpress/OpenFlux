package cupsonline

import (
	"testing"

	"openflux/transport"
)

// A Session stops a carrier through every wrapper around it; the second
// Stop must not close the channels again.
func TestStopTwice(t *testing.T) {
	c := NewCupsonlineTransport("", transport.TransportConfig{}, false)
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
}
