package wss

import (
	"errors"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestControlMessageRoundTrip(t *testing.T) {
	client, server := newWebSocketPair(t)
	want := connectRequest{Version: ProtocolVersion, Command: connectCommand, Target: "example.com:443"}
	if err := writeControl(server, want); err != nil {
		t.Fatal(err)
	}
	var got connectRequest
	if err := readControl(client, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("readControl() = %#v, want %#v", got, want)
	}
}

func TestReadControlRejectsUnknownField(t *testing.T) {
	client, server := newWebSocketPair(t)
	if err := server.WriteMessage(websocket.TextMessage, []byte(`{"version":1,"command":"CONNECT","target":"example.com:443","extra":true}`)); err != nil {
		t.Fatal(err)
	}
	var request connectRequest
	if err := readControl(client, &request); err == nil {
		t.Fatal("readControl() unexpectedly accepted an unknown field")
	}
}

func TestReadControlRejectsMultipleValues(t *testing.T) {
	client, server := newWebSocketPair(t)
	if err := server.WriteMessage(websocket.TextMessage, []byte(`{"version":1} {"version":2}`)); err != nil {
		t.Fatal(err)
	}
	var request connectRequest
	if err := readControl(client, &request); err == nil {
		t.Fatal("readControl() unexpectedly accepted multiple JSON values")
	}
}

func TestReadControlRejectsBinaryMessage(t *testing.T) {
	client, server := newWebSocketPair(t)
	if err := server.WriteMessage(websocket.BinaryMessage, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	var request connectRequest
	if err := readControl(client, &request); !errors.Is(err, errUnexpectedMessageType) {
		t.Fatalf("readControl() error = %v, want errUnexpectedMessageType", err)
	}
}

func TestReadControlRejectsOversizedMessage(t *testing.T) {
	client, server := newWebSocketPair(t)
	message := []byte(`{"value":"` + strings.Repeat("x", maxControlMessage) + `"}`)
	if err := server.WriteMessage(websocket.TextMessage, message); err != nil {
		t.Fatal(err)
	}
	var value map[string]string
	if err := readControl(client, &value); err == nil {
		t.Fatal("readControl() unexpectedly accepted an oversized message")
	}
}

func TestWriteControlRejectsOversizedMessage(t *testing.T) {
	value := struct {
		Payload string `json:"payload"`
	}{Payload: strings.Repeat("x", maxControlMessage)}
	if err := writeControl(nil, value); err == nil {
		t.Fatal("writeControl() unexpectedly accepted an oversized message")
	}
}

func TestRemoteErrorDoesNotExposeInternalDetails(t *testing.T) {
	if got := (&RemoteError{}).Error(); got != "OpenFlux exit rejected the connection" {
		t.Fatalf("empty RemoteError = %q", got)
	}
	if got := (&RemoteError{Code: responsePolicyDenied}).Error(); got != "OpenFlux exit rejected the connection (policy_denied)" {
		t.Fatalf("coded RemoteError = %q", got)
	}
}
