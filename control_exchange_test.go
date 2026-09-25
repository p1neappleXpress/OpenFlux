package main

import (
	"encoding/json"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

// pipeEnd — половина двунаправленного канала в памяти.
type pipeEnd struct {
	peer *pipeEnd
	cb   func([]byte)
}

func (p *pipeEnd) Start() error { return nil }
func (p *pipeEnd) Stop() error  { return nil }
func (p *pipeEnd) Send(data []byte) error {
	if cb := p.peer.cb; cb != nil {
		cb(append([]byte(nil), data...))
	}
	return nil
}
func (p *pipeEnd) Receive(cb func([]byte))         { p.cb = cb }
func (p *pipeEnd) IsConnected() bool               { return true }
func (p *pipeEnd) Stats() transport.TransportStats { return transport.TransportStats{Connected: true} }

// Нода просит куки → клиент отвечает своими. Это весь смысл контрольного канала,
// и ломается он молча: просьба уходит в пустоту, а нода продолжает сидеть на
// капче, ничем не отличаясь от обычного обрыва.
//
// Роль клиента играет wrapControl (он занимает глобальный слот ctrlConn — в
// одном процессе живёт ровно одна сторона), роль ноды — обычный
// ControlTransport на другом конце трубы.
func TestCookieExchangeNodeAsksClientAnswers(t *testing.T) {
	a, b := &pipeEnd{}, &pipeEnd{}
	a.peer, b.peer = b, a

	resetYandexRegistry()
	setInitialCookies("spravka=solved; yandexuid=777")
	defer setInitialCookies("")

	clientCore := newYandexDocs("https://disk.yandex.ru/i/client", transport.DefaultConfig())
	defer clientCore.Stop()

	clientCtrl := wrapControl(a, false)
	clientCtrl.Receive(func([]byte) {})

	nodeCtrl := transport.NewControlTransport(b)
	nodeCtrl.Receive(func([]byte) {})
	got := make(chan map[string]string, 1)
	nodeCtrl.SetControlHandler(func(typ byte, body []byte) {
		if typ != transport.CtrlCookiesOffer {
			return
		}
		var p transport.CookiesPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("bad offer: %v", err)
			return
		}
		got <- p.Cookies
	})

	if err := nodeCtrl.SendControl(transport.CtrlCookiesRequest,
		transport.CookiesPayload{Reason: "smartcaptcha"}); err != nil {
		t.Fatal(err)
	}

	select {
	case cookies := <-got:
		if cookies["spravka"] != "solved" || cookies["yandexuid"] != "777" {
			t.Fatalf("client offered the wrong cookies: %v", cookies)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never answered the cookie request")
	}
}

// Если у клиента кук нет, он не должен молча промолчать: UI обязан узнать, что
// пользователю надо пройти капчу, иначе связь просто не поднимется и никто не
// поймёт почему.
func TestCookieRequestWithoutCookiesFlagsCaptchaForUI(t *testing.T) {
	a, b := &pipeEnd{}, &pipeEnd{}
	a.peer, b.peer = b, a

	resetYandexRegistry()
	setInitialCookies("")
	docURL := "https://disk.yandex.ru/i/needscaptcha"
	core := newYandexDocs(docURL, transport.DefaultConfig())
	defer core.Stop()

	clientCtrl := wrapControl(a, false)
	clientCtrl.Receive(func([]byte) {})

	nodeCtrl := transport.NewControlTransport(b)
	nodeCtrl.Receive(func([]byte) {})
	if err := nodeCtrl.SendControl(transport.CtrlCookiesRequest,
		transport.CookiesPayload{Reason: "smartcaptcha"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pendingCaptchaURL() == docURL {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("UI was never told a captcha is needed, got %q", pendingCaptchaURL())
}
