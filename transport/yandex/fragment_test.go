package yandex

import (
	"bytes"
	"testing"
)

func TestFragmentFrameRoundTrip(t *testing.T) {
	// The largest frame that must work is the one the index field can address.
	// Anything past that is rejected loudly rather than truncated, so the bound
	// is asserted here instead of being discovered in production as a frame
	// that never completes.
	sizes := []int{
		0, 1, 2, 100,
		maxFragmentBytes - fragHeaderLen, maxFragmentBytes, 4096,
		8192,   // a full batched frame
		65535,  // a maximum-size tunnel IP packet
		300000, // many fragments
	}
	for _, size := range sizes {
		wire := make([]byte, size)
		for i := range wire {
			wire[i] = byte(i * 7)
		}
		frags, err := FragmentFrame(wire)
		if err != nil {
			t.Fatalf("size %d: FragmentFrame: %v", size, err)
		}
		if len(frags) > maxFragmentsPerFrame {
			t.Fatalf("size %d: %d fragments, over the %d limit",
				size, len(frags), maxFragmentsPerFrame)
		}
		for i, f := range frags {
			if len(f) > maxFragmentBytes {
				t.Fatalf("size %d: fragment %d is %d bytes, over the %d limit",
					size, i, len(f), maxFragmentBytes)
			}
		}
		var reasm frameReassembler
		var got []byte
		for _, f := range frags {
			out, ok := reasm.push(f, 0)
			if !ok {
				continue
			}
			got = out
		}
		if !bytes.Equal(got, wire) && !(len(got) == 0 && len(wire) == 0) {
			t.Fatalf("size %d: reassembled %d bytes, want %d (equal=%v)",
				size, len(got), len(wire), bytes.Equal(got, wire))
		}
	}
}

func TestFrameReassemblerDropsGappedFrame(t *testing.T) {
	wire := bytes.Repeat([]byte{0xab}, 5000)
	frags, err := FragmentFrame(wire)
	if err != nil {
		t.Fatalf("FragmentFrame: %v", err)
	}
	if len(frags) < 2 {
		t.Fatalf("expected the frame to be split, got %d fragment(s)", len(frags))
	}

	var reasm frameReassembler
	// Deliver everything except fragment 1. Nothing may come out: a partial
	// reassembly spliced onto the next frame is worse than losing this one.
	for i, f := range frags {
		if i == 1 {
			continue
		}
		if _, ok := reasm.push(f, 0); ok {
			t.Fatalf("fragment %d completed a frame, but one is missing", i)
		}
	}
	// Let the deadline pass; the next frame must then start clean.
	reasm.push(frags[1], int64(reassembleTimeout+1)*1e6)

	frags2, err := FragmentFrame([]byte("second frame"))
	if err != nil {
		t.Fatalf("FragmentFrame: %v", err)
	}
	for i, f := range frags2 {
		out, ok := reasm.push(f, int64(reassembleTimeout+2)*1e6)
		if i != len(frags2)-1 {
			if ok {
				t.Fatalf("fragment %d/%d completed early", i, len(frags2))
			}
			continue
		}
		if !ok {
			t.Fatal("the second frame never completed after the first was abandoned")
		}
		if string(out) != "second frame" {
			t.Fatalf("got %q, want %q", out, "second frame")
		}
	}
}

func TestFrameReassemblerToleratesOutOfOrderFragments(t *testing.T) {
	// Real delivery over RTP reorders packets even when none are lost - more
	// so once a publish tick can emit many fragments back to back
	// (maxFramesPerTick). The reassembler used to accept a fragment only when
	// its index was exactly the next expected one, silently discarding
	// anything else; a frame missing even one out-of-order fragment could
	// then only ever time out, no matter how completely it actually arrived.
	wire := bytes.Repeat([]byte{0x5a}, 6000)
	frags, err := FragmentFrame(wire)
	if err != nil {
		t.Fatalf("FragmentFrame: %v", err)
	}
	if len(frags) < 3 {
		t.Fatalf("expected at least 3 fragments, got %d", len(frags))
	}

	// Reverse delivery order: the last fragment first, the first fragment last.
	var reasm frameReassembler
	var got []byte
	var ok bool
	for i := len(frags) - 1; i >= 0; i-- {
		got, ok = reasm.push(frags[i], 0)
		if i > 0 && ok {
			t.Fatalf("frame completed after %d of %d fragments", len(frags)-i, len(frags))
		}
	}
	if !ok {
		t.Fatal("the frame never completed despite every fragment arriving")
	}
	if !bytes.Equal(got, wire) {
		t.Fatalf("reassembled %d bytes, want %d matching the original", len(got), len(wire))
	}
}

