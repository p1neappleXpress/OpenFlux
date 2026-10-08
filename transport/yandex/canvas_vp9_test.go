package yandex

import (
	"bytes"
	"testing"
)

func TestVP9HeaderTailIsExactlyTenBytes(t *testing.T) {
	// The whole point of choosing these particular field values (lossless,
	// no segmentation, minimum loop filter, TileColsLog2 at its minimum) was
	// to land on a byte-aligned 80 bits with vp9BitWriter's own
	// trailing_bits() padding nothing away. If this ever drifts - e.g. the
	// frame dimensions change and tile_info()'s bit count with them - the
	// fixed offsets DecodeFrame relies on silently shift instead of failing
	// loudly, so pin the length here.
	if len(vp9HeaderTail) != 10 {
		t.Fatalf("vp9HeaderTail is %d bytes, want 10 (check tile_info()'s bit "+
			"count for vp9FrameWidth=%d)", len(vp9HeaderTail), vp9FrameWidth)
	}
}

func TestVP9FrameRoundTrip(t *testing.T) {
	gen := &VP9FrameGenerator{}
	dec := NewVP9FrameDecoder()
	sizes := []int{0, 1, 2, 100, 1392, 1400}
	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i * 13)
		}
		frame := gen.GenerateKeyFrame(data)
		got, err := dec.DecodeFrame(frame)
		if err != nil {
			t.Fatalf("size %d: DecodeFrame: %v", size, err)
		}
		want := data
		if size > maxVP8Payload {
			want = data[:maxVP8Payload]
		}
		if !bytes.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("size %d: decoded %d bytes, want %d (equal=%v)",
				size, len(got), len(want), bytes.Equal(got, want))
		}
	}
}

func TestVP9FrameHasValidSyncCodeAtFixedOffset(t *testing.T) {
	// DecodeFrame scans for the sync code rather than trusting a fixed
	// offset, but GenerateKeyFrame should still place it right after the
	// descriptor flags byte, the scalability structure and the frame-
	// marker/profile byte - anything else means one of those grew
	// unexpectedly.
	gen := &VP9FrameGenerator{}
	frame := gen.GenerateKeyFrame([]byte("hello"))
	wantOffset := 1 + len(vp9ScalabilityStructure) + 1
	if len(frame) < wantOffset+3 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}
	got := frame[wantOffset : wantOffset+3]
	want := []byte{vp9SyncCode[0], vp9SyncCode[1], vp9SyncCode[2]}
	if !bytes.Equal(got, want) {
		t.Fatalf("sync code at offset %d = %x, want %x", wantOffset, got, want)
	}
}
