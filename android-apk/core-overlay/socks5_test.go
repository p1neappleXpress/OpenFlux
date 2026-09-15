package socks5

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type testDialer struct {
	address   string
	requested chan string
}

func (d testDialer) DialTCP(address string) (net.Conn, error) {
	d.requested <- address
	return net.Dial("tcp", d.address)
}

func TestFragmentedHandshakeAndHalfClose(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := target.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(4 * time.Second))
		data, e := io.ReadAll(c)
		if e != nil {
			t.Error(e)
			return
		}
		c.Write(append([]byte("reply:"), data...))
	}()
	requested := make(chan string, 1)
	server := NewSOCKS5Server("", testDialer{target.Addr().String(), requested})
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	served := make(chan struct{})
	go func() {
		defer close(served)
		c, e := local.Accept()
		if e == nil {
			server.handleConnection(c)
		}
	}()
	c, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	for _, b := range []byte{5, 1, 0} {
		c.Write([]byte{b})
	}
	var hello [2]byte
	if _, err = io.ReadFull(c, hello[:]); err != nil || hello != [2]byte{5, 0} {
		t.Fatal(hello, err)
	}
	// One byte per write would break the original client's single Read(request).
	for _, b := range []byte{5, 1, 0, 1, 203, 0, 113, 7, 1, 187} {
		c.Write([]byte{b})
		time.Sleep(time.Millisecond)
	}
	var response [10]byte
	if _, err = io.ReadFull(c, response[:]); err != nil || response[1] != 0 {
		t.Fatal(response, err)
	}
	payload := bytes.Repeat([]byte("request"), 10000)
	c.Write(payload)
	c.(*net.TCPConn).CloseWrite()
	received, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, append([]byte("reply:"), payload...)) {
		t.Fatal("half-close lost the response")
	}
	if address := <-requested; address != "203.0.113.7:443" {
		t.Fatal(address)
	}
	<-done
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("SOCKS worker did not exit")
	}
}
