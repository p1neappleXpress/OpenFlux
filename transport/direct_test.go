package transport

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// TestDirectTransportLoopback pipes a client and an exit DirectTransport
// through a real TCP listener on localhost.
func TestDirectTransportLoopback(t *testing.T) {
	ln, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	exit := NewDirectTransport(DefaultConfig(), DirectConfig{
		IsExit:           true,
		ListenAddr:       addr,
		HandshakeTimeout: 2 * time.Second,
		MaxRecordBytes:   65535,
	})
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	client := NewDirectTransport(DefaultConfig(), DirectConfig{
		IsExit:              false,
		DialAddr:            addr,
		HandshakeTimeout:    2 * time.Second,
		ReconnectMinDelay:   50 * time.Millisecond,
		ReconnectMaxDelay:   200 * time.Millisecond,
		ReconnectMultiplier: 1.2,
		MaxRecordBytes:      65535,
	})

	got := make(chan []byte, 1)
	exit.Receive(func(p []byte) { got <- append([]byte(nil), p...) })

	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	// Wait until the exit sees the client's connection.
	deadline := time.Now().Add(3 * time.Second)
	for !exit.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("exit did not accept within 3s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	want := []byte("hello through the wire")
	if err := client.Send(want); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if !bytes.Equal(p, want) {
			t.Fatalf("got %q, want %q", p, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no packet delivered")
	}
}

// netListen isolates the net.Listen call so the import stays in one place.
func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

var _ = sync.WaitGroup{}
