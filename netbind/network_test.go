package netbind

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

var errStop = errors.New("binder stops the dial here")

func TestDialForNetworkCallsTheBinder(t *testing.T) {
	var got []string
	SetNetworkBinder(func(network string, fd uintptr) error {
		if fd == 0 {
			t.Error("binder got no socket")
		}
		got = append(got, network)
		return errStop
	})
	defer SetNetworkBinder(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// TEST-NET-1: never reached, the binder stops the dial first.
	_, err := DialContextFor("wifi")(ctx, "tcp", "192.0.2.1:9")
	if !errors.Is(err, errStop) {
		t.Fatalf("dial error %v, want the binder's", err)
	}
	if len(got) != 1 || got[0] != "wifi" {
		t.Fatalf("binder calls %v", got)
	}
}

func TestDialForNoNetworkLeavesTheSocket(t *testing.T) {
	called := false
	SetNetworkBinder(func(string, uintptr) error { called = true; return errStop })
	defer SetNetworkBinder(nil)
	ln := listen(t)
	c, err := DialContextFor("")(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// Loopback stays where it is whatever network is asked for.
	c, err = DialContextFor("cellular")(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if called {
		t.Fatal("binder called for no network or loopback")
	}
}

func TestDialForNetworkWithoutABinderFails(t *testing.T) {
	SetNetworkBinder(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := DialContextFor("cellular")(ctx, "tcp", "192.0.2.1:9")
	if err == nil || !strings.Contains(err.Error(), "cellular") {
		t.Fatalf("dial error %v, want one naming the network", err)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln
}
