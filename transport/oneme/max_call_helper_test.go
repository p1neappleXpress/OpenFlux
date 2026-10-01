package oneme

import (
	"encoding/json"
	"testing"
)

func TestParseCallerParams(t *testing.T) {
	valid := `{"internalCallerParams":"{\"id\":{\"internal\":123,\"external\":\"abc\"},\"isConcurrent\":false,\"endpoint\":\"wss://relay.example/ws\",\"turn\":{\"urls\":[]},\"stun\":{\"urls\":[]}}"}`
	cases := []struct {
		name    string
		payload json.RawMessage
		wantErr bool
	}{
		{"valid params", json.RawMessage(valid), false},
		{"error response", json.RawMessage(`{"error":"AUTH_FAILED"}`), true},
		{"null payload", nil, true},
		{"invalid json", json.RawMessage(`{invalid`), true},
		{"params not a string", json.RawMessage(`{"internalCallerParams":{"a":1}}`), true},
		{"empty endpoint", json.RawMessage(`{"internalCallerParams":"{\"id\":{\"internal\":123},\"endpoint\":\"\"}"}`), true},
		{"params not json", json.RawMessage(`{"internalCallerParams":"not-json"}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, err := parseCallerParams(tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", params)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if params.Endpoint != "wss://relay.example/ws" {
				t.Fatalf("unexpected endpoint: %q", params.Endpoint)
			}
		})
	}
}
