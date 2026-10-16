package transport

import "testing"

// Telemost's filler frames (a batch-v2 frame with no records, or with one
// empty record) are neither Session records nor classic traffic: they are
// dropped without the "classic-mode frames" complaint.
func TestFrameDemuxDropsTelemostFillerQuietly(t *testing.T) {
	d := newFrameDemux(&fakeTransport{}, "telemost")
	var session, classic int
	d.sessionCb = func([]byte) { session++ }
	for _, frame := range [][]byte{encodeBatch(nil), encodeBatch([][]byte{{}})} {
		d.dispatch(frame)
	}
	if session != 0 {
		t.Fatalf("filler reached the session side %d times", session)
	}
	// A real classic frame still reaches a classic side.
	d.classicCb = func([]byte) { classic++ }
	d.dispatch(encodeBatch([][]byte{make([]byte, 40)}))
	if classic != 1 {
		t.Fatalf("a classic frame did not reach the classic side (%d)", classic)
	}
}
