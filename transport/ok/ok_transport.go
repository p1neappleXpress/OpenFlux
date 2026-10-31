package ok

import (
	"fmt"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// OkTransport is the OpenFlux transport facade (same shape as oneme.OneMeTransport).
type OkTransport struct {
	b       *transport.BaseTransport
	cookies string
	uid     int64 // peer to call (client) / own id hint (exit)
	exit    bool

	client *OkClient
	ch     *CallHandler
}

func NewOkTransport(isExit bool, cookies string, uid int64, config transport.TransportConfig) *OkTransport {
	return &OkTransport{
		b:       transport.NewBaseTransport(config),
		cookies: cookies,
		uid:     uid,
		exit:    isExit,
	}
}

func (t *OkTransport) Receive(callback func([]byte)) {
	t.b.Receive(callback)
}

func (t *OkTransport) Stats() transport.TransportStats {
	return t.b.Stats()
}

func (t *OkTransport) IsConnected() bool {
	return t.ch != nil
}

func (t *OkTransport) Send(data []byte) error {
	if t.ch == nil {
		return fmt.Errorf("ok: call not ready")
	}
	t.ch.Send(data)
	t.b.RecordSend(len(data))
	return nil
}

func (t *OkTransport) Stop() error {
	return t.b.Stop()
}

func (t *OkTransport) Start() error {
	utils.Debugf("ok: creating client (cookies len=%d)", len(t.cookies))
	if t.cookies == "" {
		return fmt.Errorf("ok: --ok-cookies required (AUTHCODE session from ok.ru)")
	}
	t.client = NewOkClient(t.cookies)

	if t.exit {
		utils.Debugf("ok: exit — waiting for incoming PORTAL call")
		t.ch = startIncomingListener(t.client)
	} else {
		utils.Debugf("ok: client — outgoing call to uid=%d", t.uid)
		if t.uid == 0 {
			return fmt.Errorf("ok: --ok-uid required for client role")
		}
		t.ch = startOutgoingCall(t.client, t.uid)
	}

	t.ch.dcInbound = func(data []byte) {
		t.b.RecordReceive(len(data))
		t.b.CallReceive(data)
	}
	return t.b.Start()
}
