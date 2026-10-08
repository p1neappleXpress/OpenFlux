package yandex

import (
	"encoding/binary"
)

// VP9 fake-keyframe generator/decoder.
//
// Same trick as CanvasVideoGenerator/CanvasVideoDecoder (canvas.go) - wrap our
// tunnel bytes inside something that reads as a valid VP9 keyframe - but VP9's
// uncompressed header is bit-packed (RFC-defined field widths that don't fall
// on byte boundaries) rather than VP8's byte-aligned 10-byte header, so it
// needs an actual bit writer instead of a handful of struct literals.
//
// This exists to test whether the SFU's handling of our fake video differs by
// declared codec: a run on VP8 showed the same fragment index of a multi-
// fragment frame getting dropped on essentially every occurrence, regardless
// of burst size, timeout, or congestion control - none of which should matter
// if forwarding is genuinely codec-blind. serverHello explicitly advertises
// PUBLISH_VP9_ENABLED and a higher per-layer framerate ceiling for VP9 than
// VP8, so it is at least plausible the forwarding path treats them
// differently.

// vp9BitWriter packs bits MSB-first into a byte slice, matching the VP9
// uncompressed header's own bit order (section 6.2 of the VP9 bitstream spec).
type vp9BitWriter struct {
	buf  []byte
	cur  byte
	nbit uint
}

func (w *vp9BitWriter) writeBit(b uint8) {
	w.cur = w.cur<<1 | (b & 1)
	w.nbit++
	if w.nbit == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur = 0
		w.nbit = 0
	}
}

// writeBits writes the low n bits of v, most-significant first.
func (w *vp9BitWriter) writeBits(v uint32, n uint) {
	for i := int(n) - 1; i >= 0; i-- {
		w.writeBit(uint8((v >> uint(i)) & 1))
	}
}

// bytes flushes any partial byte (zero-padded, same as the spec's
// trailing_bits()) and returns the packed buffer.
func (w *vp9BitWriter) bytes() []byte {
	for w.nbit != 0 {
		w.writeBit(0)
	}
	return w.buf
}

// vp9SyncCode is VP9's fixed frame sync code (spec 6.2, frame_sync_code()).
var vp9SyncCode = [3]byte{0x49, 0x83, 0x42}

// vp9FrameWidth/vp9FrameHeight are fixed dimensions the fake header declares.
// tile_info()'s bit count depends on frame width (through Sb64Cols), so
// vp9HeaderTailBytes below is only correct for this exact width - anyone
// changing it has to recheck that math.
const (
	vp9FrameWidth  = 1920
	vp9FrameHeight = 1080
)

// vp9ScalabilityStructure is the RTP descriptor's SS field (VP9 payload spec
// section 4.2.2), present whenever V=1 in the descriptor's flags byte:
//
//	N_S=0 (1 spatial layer, this field holds layer count minus 1)
//	Y=1   (per-layer resolution follows)
//	G=0   (no picture-group description)
//	then, because N_S+1=1 and Y=1: that one layer's WIDTH and HEIGHT,
//	16 bits each.
//
// A single layer declared this plainly is about as minimal as a real VP9
// encoder's SS ever gets, which is the point: anything a real publisher
// would omit here is a difference the SFU has no reason to key behavior
// off of, but the SS block's mere presence turned out to be exactly what it
// does key behavior off of.
var vp9ScalabilityStructure = []byte{
	0b000_1_0_000, // N_S=0, Y=1, G=0, reserved=0
	byte(vp9FrameWidth >> 8), byte(vp9FrameWidth & 0xFF),
	byte(vp9FrameHeight >> 8), byte(vp9FrameHeight & 0xFF),
}

// vp9CompressedHeaderFillerLen is how many filler bytes follow the
// uncompressed header, matching the header_size_in_bytes field written into
// it. VP9's compressed header carries entropy-coded probability updates that
// nothing here ever decodes, so one zero byte is enough to make
// header_size_in_bytes non-degenerate without claiming more than is sent.
const vp9CompressedHeaderFillerLen = 1

// buildVP9UncompressedHeader returns the fixed-content uncompressed header
// (minus the sync code, which the caller prepends) for a profile-0 key frame
// at vp9FrameWidth x vp9FrameHeight: color_config, frame_size, render_size,
// refresh_frame_context/frame_parallel_decoding_mode/frame_context_idx,
// loop_filter_params, quantization_params, segmentation_params, tile_info and
// header_size_in_bytes, every field at its simplest legal value. See
// telemost's VP9 generator comment for the bit-count derivation (it comes out
// to exactly 80 bits = 10 bytes at this width, which is why no padding logic
// is needed beyond vp9BitWriter's own trailing_bits()).
func buildVP9UncompressedHeaderTail() []byte {
	var w vp9BitWriter
	// color_config(): color_space (CS_BT_601) + color_range (studio swing).
	// Profile 0 means BitDepth is implied 8 and subsampling is implied 4:2:0,
	// so neither is read here.
	w.writeBits(2, 3) // color_space = CS_BT_601
	w.writeBits(0, 1) // color_range = 0
	// frame_size()
	w.writeBits(vp9FrameWidth-1, 16)
	w.writeBits(vp9FrameHeight-1, 16)
	// render_size(): render dimensions equal the frame size.
	w.writeBits(0, 1)
	// Gated by error_resilient_mode==0 in the byte the caller writes first.
	w.writeBits(0, 1) // refresh_frame_context
	w.writeBits(1, 1) // frame_parallel_decoding_mode
	w.writeBits(0, 2) // frame_context_idx
	// loop_filter_params(): level 0, sharpness 0, deltas disabled.
	w.writeBits(0, 6)
	w.writeBits(0, 3)
	w.writeBits(0, 1)
	// quantization_params(): base_q_idx 0 (lossless), no coded deltas.
	w.writeBits(0, 8)
	w.writeBits(0, 1)
	w.writeBits(0, 1)
	w.writeBits(0, 1)
	// segmentation_params(): disabled.
	w.writeBits(0, 1)
	// tile_info(): minLog2TileCols(0) < maxLog2TileCols(2) at this width, so
	// one increment_tile_cols_log2_flag bit is read; 0 keeps TileColsLog2 at
	// its minimum, which also means tile_rows_log2 is not read at all.
	w.writeBits(0, 1)
	// header_size_in_bytes: length of the compressed header that follows.
	w.writeBits(vp9CompressedHeaderFillerLen, 16)
	return w.bytes()
}

