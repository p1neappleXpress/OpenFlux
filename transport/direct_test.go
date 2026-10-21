package transport

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"
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

// TestDirectExitGhostConnYieldsLane: the exit serves one peer at a time, and
// a connection that never completes a record must not hold the lane against
// a client that speaks. This is the zombie first connection to a freshly
// started node: a ghost (an abandoned redial, a probe that stalls mid-record)
// binds the lane, the kernel keeps acknowledging the real client's bytes into
// the backlog, and the node never answers.
func TestDirectExitGhostConnYieldsLane(t *testing.T) {
	ln, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	exit := NewDirectTransport(DefaultConfig(), DirectConfig{
		IsExit:         true,
		ListenAddr:     addr,
		MaxRecordBytes: 65535,
	})
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	// The ghost: promises a 256-byte record and sends nothing else.
	ghost, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ghost.Close()
	if _, err := ghost.Write([]byte{0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !exit.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("exit did not accept the ghost within 3s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	expireLaneGrace(exit)

	got := make(chan []byte, 1)
	exit.Receive(func(p []byte) { got <- append([]byte(nil), p...) })

	// The client: a second connection whose record waits in the backlog.
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	want := []byte("hello after the ghost")
	frame := append([]byte{byte(len(want) >> 8), byte(len(want))}, want...)
	if _, err := client.Write(frame); err != nil {
		t.Fatal(err)
	}

	// The ghost's first-record grace expires and the lane passes to the
	// client, whose record is already waiting.
	select {
	case p := <-got:
		if !bytes.Equal(p, want) {
			t.Fatalf("got %q, want %q", p, want)
		}
	case <-time.After(directFirstRecord + 5*time.Second):
		t.Fatal("the silent connection held the lane past its grace")
	}
}

var _ = sync.WaitGroup{}

// expireLaneGrace moves the incumbent's handshake grace window into the
// past, so a challenger decides on the lane guard alone.
func expireLaneGrace(t *DirectTransport) {
	t.mu.Lock()
	t.connAt = time.Now().Add(-directDance - time.Second)
	t.mu.Unlock()
}

// TestDirectLaneGuard: a challenger is refused while the session above
// reports the incumbent peer alive, and takes the lane from it once the
// session stops talking to that peer - the stale client that helloes forever
// but never finishes the handshake must not hold every later client in the
// backlog.
func TestDirectLaneGuard(t *testing.T) {
	ln, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	exit := NewDirectTransport(DefaultConfig(), DirectConfig{
		IsExit:         true,
		ListenAddr:     addr,
		MaxRecordBytes: 65535,
	})
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	var hold atomic.Bool
	exit.SetLaneGuard(func() bool { return hold.Load() })

	got := make(chan []byte, 4)
	exit.Receive(func(p []byte) { got <- append([]byte(nil), p...) })
	send := func(c net.Conn, msg string) {
		frame := append([]byte{byte(len(msg) >> 8), byte(len(msg))}, msg...)
		if _, err := c.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(want string) {
		select {
		case p := <-got:
			if string(p) != want {
				t.Fatalf("got %q, want %q", p, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no packet delivered, want %q", want)
		}
	}

	incumbent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer incumbent.Close()
	send(incumbent, "incumbent")
	expect("incumbent")

	// The session is talking to the incumbent: the challenger is refused,
	// its frame never delivered.
	hold.Store(true)
	challenger, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer challenger.Close()
	send(challenger, "challenger")
	_ = challenger.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := challenger.Read(buf); err == nil {
		t.Fatal("the challenger was not refused while the lane was held")
	}
	select {
	case p := <-got:
		t.Fatalf("a refused challenger's frame %q was delivered", p)
	case <-time.After(200 * time.Millisecond):
	}

	// The peer went silent: the next challenger takes the lane, and the
	// incumbent's connection closes under it.
	hold.Store(false)
	expireLaneGrace(exit)
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	send(second, "second")
	expect("second")
	_ = incumbent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := incumbent.Read(buf); err == nil {
		t.Fatal("the displaced incumbent's connection stayed open")
	}
}
