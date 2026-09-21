package transport

import "testing"

func TestReplayAcceptsMonotonicCounters(t *testing.T) {
	var f replayFilter
	for c := uint64(0); c < 1000; c++ {
		if !f.ValidateAndUpdate(c) {
			t.Fatalf("counter %d rejected", c)
		}
	}
}

func TestReplayRejectsDuplicate(t *testing.T) {
	var f replayFilter
	if !f.ValidateAndUpdate(5) {
		t.Fatal("first 5 rejected")
	}
	if f.ValidateAndUpdate(5) {
		t.Fatal("duplicate 5 accepted")
	}
	if !f.ValidateAndUpdate(100) {
		t.Fatal("100 rejected")
	}
	if f.ValidateAndUpdate(5) {
		t.Fatal("duplicate 5 accepted after the window moved")
	}
}

func TestReplayAcceptsOutOfOrderWithinWindow(t *testing.T) {
	var f replayFilter
	if !f.ValidateAndUpdate(1000) {
		t.Fatal("1000 rejected")
	}
	for _, c := range []uint64{999, 998, 1} {
		if !f.ValidateAndUpdate(c) {
			t.Fatalf("late counter %d rejected", c)
		}
	}
}

func TestReplayRejectsOlderThanWindow(t *testing.T) {
	var f replayFilter
	if !f.ValidateAndUpdate(10000) {
		t.Fatal("10000 rejected")
	}
	if f.ValidateAndUpdate(10000 - replayWindowSize) {
		t.Fatal("counter one full window behind was accepted")
	}
	if !f.ValidateAndUpdate(10000 - replayWindowSize + 1) {
		t.Fatal("counter just inside the window was rejected")
	}
}

func TestReplayClearsStaleBitsAfterJump(t *testing.T) {
	var f replayFilter
	// 64 lands in ring block 1, bit 0. A jump past a whole ring must clear
	// that block, so 64+8192 (same block, same bit) is a fresh counter.
	if !f.ValidateAndUpdate(64) {
		t.Fatal("64 rejected")
	}
	if !f.ValidateAndUpdate(64 + 8192 + 64) {
		t.Fatal("jump rejected")
	}
	if !f.ValidateAndUpdate(64 + 8192) {
		t.Fatal("in-window counter rejected because of a stale bit")
	}
	if f.ValidateAndUpdate(64) {
		t.Fatal("counter far behind the window accepted")
	}
}
