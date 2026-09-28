package yandex

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNewBoardsAPIRequestUsesJSONEnvelope(t *testing.T) {
	req, err := newBoardsAPIRequest("board-hash", "request-guest-token", map[string]string{
		"name": "guest_test",
		"hash": "board-hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := req.Header.Get("Referer"); !strings.Contains(got, "board-hash") {
		t.Fatalf("Referer = %q, want board hash", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Action  string `json:"action"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if envelope.Action != "request-guest-token" {
		t.Fatalf("action = %q", envelope.Action)
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.Content)
	if err != nil {
		t.Fatalf("content is not base64: %v", err)
	}
	var content map[string]string
	if err := json.Unmarshal(decoded, &content); err != nil {
		t.Fatalf("decoded content is not JSON: %v", err)
	}
	if content["name"] != "guest_test" || content["hash"] != "board-hash" {
		t.Fatalf("unexpected content: %#v", content)
	}
}

func TestGetWhiteboardInfoUsesJSONRequest(t *testing.T) {
	client := &http.Client{Transport: boardsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]string
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if envelope["action"] != "get-whiteboard-info" {
			t.Fatalf("action = %q", envelope["action"])
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"presentation":{"properties":{"current_slide":"slide"}},"socket_servers":[{"ip":"socket.example"}]}`)),
			Header:     make(http.Header),
		}, nil
	})}

	info, err := (&BoardsTransport{}).getWhiteboardInfo(client, "board-hash")
	if err != nil {
		t.Fatal(err)
	}
	if info["dashboard"] != "slide" || info["ws_host"] != "socket.example" {
		t.Fatalf("unexpected info: %#v", info)
	}
}

type boardsRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn boardsRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
