package transport

import (
	"encoding/json"
	"sync"
	"testing"
)

// Разделение потока — вся суть слоя: контрольный кадр не должен попасть в
// туннель как пакет, а пакет не должен уйти в обработчик. Перепутанная ветка
// означала бы, что gVisor получает JSON, а капча-логика — IP-пакеты.
func TestControlSplitsStream(t *testing.T) {
	inner := &recordingTransport{}
	c := NewControlTransport(inner)

	var mu sync.Mutex
	var packets [][]byte
	var ctrl []struct {
		typ  byte
		body []byte
	}

	c.Receive(func(p []byte) {
		mu.Lock()
		packets = append(packets, append([]byte(nil), p...))
		mu.Unlock()
	})
	c.SetControlHandler(func(typ byte, body []byte) {
		mu.Lock()
		ctrl = append(ctrl, struct {
			typ  byte
			body []byte
		}{typ, append([]byte(nil), body...)})
		mu.Unlock()
	})

	// Настоящий IPv4-пакет: первый байт 0x45.
	ipPacket := []byte{0x45, 0x00, 0x00, 0x14, 0, 0, 0, 0, 64, 6, 0, 0, 10, 0, 0, 2, 1, 1, 1, 1}
	inner.push(ipPacket)
	inner.push([]byte{controlMagic, CtrlCookiesRequest, '{', '}'})

	mu.Lock()
	defer mu.Unlock()
	if len(packets) != 1 || packets[0][0] != 0x45 {
		t.Fatalf("IP packet did not reach the tunnel: %v", packets)
	}
	if len(ctrl) != 1 || ctrl[0].typ != CtrlCookiesRequest {
		t.Fatalf("control frame did not reach the handler: %v", ctrl)
	}
}

// 0xFF невозможен как первый байт IPv4-пакета (там всегда 0x4X) — на этом
// держится всё разделение, поэтому проверяем, что данные с таким байтом в
// туннель не уходят и наоборот.
func TestControlMagicCannotCollideWithIPv4(t *testing.T) {
	for v := 0x40; v <= 0x4F; v++ {
		if byte(v) == controlMagic {
			t.Fatalf("control magic %#x collides with an IPv4 first byte", controlMagic)
		}
	}
}

func TestControlRoundTripCookies(t *testing.T) {
	inner := &recordingTransport{}
	c := NewControlTransport(inner)
	c.Receive(func([]byte) {})

	want := map[string]string{"spravka": "abc", "yandexuid": "42"}
	if err := c.SendControl(CtrlCookiesOffer, CookiesPayload{Reason: "captcha", Cookies: want}); err != nil {
		t.Fatal(err)
	}

	frames := inner.sentFrames()
	if len(frames) != 1 {
		t.Fatalf("expected one frame, got %d", len(frames))
	}
	f := frames[0]
	if f[0] != controlMagic || f[1] != CtrlCookiesOffer {
		t.Fatalf("bad header: %#v", f[:2])
	}
	var got CookiesPayload
	if err := json.Unmarshal(f[2:], &got); err != nil {
		t.Fatal(err)
	}
	if got.Reason != "captcha" || got.Cookies["spravka"] != "abc" || got.Cookies["yandexuid"] != "42" {
		t.Fatalf("payload mangled: %+v", got)
	}
}

// Кадр без обработчика не должен утечь в туннель как пакет: gVisor принял бы
// его за мусор, но хуже — мы бы не заметили потерю контрольного сообщения.
func TestControlFrameWithoutHandlerIsNotDelivered(t *testing.T) {
	inner := &recordingTransport{}
	c := NewControlTransport(inner)

	var mu sync.Mutex
	var packets int
	c.Receive(func([]byte) { mu.Lock(); packets++; mu.Unlock() })

	inner.push([]byte{controlMagic, CtrlCookiesRequest, '{', '}'})

	mu.Lock()
	defer mu.Unlock()
	if packets != 0 {
		t.Fatalf("control frame leaked into the tunnel as a packet")
	}
}
