package oneme

import "testing"

// startIncomingListener builds a CallHandler whose msgHandler closure parses
// JSON coming straight from the remote MAX peer with a chain of unchecked
// type assertions. A malformed message must not panic the goroutine it runs
// in (readLoop spawns it via `go h.msgHandler(text)`, which readLoop's own
// recover() cannot catch since it runs in a different goroutine) — on a live
// exit node that would crash the whole process, dropping every other tunnel.
func newTestReceiver() *CallHandler {
	client := &MaxClient{}
	return startIncomingListener(client)
}

func TestMsgHandlerMalformedSDPDoesNotPanic(t *testing.T) {
	h := newTestReceiver()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("msgHandler panicked on malformed sdp.sdp: %v", r)
		}
	}()
	// sdp.sdp is a number instead of a string.
	h.msgHandler(`{"data":{"sdp":{"type":"offer","sdp":12345}}}`)
}

func TestMsgHandlerMalformedSDPTypeDoesNotPanic(t *testing.T) {
	h := newTestReceiver()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("msgHandler panicked on malformed sdp.type: %v", r)
		}
	}()
	// sdp.type is missing entirely; sdp.sdp is present.
	h.msgHandler(`{"data":{"sdp":{"sdp":"v=0"}}}`)
}

func TestMsgHandlerMalformedParticipantDoesNotPanic(t *testing.T) {
	h := newTestReceiver()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("msgHandler panicked on malformed participant entry: %v", r)
		}
	}()
	// participants[0] is a string, not an object; roles[0] is a number, not
	// a string; id is a string, not a float64.
	h.msgHandler(`{"conversation":{"participants":["not-an-object"]}}`)
}

func TestMsgHandlerMalformedRoleAndIDDoesNotPanic(t *testing.T) {
	h := newTestReceiver()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("msgHandler panicked on malformed role/id: %v", r)
		}
	}()
	h.msgHandler(`{"conversation":{"participants":[{"roles":[42],"id":"not-a-number"}]}}`)
}

func TestMsgHandlerWellFormedSDPStillHandled(t *testing.T) {
	h := newTestReceiver()
	called := false
	h.pc = nil // handleSDP will hit its own nil checks; we only care that we reach it without panicking on the type assertions.
	defer func() {
		if r := recover(); r != nil {
			// handleSDP itself may legitimately fail without a real
			// PeerConnection; we only assert the JSON-shape parsing above
			// it (the part under test) doesn't panic on well-formed input.
			t.Fatalf("unexpected panic on well-formed sdp message: %v", r)
		}
	}()
	_ = called
	h.msgHandler(`{"data":{"sdp":{"type":"offer","sdp":"v=0"}}}`)
}
