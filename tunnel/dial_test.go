package tunnel

import (
	"net"
	"testing"
	"time"
)

// With nobody answering on the other side of the transport, DialTCP must
// fail after its timeout instead of blocking forever.
func TestDialTCPTimesOutWhenTransportIsDown(t *testing.T) {
	old := dialTimeout
	dialTimeout = 200 * time.Millisecond
	defer func() { dialTimeout = old }()

	client, _ := newTransportPair() // the peer end has no stack behind it
	tun := NewTCPTunnel(client, false)
	defer tun.Close()

	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := tun.DialTCP("192.0.2.1:443")
		done <- result{conn, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("dial through a dead transport succeeded")
		}
		if r.conn != nil {
			t.Fatal("failed dial returned a non-nil net.Conn")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DialTCP still blocked after 5s")
	}
}