var vp9HeaderTail = buildVP9UncompressedHeaderTail()

// vp9HeaderLen is the full fake bitstream header length: the byte0 flags,
// the 3-byte sync code, the bit-packed tail, and the compressed-header
// filler - everything before our own embedded [len:4][data].
var vp9HeaderLen = 1 + len(vp9SyncCode) + len(vp9HeaderTail) + vp9CompressedHeaderFillerLen

// VP9FrameGenerator builds fake VP9 keyframes carrying tunnel data, the VP9
// counterpart of CanvasVideoGenerator.
type VP9FrameGenerator struct{}

// GenerateKeyFrame wraps data in a VP9 RTP payload descriptor plus a fake
// profile-0 key frame bitstream header, with data embedded as
// [len:4 BE][data] where VP9's compressed frame payload would be.
func (g *VP9FrameGenerator) GenerateKeyFrame(data []byte) []byte {
	if len(data) > maxVP8Payload {
		// Same cap as the VP8 path (see canvas.go for why this is a
		// last-resort guard, not a working path): FragmentFrame already
		// keeps every fragment within this size.
		data = data[:maxVP8Payload]
	}

	buf := make([]byte, 0, len(vp9ScalabilityStructure)+1+vp9HeaderLen+4+len(data))
	// RTP payload descriptor (I=0,P=0,L=0,F=0,B=1,E=1,V=1,Z=0): this packet
	// is a whole, independent, non-inter-predicted VP9 frame by itself - the
	// same "one fragment, one complete fake frame" shape canvas.go uses for
	// VP8, not a real multi-packet VP9 frame split by B/E. V=1 means the
	// scalability structure right below is present.
	//
	// Every one of our "frames" is a VP9 keyframe by construction, so every
	// one carries SS rather than tracking which one is logically "first":
	// the spec requires it on keyframes, and a real encoder's own stream
	// only has one to begin with. Omitting it is what measurably kept the
	// SFU from ever telling a second subscriber about this track at all -
	// the slot still showed a Camera label (a lighter check than real
	// promotion), but no renegotiated subscriberSdpOffer ever followed, on
	// a run where the identical timing worked immediately under VP8.
	buf = append(buf, 0b0000_1110)
	buf = append(buf, vp9ScalabilityStructure...)
	// frame_marker=10, profile=00, show_existing_frame=0, frame_type=0
	// (KEY_FRAME), show_frame=1, error_resilient_mode=0.
	buf = append(buf, 0b1000_0010)
	buf = append(buf, vp9SyncCode[0], vp9SyncCode[1], vp9SyncCode[2])
	buf = append(buf, vp9HeaderTail...)
	for i := 0; i < vp9CompressedHeaderFillerLen; i++ {
		buf = append(buf, 0)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	buf = append(buf, lenBuf[:]...)
	buf = append(buf, data...)
	return buf
}

// VP9FrameDecoder extracts the data a VP9FrameGenerator embedded.
type VP9FrameDecoder struct{}

func NewVP9FrameDecoder() *VP9FrameDecoder { return &VP9FrameDecoder{} }

// DecodeFrame locates our sync code and reads the [len:4][data] that follows
// the fixed-size tail after it, the VP9 counterpart of
// CanvasVideoDecoder.DecodeFrame. Scanning for the sync code rather than
// assuming the descriptor is always exactly 1 byte keeps this tolerant of a
// PictureID or other extension the descriptor might carry, same as the VP8
// decoder locating its own sync code rather than trusting a fixed offset.
func (d *VP9FrameDecoder) DecodeFrame(frameData []byte) ([]byte, error) {
	tail := len(vp9SyncCode) + len(vp9HeaderTail) + vp9CompressedHeaderFillerLen
	for i := 0; i+len(vp9SyncCode) <= len(frameData); i++ {
		if frameData[i] != vp9SyncCode[0] || frameData[i+1] != vp9SyncCode[1] || frameData[i+2] != vp9SyncCode[2] {
			continue
		}
		offset := i + tail
		if len(frameData) < offset+4 {
			return nil, nil
		}
		dataLen := binary.BigEndian.Uint32(frameData[offset : offset+4])
		offset += 4
		if dataLen == 0 || int(dataLen) > len(frameData)-offset {
			return nil, nil
		}
		out := make([]byte, dataLen)
		copy(out, frameData[offset:offset+int(dataLen)])
		return out, nil
	}
	return nil, nil
}
