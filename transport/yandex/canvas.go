package yandex

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// CanvasVideoGenerator generates valid VP8 keyframes with encapsulated data.
type CanvasVideoGenerator struct {
	frameSeq   atomic.Uint32
	track      *webrtc.TrackLocalStaticSample
	width      int
	height     int
	fps        int
	frameQueue chan []byte
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

func NewCanvasVideoGenerator(track *webrtc.TrackLocalStaticSample, width, height, fps int) *CanvasVideoGenerator {
	return &CanvasVideoGenerator{
		track:      track,
		width:      width,
		height:     height,
		fps:        fps,
		frameQueue: make(chan []byte, 100),
		stopCh:     make(chan struct{}),
	}
}

func (g *CanvasVideoGenerator) Start() {
	g.wg.Add(1)
	go g.encodeLoop()
}

func (g *CanvasVideoGenerator) Stop() {
	close(g.stopCh)
	g.wg.Wait()
}

func (g *CanvasVideoGenerator) SendData(data []byte) error {
	select {
	case g.frameQueue <- data:
		return nil
	case <-g.stopCh:
		return nil
	default:
		utils.Debugf("[CanvasVideo] Frame queue full, dropping data")
		return nil
	}
}

func (g *CanvasVideoGenerator) encodeLoop() {
	defer g.wg.Done()
	frameDuration := time.Second / time.Duration(g.fps)
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	var currentData []byte
	for {
		select {
		case <-g.stopCh:
			return
		case data := <-g.frameQueue:
			currentData = data
		case <-ticker.C:
			frame := g.generateVP8KeyFrame(currentData)
			if err := g.track.WriteSample(media.Sample{Data: frame, Duration: frameDuration}); err != nil {
				utils.Debugf("[CanvasVideo] WriteSample error: %v", err)
			}
		}
	}
}

// generateVP8KeyFrame создаёт RTP payload формата Telemost:
//
//	VP8 Payload Descriptor (RFC 7741):
//	  byte 0: X=1, R=0, N=0, S=1, R=0, PID=0     -> 0x90
//	  byte 1: I=1, L=1, T=1, K=0, RSV=0           -> 0xe0
//	  byte 2: M=1, PictureID[14:8]                -> 0x80 | (picID>>8)
//	  byte 3: PictureID[7:0]                      -> picID & 0xff
//	  byte 4: TL0PICIDX                            -> tloIdx
//	  byte 5: TID(3) | Y(1) | KEYIDX(5)            -> 0x00
//
//	VP8 Keyframe Header (RFC 6386):
//	  3 байта: размер первой партиции (19 бит) | keyframe=0 | ver=1 | show=1
//	  3 байта: sync code 0x9D 0x01 0x2A
//	  2 байта: ширина (14 бит, LE)
//	  2 байта: высота (14 бит, LE)
//	  тело: [len:4 BE][data] — наш туннельный payload
func (g *CanvasVideoGenerator) generateVP8KeyFrame(data []byte) []byte {
	// Match the FragmentFrame capacity so the send path and the frame
	// generator share the same ceiling. See maxVP8Payload in fragment.go.
	maxPayload := maxVP8Payload
	if len(data) > maxPayload {
		data = data[:maxPayload]
	}

	payloadLen := 4 + len(data)
	body := make([]byte, payloadLen)
	binary.BigEndian.PutUint32(body[0:4], uint32(len(data)))
	if len(data) > 0 {
		copy(body[4:], data)
	}

	// PictureID и TL0PICIDX — монотонные счётчики на кадр.
	picID := uint16(g.frameSeq.Add(1) & 0x7fff) // 15 бит
	tloIdx := uint8(g.frameSeq.Load() & 0xff)

	// VP8 Payload Descriptor: X=1, S=1, I=1, L=1, T=1
	desc := []byte{
		0x90,                      // X=1, S=1, PID=0
		0xe0,                      // I=1, L=1, T=1
		byte(0x80 | (picID >> 8)), // M=1, PictureID[14:8]
		byte(picID & 0xff),        // PictureID[7:0]
		tloIdx,                    // TL0PICIDX
		0x00,                      // TID=0, Y=0, KEYIDX=0
	}

	// VP8 Keyframe Header.
	partSize := uint32(len(body) + 7)
	tag0 := byte((partSize&0x07)<<5) | 0x10 // keyframe=0, show=1
	tag1 := byte((partSize >> 3) & 0xFF)
	tag2 := byte((partSize >> 11) & 0xFF)
	w := uint16(g.width) & 0x3FFF
	h := uint16(g.height) & 0x3FFF
	frameHeader := []byte{
		tag0, tag1, tag2,
		0x9D, 0x01, 0x2A,
		byte(w & 0xFF), byte((w >> 8) & 0x3F),
		byte(h & 0xFF), byte((h >> 8) & 0x3F),
	}

	buf := make([]byte, 0, len(desc)+len(frameHeader)+len(body))
	buf = append(buf, desc...)
	buf = append(buf, frameHeader...)
	buf = append(buf, body...)
	return buf
}

// CanvasVideoDecoder extracts encapsulated data from VP8 keyframes.
type CanvasVideoDecoder struct {
	width  int
	height int
}

func NewCanvasVideoDecoder(width, height int) *CanvasVideoDecoder {
	return &CanvasVideoDecoder{width: width, height: height}
}

// GenerateKeyFrame is exposed so callers can build a single fake-VP8
// keyframe directly (e.g. to send it as one RTP packet).
// GenerateKeyFrame создаёт fake-VP8 keyframe, точно как браузер Telemost:
//
//	90 e0 80 01 14 00   VP8 Payload Descriptor (6 байт)
//	d0 4f 00            Keyframe tag
//	9d 01 2a            Sync code
//	c8 00 56 01         Width, Height
//	...body...          [len:4 BE][payload]
func (g *CanvasVideoGenerator) GenerateKeyFrame(data []byte) []byte {
	// Contract: len(data) <= maxVP8Payload. The send path enforces this with
	// FragmentFrame before it gets here. The clamp below is a last-resort
	// guard, not a working path: an oversized frame still carries a length
	// prefix advertising the full size, so the receiver would either fail to
	// decompress it or run off the end of the record. Callers that can report
	// an error should check the size themselves rather than rely on this.
	if len(data) > maxVP8Payload {
		data = data[:maxVP8Payload]
	}

	picID := uint16(g.frameSeq.Add(1) & 0x7fff)
	tloIdx := uint8(g.frameSeq.Load() & 0xff)

	// VP8 Payload Descriptor (RFC 7741)
	desc := []byte{
		0x90,                               // X=1, S=1, PID=0
		0xe0,                               // I=1, L=1, T=1
		byte(0x80 | ((picID >> 8) & 0x7f)), // M=1, PictureID[14:8]
		byte(picID & 0xff),                 // PictureID[7:0]
		tloIdx,                             // TL0PICIDX
		0x00,                               // TID=0, Y=0, KEYIDX=0
	}

	// Body = [len:4 BE][data]
	body := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(body[0:4], uint32(len(data)))
	copy(body[4:], data)

	// Keyframe tag: key_frame=0, version=0, show_frame=1, first_part_size.
	//
	// A VP8 keyframe's first partition is the 10-byte uncompressed header and
	// nothing else: 3 tag bytes, the 0x9d 0x01 0x2a start code, and 4 bytes of
	// width/height. Everything after it is the compressed frame payload, which
	// is where our tunnel bytes go. We used to declare
	// first_part_size = len(body)+7, i.e. one partition spanning the whole
	// frame. The sharing pipeline tolerated that and passed the packets
	// through, but the camera pipeline runs a real VP8 depacketizer, which
	// rejects a keyframe whose first partition exceeds 10 bytes - so a
	// VIDEO track was ingested (the SFU answered with receiver reports) yet
	// never forwarded a single data frame to subscribers.
	//
	// Note that CanvasVideoDecoder does not read first_part_size; it locates
	// the start code and the length prefix directly, so this stays compatible
	// with our own decoder.
	const partSize = 10
	tag0 := byte((partSize&0x07)<<5) | 0x10
	tag1 := byte((partSize >> 3) & 0xff)
	tag2 := byte((partSize >> 11) & 0x1f)

	w := uint16(g.width) & 0x3fff
	h := uint16(g.height) & 0x3fff

	frameHeader := []byte{
		tag0, tag1, tag2,
		0x9d, 0x01, 0x2a,
		byte(w & 0xff), byte((w >> 8) & 0x3f),
		byte(h & 0xff), byte((h >> 8) & 0x3f),
	}

	buf := make([]byte, 0, len(desc)+len(frameHeader)+len(body))
	buf = append(buf, desc...)
	buf = append(buf, frameHeader...)
	buf = append(buf, body...)
	return buf
}

func (d *CanvasVideoDecoder) DecodeFrame(frameData []byte) ([]byte, error) {
	if len(frameData) < 15 {
		return nil, nil
	}
	offset := 0
	firstByte := frameData[0]
	offset = 1
	if firstByte&0x80 != 0 && len(frameData) > offset {
		extByte := frameData[offset]
		offset++
		if extByte&0x80 != 0 && len(frameData) > offset {
			picID := frameData[offset]
			offset++
			if picID&0x80 != 0 && len(frameData) > offset {
				offset++
			}
		}
		if extByte&0x40 != 0 && len(frameData) > offset {
			offset++
		}
		if (extByte&0x20 != 0 || extByte&0x10 != 0) && len(frameData) > offset {
			offset++
		}
	}
	if len(frameData) < offset+10 {
		return nil, nil
	}
	if frameData[offset+3] == 0x9D && frameData[offset+4] == 0x01 && frameData[offset+5] == 0x2A {
		offset += 10
	}
	if len(frameData) < offset+4 {
		return nil, nil
	}
	dataLen := binary.BigEndian.Uint32(frameData[offset : offset+4])
	offset += 4
	if dataLen == 0 || int(dataLen) > (len(frameData)-offset) {
		return nil, nil
	}
	data := make([]byte, dataLen)
	copy(data, frameData[offset:offset+int(dataLen)])
	return data, nil
}

// truncHex returns at most n bytes of b as a hex string.
func truncHex(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}