func TestFrameReassemblerCompletesTrulyInterleavedFrames(t *testing.T) {
	// A single reassembly buffer can only ever track one frame: a second
	// frame starting before the first finishes necessarily evicts it, no
	// matter how correctly frames are told apart. reassemblySlots exists so
	// that frames which are genuinely interleaved on the wire - not just
	// arriving out of order within themselves - can still both complete.
	wireA := bytes.Repeat([]byte{0x11}, 4000)
	wireB := bytes.Repeat([]byte{0x22}, 7000)
	fragsA, err := FragmentFrame(wireA)
	if err != nil {
		t.Fatalf("FragmentFrame(A): %v", err)
	}
	fragsB, err := FragmentFrame(wireB)
	if err != nil {
		t.Fatalf("FragmentFrame(B): %v", err)
	}
	if len(fragsA) < 2 || len(fragsB) < 2 {
		t.Fatalf("expected both frames to need multiple fragments, got %d and %d", len(fragsA), len(fragsB))
	}

	// Interleave: A's first fragment, B's first fragment, A's second, B's
	// second, and so on, each frame's fragments still in their own order.
	var reasm frameReassembler
	results := map[string][]byte{}
	i, j := 0, 0
	for i < len(fragsA) || j < len(fragsB) {
		if i < len(fragsA) {
			if out, ok := reasm.push(fragsA[i], 0); ok {
				results["A"] = out
			}
			i++
		}
		if j < len(fragsB) {
			if out, ok := reasm.push(fragsB[j], 0); ok {
				results["B"] = out
			}
			j++
		}
	}
	gotA, gotB := results["A"], results["B"]
	if !bytes.Equal(gotA, wireA) {
		t.Fatalf("frame A: reassembled %d bytes, want %d matching the original", len(gotA), len(wireA))
	}
	if !bytes.Equal(gotB, wireB) {
		t.Fatalf("frame B: reassembled %d bytes, want %d matching the original", len(gotB), len(wireB))
	}
}

func TestFrameReassemblerDisambiguatesSameSizedInterleavedFrames(t *testing.T) {
	// Two unrelated frames of identical length fragment into an identical
	// (count, total) pair - that pairing used to be the reassembler's only
	// notion of frame identity. If frame A's first fragment arrives, then
	// frame B starts and finishes before A's remaining fragments show up,
	// (count, total) alone cannot tell the two apart: A's late fragments
	// would be accepted as if they belonged to B, splicing A's bytes into
	// what should have been a clean copy of B. frameSeq is what actually
	// distinguishes them.
	wireA := bytes.Repeat([]byte{0xAA}, 5000)
	wireB := bytes.Repeat([]byte{0xBB}, 5000)
	fragsA, err := FragmentFrame(wireA)
	if err != nil {
		t.Fatalf("FragmentFrame(A): %v", err)
	}
	fragsB, err := FragmentFrame(wireB)
	if err != nil {
		t.Fatalf("FragmentFrame(B): %v", err)
	}
	if len(fragsA) != len(fragsB) || len(fragsA) < 2 {
		t.Fatalf("expected equal-length, multi-fragment frames; got %d and %d", len(fragsA), len(fragsB))
	}

	var reasm frameReassembler
	// A's first fragment arrives, then B fully interrupts and completes
	// before any more of A shows up.
	if _, ok := reasm.push(fragsA[0], 0); ok {
		t.Fatal("a single fragment completed a multi-fragment frame")
	}
	var got []byte
	var ok bool
	for _, f := range fragsB {
		got, ok = reasm.push(f, 0)
	}
	if !ok {
		t.Fatal("frame B never completed")
	}
	if !bytes.Equal(got, wireB) {
		t.Fatalf("frame B reassembled to the wrong bytes (len %d, want %d) - likely spliced with frame A",
			len(got), len(wireB))
	}
}

func TestFrameReassemblerPassesThroughUnfragmented(t *testing.T) {
	// A body that is not one of ours must be handed on untouched rather than
	// swallowed, so a peer that sends plain frames still works.
	var reasm frameReassembler
	body := []byte{0x02, 0x00, 0x00, 0x40}
	out, ok := reasm.push(body, 0)
	if !ok {
		t.Fatal("an unfragmented body was dropped")
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("got %x, want %x", out, body)
	}
}

func TestFrameReassemblerRejectsOversizedTotal(t *testing.T) {
	// A peer claiming a huge frame must not be able to make us allocate it.
	body := []byte{fragMagic, 0, 1, 0xff, 0xff, 0xff, 0xff, 0}
	var reasm frameReassembler
	if _, ok := reasm.push(body, 0); ok {
		t.Fatal("a frame claiming 4GB was accepted")
	}
}
