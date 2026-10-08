package yandex

// H264 fake-keyframe generator/decoder, the H264 counterpart of
// canvas.go (VP8) and canvas_vp9.go (VP9).
//
// H264 has no single per-frame header to fake: a decoder needs an SPS
// (sequence parameters: profile, level, frame size) and a PPS (picture
// parameters: entropy coding mode, slice group count, QP) once before it can
// make sense of any slice NAL unit at all, and those two use Exp-Golomb
// (ue(v)/se(v)) bit-packed fields rather than VP9's mostly fixed-width ones.
// H264Params below builds a minimal baseline-profile SPS/PPS pair once;
// H264FrameGenerator then wraps each fragment as its own complete IDR slice
// NAL unit - "one fragment, one complete fake frame", the same shape the
// other two carriers use, not a real multi-NAL frame.
//
// RTP packetization (RFC 6184) single-NAL-unit mode has no framing of its
// own either: the RTP payload *is* the NAL unit, starting directly with its
// one-byte header - no start code (that is an Annex-B byte-stream-format
// concept, not an RTP one) and no sync pattern to scan for the way VP9's
// frame_sync_code gives DecodeFrame something to anchor on. Decoding is
// "skip the NAL header and our fixed-size slice header, the rest is data" -
// simpler than VP9, but only because there is nothing to search for if that
// fixed size is ever wrong.
//
// slice_data() - the actual macroblock bitstream - is CAVLC/CABAC entropy
// coded from its very first bit, the same way VP8's compressed partition
// and VP9's compressed header are: nothing here attempts to produce a
// decodable one, only syntactically-plausible NAL units, same as the other
// two carriers.

// h264BitWriter packs bits MSB-first, with H264's Exp-Golomb codes on top of
// the same writeBit/writeBits primitives vp9BitWriter uses.
type h264BitWriter struct {
	buf  []byte
	cur  byte
	nbit uint
}

func (w *h264BitWriter) writeBit(b uint8) {
	w.cur = w.cur<<1 | (b & 1)
	w.nbit++
	if w.nbit == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur = 0
		w.nbit = 0
	}
}

func (w *h264BitWriter) writeBits(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		w.writeBit(uint8((v >> uint(i)) & 1))
	}
}

// writeUE writes v as an Exp-Golomb unsigned code (ue(v), spec 9.1): v+1 in
// binary, preceded by (bitlen(v+1)-1) zero bits.
func (w *h264BitWriter) writeUE(v uint32) {
	v1 := v + 1
	nbits := uint(0)
	for t := v1; t != 0; t >>= 1 {
		nbits++
	}
	w.writeBits(0, nbits-1)
	w.writeBits(v1, nbits)
}

// writeSE writes v as an Exp-Golomb signed code (se(v), spec 9.1.1): the
// standard mapping 0,-1,1,-2,2,... -> 0,1,2,3,4,... fed through writeUE. Only
// se(0) is used here, which is just writeUE(0) either way, but spelling it
// out keeps the slice/PPS field list below matching the spec's own names.
func (w *h264BitWriter) writeSE(v int32) {
	var code uint32
	if v <= 0 {
		code = uint32(-2 * v)
	} else {
		code = uint32(2*v - 1)
	}
	w.writeUE(code)
}

// rbspTrailingBits ends an RBSP with the mandatory stop bit, then pads with
// zero bits out to a byte boundary (spec 7.3.2.11) - the H264 counterpart of
// vp9BitWriter's trailing_bits() padding, except the stop bit itself is
// part of the bitstream, not just alignment.
func (w *h264BitWriter) rbspTrailingBits() []byte {
	w.writeBit(1)
	for w.nbit != 0 {
		w.writeBit(0)
	}
	return w.buf
}

// h264FrameWidth/h264FrameHeight are fixed dimensions the fake SPS declares.
// Both divide evenly by 16 (the macroblock size) so no frame_cropping is
// needed - frame_cropping_flag's own fields are themselves Exp-Golomb and
// not worth the extra bit-packing for a stream nothing really decodes.
const (
	h264FrameWidth  = 1920
	h264FrameHeight = 1088
)

// buildH264SPS returns a complete SPS NAL unit (nal_unit_type 7) for
// baseline profile at h264FrameWidth x h264FrameHeight: one reference
// frame, no field coding, no VUI. profile_idc 66 (Baseline) means the
// chroma/bit-depth fields High profiles carry are skipped entirely (spec
// 7.3.2.1.1's profile_idc guard).
func buildH264SPS() []byte {
	var b h264BitWriter
	b.writeUE(0)      // seq_parameter_set_id
	b.writeUE(0)      // log2_max_frame_num_minus4
	b.writeUE(0)      // pic_order_cnt_type
	b.writeUE(0)      // log2_max_pic_order_cnt_lsb_minus4 (pic_order_cnt_type==0)
	b.writeUE(0)      // max_num_ref_frames
	b.writeBits(0, 1) // gaps_in_frame_num_value_allowed_flag
	// pic_width_in_mbs_minus1 / pic_height_in_map_units_minus1
	b.writeUE(uint32(h264FrameWidth/16 - 1))
	b.writeUE(uint32(h264FrameHeight/16 - 1))
	b.writeBits(1, 1) // frame_mbs_only_flag
	b.writeBits(1, 1) // direct_8x8_inference_flag
	b.writeBits(0, 1) // frame_cropping_flag
	b.writeBits(0, 1) // vui_parameters_present_flag
	rbsp := b.rbspTrailingBits()

	nal := make([]byte, 0, 4+len(rbsp))
	nal = append(nal, 0x67) // NAL header: ref_idc=3, type=7 (SPS)
	nal = append(nal, 0x42) // profile_idc = 66 (Baseline)
	nal = append(nal, 0x00) // constraint flags + reserved
	nal = append(nal, 40)   // level_idc = 40 (level 4.0)
	nal = append(nal, rbsp...)
	return nal
}

