package transport

import (
	"bytes"
	"io"
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

	// Wait until the client is connected. The exit takes the connection as
	// active only once the first record arrives.
	deadline := time.Now().Add(3 * time.Second)
	for !client.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("client did not connect within 3s")
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

// startDirectExit starts an exit DirectTransport on a free localhost port and
// returns it, its address and a channel of the records it receives.
func startDirectExit(t *testing.T, handshake time.Duration) (*DirectTransport, string, chan []byte) {
	t.Helper()
	ln, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	exit := NewDirectTransport(DefaultConfig(), DirectConfig{
		IsExit:           true,
		ListenAddr:       addr,
		HandshakeTimeout: handshake,
		MaxRecordBytes:   65535,
	})
	got := make(chan []byte, 16)
	exit.Receive(func(p []byte) { got <- append([]byte(nil), p...) })
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exit.Stop() })
	return exit, addr, got
}

// dialRaw opens a plain TCP connection to the exit.
func dialRaw(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// writeRecord writes one length-prefixed record.
func writeRecord(t *testing.T, c net.Conn, p []byte) {
	t.Helper()
	if _, err := c.Write(append([]byte{byte(len(p) >> 8), byte(len(p))}, p...)); err != nil {
		t.Fatal(err)
	}
}

// readRecord reads one length-prefixed record within d.
func readRecord(c net.Conn, d time.Duration) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	p := make([]byte, int(hdr[0])<<8|int(hdr[1]))
	_, err := io.ReadFull(c, p)
	return p, err
}

// expectRecord waits for want on the exit's receive channel.
func expectRecord(t *testing.T, got chan []byte, want string) {
	t.Helper()
	select {
	case p := <-got:
		if string(p) != want {
			t.Fatalf("got %q, want %q", p, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("record %q not delivered", want)
	}
}

// expectClosedByExit wants c closed by the exit within d (not a local timeout).
func expectClosedByExit(t *testing.T, c net.Conn, d time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("exit did not close the connection within %v", d)
	}
	if err == nil {
		t.Fatal("unexpected data on the connection")
	}
}

// A connection that never sends anything must not keep a real client from
// reaching the exit.
func TestDirectExitSilentConnDoesNotBlockClient(t *testing.T) {
	_, addr, got := startDirectExit(t, 5*time.Second)
	_ = dialRaw(t, addr) // silent: a stale peer or a port scanner

	c := dialRaw(t, addr)
	writeRecord(t, c, []byte("hello"))
	expectRecord(t, got, "hello")
}

// A new client replaces a stale active one: data goes through the new
// connection and the old one is closed.
func TestDirectExitNewClientReplacesStale(t *testing.T) {
	exit, addr, got := startDirectExit(t, 5*time.Second)

	old := dialRaw(t, addr)
	writeRecord(t, old, []byte("old"))
	expectRecord(t, got, "old")

	// The old peer goes silent without closing; a new one connects.
	c := dialRaw(t, addr)
	writeRecord(t, c, []byte("new"))
	expectRecord(t, got, "new")

	if err := exit.Send([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	p, err := readRecord(c, 2*time.Second)
	if err != nil || string(p) != "reply" {
		t.Fatalf("reply via new conn: %q, %v", p, err)
	}
	expectClosedByExit(t, old, 2*time.Second)
}

// A connection that sends no first record within HandshakeTimeout is closed.
func TestDirectExitClosesConnWithoutFirstRecord(t *testing.T) {
	_, addr, _ := startDirectExit(t, 200*time.Millisecond)
	c := dialRaw(t, addr)
	expectClosedByExit(t, c, 2*time.Second)
}

// A connection with a bad record length is closed without becoming active
// and without disturbing the active client.
func TestDirectExitGarbageLengthKeepsClient(t *testing.T) {
	exit, addr, got := startDirectExit(t, 5*time.Second)

	c := dialRaw(t, addr)
	writeRecord(t, c, []byte("first"))
	expectRecord(t, got, "first")

	bad := dialRaw(t, addr)
	if _, err := bad.Write([]byte{0, 0}); err != nil { // length 0 is invalid
		t.Fatal(err)
	}
	expectClosedByExit(t, bad, 2*time.Second)

	if !exit.IsConnected() {
		t.Fatal("exit lost its client")
	}
	writeRecord(t, c, []byte("second"))
	expectRecord(t, got, "second")
	if err := exit.Send([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	p, err := readRecord(c, 2*time.Second)
	if err != nil || string(p) != "reply" {
		t.Fatalf("reply via active conn: %q, %v", p, err)
	}
}

// netListen isolates the net.Listen call so the import stays in one place.
func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

var _ = sync.WaitGroup{}
