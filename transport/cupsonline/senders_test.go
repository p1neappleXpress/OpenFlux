package cupsonline

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"
)

// cursorMessage is what the room pushes when member `from` moves its cursors to carry `chunk`.
func cursorMessage(t *testing.T, from string, chunk []byte) []byte {
	t.Helper()
	blob := binary.BigEndian.AppendUint16(nil, uint16(len(chunk)))
	blob = append(blob, chunk...)
	raw, err := json.Marshal(map[string]interface{}{
		"push": map[string]interface{}{"pub": map[string]interface{}{"data": map[string]interface{}{
			"type":    "cursors_update",
			"payload": map[string]interface{}{"user_uuid": from, "cursors": cursorsFromBytes(blob)},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// packet frames one payload as the stream carries it: 2-byte length, then the bytes.
func packet(p []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(p))), p...)
}

// Two members send at once - a PHP exit's generations handing over. One packet of A is cut across two messages and
// B's message lands between the halves: with one buffer for the room, B's bytes went into the middle of A's packet,
// the stream went out of sync and both lost data.
func TestEachMemberIsReassembledApart(t *testing.T) {
	var got [][]byte
	w := &cupsWS{config: DefaultCupsonlineConfig(), stats: &channelStats{}, onData: func(p []byte) { got = append(got, append([]byte(nil), p...)) }}
	w.authPtr.Store(&cupsAuth{userUUID: "me"})

	bigA := bytes.Repeat([]byte("A"), 5000)
	smallB1, smallB2 := []byte("b-first"), []byte("b-second")
	streamA := packet(bigA)
	w.handleMessage(cursorMessage(t, "gen-1", streamA[:2900]))
	w.handleMessage(cursorMessage(t, "gen-2", append(packet(smallB1), packet(smallB2)...)))
	w.handleMessage(cursorMessage(t, "gen-1", streamA[2900:]))
	w.handleMessage(cursorMessage(t, "me", packet([]byte("my own echo")))) // our own cursors are never data

	if len(got) != 3 {
		t.Fatalf("got %d packets, want 3 (two of B, then all of A)", len(got))
	}
	if !bytes.Equal(got[0], smallB1) || !bytes.Equal(got[1], smallB2) {
		t.Fatalf("B's packets came out as %q, %q", got[0], got[1])
	}
	if !bytes.Equal(got[2], bigA) {
		t.Fatalf("A's packet came out %d bytes, want %d intact", len(got[2]), len(bigA))
	}
	if len(w.recvBufs) != 0 {
		t.Fatalf("members with nothing pending still hold %d buffers", len(w.recvBufs))
	}
}

// A message of a batch is lost: the first half of a big packet came, its rest never will. The stream must not stay
// stuck on that half: once it has waited longer than a batch ever takes, the next message starts the stream afresh
// and its packets come through. Before, every later packet of that member went into the dead half and was lost.
func TestALostMessageDoesNotStallTheStream(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	cupsNow = func() time.Time { return clock }
	t.Cleanup(func() { cupsNow = time.Now })

	var got [][]byte
	w := &cupsWS{config: DefaultCupsonlineConfig(), stats: &channelStats{}, onData: func(p []byte) { got = append(got, append([]byte(nil), p...)) }}
	w.authPtr.Store(&cupsAuth{userUUID: "me"})

	big := bytes.Repeat([]byte("L"), 9000)
	stream := packet(big)
	w.handleMessage(cursorMessage(t, "exit", stream[:4000])) // the rest of this batch is lost

	// Within the window a message continues the packet, as a burst does.
	clock = clock.Add(500 * time.Millisecond)
	w.handleMessage(cursorMessage(t, "exit", stream[4000:8000]))
	if len(got) != 0 {
		t.Fatalf("a packet still being reassembled came out early: %d", len(got))
	}

	// Long after: the sender resends; its new batch starts at a packet boundary.
	clock = clock.Add(3 * time.Second)
	resent := []byte("resent")
	w.handleMessage(cursorMessage(t, "exit", append(packet(resent), packet([]byte("next"))...)))
	if len(got) != 2 || !bytes.Equal(got[0], resent) || !bytes.Equal(got[1], []byte("next")) {
		t.Fatalf("after the lost message got %q, want the resent packets", got)
	}
	if len(w.recvBufs) != 0 || len(w.recvSince) != 0 {
		t.Fatalf("nothing pending, yet %d buffers and %d clocks are held", len(w.recvBufs), len(w.recvSince))
	}

	// A big packet split over messages a moment apart still comes whole.
	w.handleMessage(cursorMessage(t, "exit", stream[:4000]))
	clock = clock.Add(20 * time.Millisecond)
	w.handleMessage(cursorMessage(t, "exit", stream[4000:8000]))
	clock = clock.Add(20 * time.Millisecond)
	w.handleMessage(cursorMessage(t, "exit", stream[8000:]))
	if len(got) != 3 || !bytes.Equal(got[2], big) {
		t.Fatalf("a split packet came out %d packets, last %d bytes", len(got), len(got[len(got)-1]))
	}
}
