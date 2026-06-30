package oneme

import (
	"universal-bypass-tool/transport"
	"universal-bypass-tool/utils"
)

type OneMeTransport struct {
	b     *transport.BaseTransport
	token string
	uid   int64
	exit  bool

	oneMeClient MaxClient
	ch          *CallHandler
}

func (t *OneMeTransport) Receive(callback func([]byte)) {
	t.b.Receive(callback)
}

func (t *OneMeTransport) Stats() transport.TransportStats {
	return t.b.Stats()
}

func NewOneMeTransport(isExit bool, maxToken string, maxUid int64, config transport.TransportConfig) *OneMeTransport {
	return &OneMeTransport{
		b:     transport.NewBaseTransport(config),
		token: maxToken,
		uid:   maxUid,
		exit:  isExit,
	}
}

func (t *OneMeTransport) Start() error {
	utils.Debugf("creating max client ...")
	t.oneMeClient = *NewMaxClient()
	t.oneMeClient.Connect()
	t.oneMeClient.LoginByToken(t.token)

	if t.exit {
		utils.Debugf("configured ch for exit node")
		t.ch = startIncomingListener(&t.oneMeClient)
	} else {
		utils.Debugf("configured ch for client mode")
		t.ch = startOutgoingCall(&t.oneMeClient, t.uid)
	}

	utils.Debugf("configured dc inbound")
	t.ch.dcInbound = func(data []byte) {
		t.b.CallReceive(data)
	}

	return t.b.Start()
}

func (t *OneMeTransport) Stop() error {
	return t.b.Stop()
}

func (t *OneMeTransport) IsConnected() bool {
	return true
}

func (t *OneMeTransport) Send(data []byte) error {
	t.ch.Send(data)
	return nil
}
