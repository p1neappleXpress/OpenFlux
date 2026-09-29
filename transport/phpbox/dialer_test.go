package phpbox

import (
	"io"
	"testing"
)

func TestSocksDialerOverMux(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()

	m := NewMux(newPipeCarrier())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	d := NewSocksDialer(m)
	conn, err := d.DialTCP(echo.Addr().String())
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	msg := "socks dialer over the mux"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatalf("got %q want %q", got, msg)
	}
}
