package wss

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
)

const (
	ProtocolVersion       = 1
	Subprotocol           = "openflux.v1"
	TunnelPath            = "/openflux/v1/tunnel"
	maxControlMessage     = 8 * 1024
	maxFramePayload       = 64 * 1024
	maxTargetLength       = 512
	connectCommand        = "CONNECT"
	responseCodeOK        = "ok"
	responseBadRequest    = "bad_request"
	responseUnsupported   = "unsupported_version"
	responsePolicyDenied  = "policy_denied"
	responseResolveFailed = "resolve_failed"
	responseConnectFailed = "connect_failed"
)

var errUnexpectedMessageType = errors.New("unexpected WebSocket message type")

type connectRequest struct {
	Version int    `json:"version"`
	Command string `json:"command"`
	Target  string `json:"target"`
}

type connectResponse struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
}

// RemoteError reports only a coarse exit-node result. Internal resolver,
// policy, and dial details remain on the exit side.
type RemoteError struct {
	Code string
}

func (e *RemoteError) Error() string {
	if e.Code == "" {
		return "OpenFlux exit rejected the connection"
	}
	return fmt.Sprintf("OpenFlux exit rejected the connection (%s)", e.Code)
}

// SOCKS5ReplyCode lets the local SOCKS front end preserve the exit's coarse
// failure category without exposing internal error details.
func (e *RemoteError) SOCKS5ReplyCode() byte {
	switch e.Code {
	case responsePolicyDenied:
		return 0x02
	case responseResolveFailed:
		return 0x04
	case responseConnectFailed:
		return 0x05
	default:
		return 0x01
	}
}

func writeControl(conn *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxControlMessage {
		return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func readControl(conn *websocket.Conn, value any) error {
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if messageType != websocket.TextMessage {
		return errUnexpectedMessageType
	}
	if len(data) > maxControlMessage {
		return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("control message contains multiple JSON values")
		}
		return err
	}
	return nil
}
