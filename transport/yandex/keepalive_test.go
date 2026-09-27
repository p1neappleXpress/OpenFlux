package yandex

import (
	"strings"
	"testing"
	"time"

	"openflux/transport"
)

func TestKeepAliveVariesAndStaysRecognizable(t *testing.T) {
	sizes := map[int]bool{}
	for i := 0; i < 200; i++ {
		msg := keepAliveMessage()
		// Older nodes drop anything containing the bare marker.
		if !strings.Contains(msg, "---KA---") {
			t.Fatalf("keepalive without the marker: %s", msg)
		}
		sizes[len(msg)] = true
	}
	if len(sizes) < 10 {
		t.Fatalf("keepalive sizes barely vary: %d distinct", len(sizes))
	}

	base := 10 * time.Second
	delays := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := keepAliveDelay(base)
		if d < base/2 || d >= base*3/2 {
			t.Fatalf("delay %v outside [%v, %v)", d, base/2, base*3/2)
		}
		delays[d] = true
	}
	if len(delays) < 100 {
		t.Fatalf("keepalive delays barely vary: %d distinct", len(delays))
	}
}

// Our own data frames are base64 and can never contain the marker.
func TestHandleMessageDropsPaddedKeepAlive(t *testing.T) {
	tr := NewYandexDocsTransport("https://docs.example/d", transport.DefaultConfig())
	got := false
	tr.Receive(func([]byte) { got = true })
	tr.handleMessage(nil, []byte(keepAliveMessage()))
	if got {
		t.Fatal("a padded keepalive was delivered as data")
	}
	tr.handleMessage(nil, []byte(`42["message",{"type":"cursor","cursor":"18;AQID"}]`))
	if !got {
		t.Fatal("control: a data cursor was not delivered")
	}
}
