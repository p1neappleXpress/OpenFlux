package transport

import (
	"encoding/json"
	"fmt"
	"sync"

	"universal-bypass-tool/utils"
)

// ControlTransport carries small out-of-band messages alongside tunnel traffic
// on the same channel, so the two peers can talk about the channel itself.
//
// Its reason for existing is cookies. Yandex can put an interactive captcha in
// front of a document; the exit node cannot solve it (no browser, no user) while
// the client can. So the client hands over cookies and the node keeps working.
//
// IMPORTANT — what this can and cannot do. The captcha only blocks
// fetchDocInfo, i.e. RE-connection. While the node's current session is alive,
// this channel works and cookies get through. Once the node has lost the session
// and cannot re-open the document, there is no channel left and nothing here can
// help: that state needs an out-of-band unblock (--cookies-file). Hence the
// design goal is to deliver cookies EARLY — while the link is healthy — so the
// node never reaches the dead state, rather than to rescue it afterwards.
//
// Framing: the tunnel's own payload is raw IPv4 packets, whose first byte is
// always 0x4X. 0xFF can therefore never start a tunnel packet and is free to
// mark a control frame:
//
//	[0]   0xFF
//	[1]   message type
//	[2:]  JSON body
//
// The layer sits between the tunnel and the codec, so control frames are
// compressed and encrypted exactly like data.
type ControlTransport struct {
	Transport

	mu      sync.RWMutex
	dataCb  func([]byte)
	handler func(msgType byte, body []byte)
}

const controlMagic = 0xFF

// Control message types.
const (
	// CtrlCookiesRequest — "I need cookies for this transport." Sent by the side
	// that hit a captcha (in practice the exit node).
	CtrlCookiesRequest = byte(1)
	// CtrlCookiesOffer — "here are cookies", name -> value.
	CtrlCookiesOffer = byte(2)
	// CtrlAuthRequired — the peer (in practice the exit node) cannot pass an
	// interactive check and reports WHERE it is stuck, so the client can solve it.
	//
	// The client must solve it THROUGH the tunnel: the check is bound to the
	// address that passed it, and the cookies have to be valid for the exit
	// node's address, not the phone's. Solving it with the tunnel down produces
	// cookies that are useless to the node.
	CtrlAuthRequired = byte(3)
)

// CookiesPayload is the body of both cookie messages. Reason is free-form and
// only for logging.
type CookiesPayload struct {
	Reason  string            `json:"reason,omitempty"`
	Cookies map[string]string `json:"cookies,omitempty"`
}

// AuthRequiredPayload tells the peer which of ITS transports is stuck and where.
type AuthRequiredPayload struct {
	Transport string `json:"transport,omitempty"`
	URL       string `json:"url,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func NewControlTransport(inner Transport) *ControlTransport {
	return &ControlTransport{Transport: inner}
}

// SetControlHandler installs the callback for incoming control messages.
func (c *ControlTransport) SetControlHandler(fn func(msgType byte, body []byte)) {
	c.mu.Lock()
	c.handler = fn
	c.mu.Unlock()
}

// SendControl sends one control message.
func (c *ControlTransport) SendControl(msgType byte, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("control marshal: %w", err)
	}
	frame := make([]byte, 0, 2+len(body))
	frame = append(frame, controlMagic, msgType)
	frame = append(frame, body...)
	return c.Transport.Send(frame)
}

// Receive splits the inbound stream: control frames go to the handler, anything
// else is a tunnel packet and goes to the tunnel unchanged.
func (c *ControlTransport) Receive(callback func([]byte)) {
	c.mu.Lock()
	c.dataCb = callback
	c.mu.Unlock()

	c.Transport.Receive(func(data []byte) {
		if len(data) >= 2 && data[0] == controlMagic {
			c.mu.RLock()
			h := c.handler
			c.mu.RUnlock()
			if h != nil {
				h(data[1], data[2:])
			} else {
				utils.Debugf("[CTRL] control frame type %d with no handler", data[1])
			}
			return
		}
		c.mu.RLock()
		cb := c.dataCb
		c.mu.RUnlock()
		if cb != nil {
			cb(data)
		}
	})
}
