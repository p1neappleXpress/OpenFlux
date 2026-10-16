package oneme

import "testing"

// Signaling messages come from the other side of a MAX call; a malformed
// one must be ignored, not panic the exit process (#82).
func TestMalformedSignalingDoesNotPanic(t *testing.T) {
	msgs := []string{
		`{"conversation":{"participants":["not-an-object"]}}`,
		`{"conversation":{"participants":[{"roles":[42],"id":"x"}]}}`,
		`{"data":{"sdp":{"type":7,"sdp":null}}}`,
		`{"data":{"sdp":{"type":"answer","sdp":7}}}`,
		`{"conversationParams":{"turn":"x","stun":{"urls":[5]}}}`,
	}
	h := startIncomingListener(&MaxClient{})
	for _, m := range msgs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("message %s panicked: %v", m, r)
				}
			}()
			h.msgHandler(m)
		}()
	}
}

func TestParticipantID(t *testing.T) {
	data := map[string]interface{}{"conversation": map[string]interface{}{
		"participants": []interface{}{
			"junk",
			map[string]interface{}{"id": float64(7), "roles": []interface{}{"CREATOR"}},
			map[string]interface{}{"id": float64(9), "roles": []interface{}{}},
		},
	}}
	if id, ok := participantID(data, true); !ok || id != 7 {
		t.Fatalf("creator = %d, %v", id, ok)
	}
	if id, ok := participantID(data, false); !ok || id != 9 {
		t.Fatalf("callee = %d, %v", id, ok)
	}
}

func TestDecodeCallDetailsRejectsBadSize(t *testing.T) {
	for _, vcp := range []string{"-01:AAAA", "000:AAAA"} {
		if _, err := decodeCallDetails(vcp); err == nil {
			t.Fatalf("decodeCallDetails(%q) accepted a bad size", vcp)
		}
	}
}
