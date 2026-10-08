package yandex

import (
	"bytes"
	"testing"
)

// h264BitReader is a tiny Exp-Golomb reader, used only by this test file to
// check buildH264SPS's bit-packing against a decoder independent of the
// writer that produced it - the same reason canvas_vp9_test.go pins
// vp9HeaderTail's exact byte length rather than only round-tripping through
// GenerateKeyFrame/DecodeFrame, which would not catch the writer and an
// equally-wrong reader agreeing with each other.
type h264BitReader struct {
	buf []byte
	pos int // bit position
}

func (r *h264BitReader) readBit() uint32 {
	byteIdx := r.pos / 8
	bitIdx := 7 - uint(r.pos%8)
	r.pos++
	if byteIdx >= len(r.buf) {
		return 0
	}
	return uint32((r.buf[byteIdx] >> bitIdx) & 1)
}

func (r *h264BitReader) readBits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = v<<1 | r.readBit()
	}
	return v
}

func (r *h264BitReader) readUE() uint32 {
	zeros := 0
	for r.readBit() == 0 {
		zeros++
		if zeros > 32 {
			return 0
		}
	}
	if zeros == 0 {
		return 0
	}
	return (1 << uint(zeros)) - 1 + r.readBits(zeros)
}

func TestH264SPSRoundTripsThroughAnIndependentReader(t *testing.T) {
	sps := buildH264SPS()
	if sps[0] != 0x67 {
		t.Fatalf("SPS NAL header = %#x, want 0x67", sps[0])
	}
	if sps[1] != 0x42 {
		t.Fatalf("profile_idc byte = %#x, want 0x42 (Baseline)", sps[1])
	}
	if sps[3] != 40 {
		t.Fatalf("level_idc byte = %d, want 40", sps[3])
	}

	r := &h264BitReader{buf: sps[4:]} // past the 4 fixed bytes
	if v := r.readUE(); v != 0 {
		t.Fatalf("seq_parameter_set_id = %d, want 0", v)
	}
	if v := r.readUE(); v != 0 {
		t.Fatalf("log2_max_frame_num_minus4 = %d, want 0", v)
	}
	if v := r.readUE(); v != 0 {
		t.Fatalf("pic_order_cnt_type = %d, want 0", v)
	}
	if v := r.readUE(); v != 0 {
		t.Fatalf("log2_max_pic_order_cnt_lsb_minus4 = %d, want 0", v)
	}
	if v := r.readUE(); v != 0 {
		t.Fatalf("max_num_ref_frames = %d, want 0", v)
	}
	if v := r.readBits(1); v != 0 {
		t.Fatalf("gaps_in_frame_num_value_allowed_flag = %d, want 0", v)
	}
	gotWidthMbs := r.readUE()
	gotHeightMapUnits := r.readUE()
	wantWidth := (gotWidthMbs + 1) * 16
	wantHeight := (gotHeightMapUnits + 1) * 16
	if wantWidth != h264FrameWidth {
		t.Fatalf("decoded width = %d, want %d", wantWidth, h264FrameWidth)
	}
	if wantHeight != h264FrameHeight {
		t.Fatalf("decoded height = %d, want %d", wantHeight, h264FrameHeight)
	}
	if v := r.readBits(1); v != 1 {
		t.Fatalf("frame_mbs_only_flag = %d, want 1", v)
	}
}

func TestH264PPSStartsWithExpectedHeader(t *testing.T) {
	pps := buildH264PPS()
	if pps[0] != 0x68 {
		t.Fatalf("PPS NAL header = %#x, want 0x68", pps[0])
	}
	r := &h264BitReader{buf: pps[1:]}
	if v := r.readUE(); v != 0 {
		t.Fatalf("pic_parameter_set_id = %d, want 0", v)
	}
	if v := r.readUE(); v != 0 {
		t.Fatalf("seq_parameter_set_id = %d, want 0", v)
	}
	if v := r.readBits(1); v != 0 {
		t.Fatalf("entropy_coding_mode_flag = %d, want 0 (CAVLC)", v)
	}
}

func TestH264FrameRoundTrip(t *testing.T) {
	gen := &H264FrameGenerator{}
	dec := NewH264FrameDecoder()
	sizes := []int{0, 1, 2, 100, 1392, 1400}
	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i * 17)
		}
		frame := gen.GenerateKeyFrame(data)
		if frame[0]&0x1F != 5 {
			t.Fatalf("size %d: NAL type = %d, want 5 (IDR slice)", size, frame[0]&0x1F)
		}
		got, err := dec.DecodeFrame(frame)
		if err != nil {
			t.Fatalf("size %d: DecodeFrame: %v", size, err)
		}
		want := data
		if size > maxFragmentBytes {
			want = data[:maxFragmentBytes]
		}
		if !bytes.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("size %d: decoded %d bytes, want %d (equal=%v)",
				size, len(got), len(want), bytes.Equal(got, want))
		}
	}
}

func TestH264FrameDecoderSkipsNonSliceNALs(t *testing.T) {
	dec := NewH264FrameDecoder()
	for _, nal := range [][]byte{H264SPS, H264PPS} {
		got, err := dec.DecodeFrame(nal)
		if err != nil {
			t.Fatalf("DecodeFrame on NAL type %d: %v", nal[0]&0x1F, err)
		}
		if got != nil {
			t.Fatalf("DecodeFrame on NAL type %d returned %d bytes, want nil", nal[0]&0x1F, len(got))
		}
	}
}