// buildH264PPS returns a complete PPS NAL unit (nal_unit_type 8): CAVLC
// entropy coding (simpler to byte-align after than CABAC, which would need
// its own cabac_alignment_one_bit), one slice group, no deblocking-filter
// or weighted-prediction fields set.
func buildH264PPS() []byte {
	var b h264BitWriter
	b.writeUE(0)      // pic_parameter_set_id
	b.writeUE(0)      // seq_parameter_set_id
	b.writeBits(0, 1) // entropy_coding_mode_flag (0 = CAVLC)
	b.writeBits(0, 1) // bottom_field_pic_order_in_frame_present_flag
	b.writeUE(0)      // num_slice_groups_minus1
	b.writeUE(0)      // num_ref_idx_l0_default_active_minus1
	b.writeUE(0)      // num_ref_idx_l1_default_active_minus1
	b.writeBits(0, 1) // weighted_pred_flag
	b.writeBits(0, 2) // weighted_bipred_idc
	b.writeSE(0)      // pic_init_qp_minus26
	b.writeSE(0)      // pic_init_qs_minus26
	b.writeSE(0)      // chroma_qp_index_offset
	b.writeBits(0, 1) // deblocking_filter_control_present_flag
	b.writeBits(0, 1) // constrained_intra_pred_flag
	b.writeBits(0, 1) // redundant_pic_cnt_present_flag
	rbsp := b.rbspTrailingBits()

	nal := make([]byte, 0, 1+len(rbsp))
	nal = append(nal, 0x68) // NAL header: ref_idc=3, type=8 (PPS)
	nal = append(nal, rbsp...)
	return nal
}

// H264SPS and H264PPS are built once: every participant publishing H264
// through this transport uses the exact same fixed parameters, so there is
// nothing participant-specific to compute per instance.
var (
	H264SPS = buildH264SPS()
	H264PPS = buildH264PPS()
)

// h264SliceHeader is the fixed IDR slice header every H264FrameGenerator
// frame starts with, right after the NAL header byte: first_mb_in_slice=0,
// slice_type=7 (I, "all slices in picture are I" variant - unambiguous even
// read in isolation), pic_parameter_set_id=0, frame_num=0 (4 bits, from
// SPS's log2_max_frame_num_minus4+4), idr_pic_id=ue(0), pic_order_cnt_lsb=0
// (4 bits, from log2_max_pic_order_cnt_lsb_minus4+4),
// no_output_of_prior_pics_flag=0, long_term_reference_flag=0,
// slice_qp_delta=se(0). Byte-aligned afterward (not spec-mandated for CAVLC
// mid-slice, but this is where H264FrameDecoder's fixed read offset
// assumes slice_data starts) rather than left at its natural 21-bit length,
// trading strict compliance for a fixed, simple decode offset - consistent
// with slice_data itself never being real entropy-coded macroblocks either.
var h264SliceHeader = func() []byte {
	var b h264BitWriter
	b.writeUE(0)      // first_mb_in_slice
	b.writeUE(7)      // slice_type = I (all-I)
	b.writeUE(0)      // pic_parameter_set_id
	b.writeBits(0, 4) // frame_num
	b.writeUE(0)      // idr_pic_id
	b.writeBits(0, 4) // pic_order_cnt_lsb
	b.writeBits(0, 1) // no_output_of_prior_pics_flag
	b.writeBits(0, 1) // long_term_reference_flag
	b.writeSE(0)      // slice_qp_delta
	// Byte-align without the RBSP stop bit: this header is followed by our
	// own data, not immediately by rbsp_trailing_bits().
	for b.nbit != 0 {
		b.writeBit(0)
	}
	return b.buf
}()

// h264IDRHeaderLen is the NAL header byte plus h264SliceHeader - everything
// H264FrameDecoder has to skip to reach a frame's embedded data.
var h264IDRHeaderLen = 1 + len(h264SliceHeader)

// H264FrameGenerator wraps one already-fragmented wire chunk (fragment.go)
// as a fake H264 IDR slice NAL unit.
type H264FrameGenerator struct{}

func (g *H264FrameGenerator) GenerateKeyFrame(data []byte) []byte {
	if len(data) > maxFragmentBytes {
		// Same last-resort clamp as the other carriers: FragmentFrame
		// already keeps every fragment within this size.
		data = data[:maxFragmentBytes]
	}
	buf := make([]byte, 0, h264IDRHeaderLen+len(data))
	buf = append(buf, 0x65) // NAL header: ref_idc=3, type=5 (IDR slice)
	buf = append(buf, h264SliceHeader...)
	buf = append(buf, data...)
	return buf
}

// H264FrameDecoder extracts what an H264FrameGenerator embedded.
type H264FrameDecoder struct{}

func NewH264FrameDecoder() *H264FrameDecoder { return &H264FrameDecoder{} }

func (d *H264FrameDecoder) DecodeFrame(frameData []byte) ([]byte, error) {
	if len(frameData) < h264IDRHeaderLen {
		return nil, nil
	}
	// Tolerate an SPS or PPS NAL arriving interleaved with slice NALs (both
	// are sent once up front, see telemost.go) rather than erroring: this
	// is a read-many-packets loop, and a NAL type we are not carrying data
	// in is simply not a frame to reassemble, not a malformed one.
	nalType := frameData[0] & 0x1F
	if nalType != 5 {
		return nil, nil
	}
	out := make([]byte, len(frameData)-h264IDRHeaderLen)
	copy(out, frameData[h264IDRHeaderLen:])
	return out, nil
}
