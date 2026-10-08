package yandex

// Fake-Opus carrier.
//
// Opus's RTP payload format (RFC 7587) has no descriptor at all: the RTP
// payload *is* the Opus packet, with nothing else around it. Opus's own
// framing (the TOC byte) only needs frame_count_code = 0 - "one frame,
// length implicit from the payload's own length" - for everything after
// that one byte to be free-form: no declared length to get right, no sync
// code to scan for, unlike the VP8/VP9 paths (canvas.go, canvas_vp9.go).
// Decoding is just "drop the first byte".
//
// This exists to test whether the SFU's handling differs between media
// kinds, not just between video codecs: video goes through per-participant
// slot assignment, layout and (for VP9) scalability-structure checks before
// a second subscriber is even told the track exists at all (see telemost.go
// and canvas_vp9.go) - audio has none of that, so if the same "first
// fragment or two arrives, the rest of that burst does not" pattern shows
// up here too, the cause is not anything video-pipeline-specific.

// opusTOC is a fixed, minimal valid Opus TOC byte: configuration 0
// (SILK-only narrowband, 10ms frames), mono, frame_count_code 0.
const opusTOC = 0x00

// AudioFrameGenerator wraps one already-fragmented wire chunk (see
// fragment.go - the same FragmentFrame/frameReassembler pair the video
// carriers use) as a fake Opus packet.
type AudioFrameGenerator struct{}

func (g *AudioFrameGenerator) GenerateFrame(data []byte) []byte {
	if len(data) > maxFragmentBytes {
		// Matches the video carriers' own last-resort clamp (canvas.go,
		// canvas_vp9.go): FragmentFrame already keeps every fragment within
		// this size, so this is a guard, not a working path.
		data = data[:maxFragmentBytes]
	}
	buf := make([]byte, 0, 1+len(data))
	buf = append(buf, opusTOC)
	buf = append(buf, data...)
	return buf
}

// AudioFrameDecoder extracts what an AudioFrameGenerator embedded.
type AudioFrameDecoder struct{}

func NewAudioFrameDecoder() *AudioFrameDecoder { return &AudioFrameDecoder{} }

func (d *AudioFrameDecoder) DecodeFrame(frameData []byte) ([]byte, error) {
	if len(frameData) < 1 {
		return nil, nil
	}
	out := make([]byte, len(frameData)-1)
	copy(out, frameData[1:])
	return out, nil
}
