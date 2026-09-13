package wss

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStreamConnReadsAcrossMessageBoundaries(t *testing.T) {
	client, server := newWebSocketPair(t)
	client.SetReadLimit(maxFramePayload)
	stream := newStreamConn(client)

	if err := server.WriteMessage(websocket.BinaryMessage, []byte("first-")); err != nil {
		t.Fatal(err)
	}
	if err := server.WriteMessage(websocket.BinaryMessage, []byte("second")); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, len("first-second"))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "first-second" {
		t.Fatalf("stream data = %q", got)
	}
}

func TestStreamConnSplitsLargeWrites(t *testing.T) {
	client, server := newWebSocketPair(t)
	stream := newStreamConn(client)
	payload := bytes.Repeat([]byte("x"), 2*maxFramePayload+17)

	writeResult := make(chan error, 1)
	go func() {
		n, err := stream.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		writeResult <- err
	}()

	var got []byte
	for index, wantLength := range []int{maxFramePayload, maxFramePayload, 17} {
		messageType, reader, err := server.NextReader()
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.BinaryMessage {
			t.Fatalf("message %d type = %d, want binary", index, messageType)
		}
		part, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(part) != wantLength {
			t.Fatalf("message %d length = %d, want %d", index, len(part), wantLength)
		}
		got = append(got, part...)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("reassembled payload differs from input")
	}
}

func TestStreamConnAllowsConcurrentReadAndWrite(t *testing.T) {
	client, server := newWebSocketPair(t)
	stream := newStreamConn(client)
	peerResult := make(chan error, 1)
	go func() {
		messageType, request, err := server.ReadMessage()
		if err != nil {
			peerResult <- err
			return
		}
		if messageType != websocket.BinaryMessage || string(request) != "request" {
			peerResult <- errors.New("peer received unexpected request")
			return
		}
		peerResult <- server.WriteMessage(websocket.BinaryMessage, []byte("response"))
	}()

	writeResult := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("request"))
		writeResult <- err
	}()
	response := make([]byte, len("response"))
	if _, err := io.ReadFull(stream, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q", response)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	if err := <-peerResult; err != nil {
		t.Fatal(err)
	}
}

func TestStreamConnRejectsTextMessages(t *testing.T) {
	client, server := newWebSocketPair(t)
	stream := newStreamConn(client)
	if err := server.WriteMessage(websocket.TextMessage, []byte("not stream data")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := stream.Read(buffer); !errors.Is(err, errUnexpectedMessageType) {
		t.Fatalf("Read() error = %v, want errUnexpectedMessageType", err)
	}
}

func TestStreamConnNormalClosureIsEOF(t *testing.T) {
	client, server := newWebSocketPair(t)
	stream := newStreamConn(client)
	deadline := time.Now().Add(time.Second)
	if err := server.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := stream.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want EOF", err)
	}
}

func TestStreamConnDeadlineAndAddresses(t *testing.T) {
	client, _ := newWebSocketPair(t)
	stream := newStreamConn(client)
	if stream.LocalAddr() == nil || stream.RemoteAddr() == nil {
		t.Fatal("stream addresses must not be nil")
	}
	if err := stream.SetDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	_, err := stream.Read(buffer)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read() error = %v, want timeout", err)
	}
}

func TestStreamConnCloseIsIdempotent(t *testing.T) {
	client, _ := newWebSocketPair(t)
	stream := newStreamConn(client)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
}
