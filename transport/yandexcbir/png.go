package yandexcbir

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// The PNG that carries a record block: a 1x1 image whose IDAT is the zlib
// stream of one transparent pixel, plus one ancillary chunk ("ofXT") holding
// the payload. Yandex keeps the file as uploaded and serves it back byte for
// byte, so the chunk survives the round trip.
var (
	pngSig  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	chunkID = []byte("ofXT")
	// zlib stream of a 1x1 RGBA pixel: filter 0, R=G=B=A=0.
	pixelIDAT = []byte{0x78, 0x9c, 0x62, 0x60, 0x60, 0x60, 0x00, 0x04, 0x00, 0x00, 0xff, 0xff, 0x00, 0x04, 0x00, 0x01}
)

func pngChunk(kind []byte, data []byte) []byte {
	var b bytes.Buffer
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(data)))
	b.Write(l[:])
	b.Write(kind)
	b.Write(data)
	crc := crc32.NewIEEE()
	crc.Write(kind)
	crc.Write(data)
	binary.BigEndian.PutUint32(l[:], crc.Sum32())
	b.Write(l[:])
	return b.Bytes()
}

// WrapPNG builds the image that carries payload.
func WrapPNG(payload []byte) []byte {
	var out bytes.Buffer
	out.Write(pngSig)
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:], 1) // width
	binary.BigEndian.PutUint32(ihdr[4:], 1) // height
	ihdr[8], ihdr[9] = 8, 2                 // 8-bit RGB (the IDAT below)
	out.Write(pngChunk([]byte("IHDR"), ihdr[:]))
	out.Write(pngChunk([]byte("IDAT"), pixelIDAT))
	out.Write(pngChunk(chunkID, payload))
	out.Write(pngChunk([]byte("IEND"), nil))
	return out.Bytes()
}

// UnwrapPNG returns the payload of the "ofXT" chunk.
func UnwrapPNG(b []byte) ([]byte, error) {
	if len(b) > 2 && b[len(b)-2] == '\r' && b[len(b)-1] == '\n' {
		b = b[:len(b)-2] // the storage appends a CRLF
	}
	if !bytes.HasPrefix(b, pngSig) {
		return nil, errors.New("yandexcbir: not a PNG")
	}
	b = b[len(pngSig):]
	for len(b) >= 12 {
		n := int(binary.BigEndian.Uint32(b[:4]))
		if n < 0 || len(b) < 12+n {
			return nil, errors.New("yandexcbir: truncated PNG")
		}
		kind := b[4:8]
		data := b[8 : 8+n]
		if bytes.Equal(kind, chunkID) {
			return data, nil
		}
		b = b[12+n:]
	}
	return nil, errors.New("yandexcbir: no payload chunk")
}

// Deflate compresses a record block for the chunk; records are zlib'd so a
// page or a JSON reply shrinks several times over.
func Deflate(p []byte) []byte {
	var b bytes.Buffer
	// Best compression: the upload is the scarce resource, not the CPU.
	w, err := zlib.NewWriterLevel(&b, zlib.BestCompression)
	if err != nil {
		w = zlib.NewWriter(&b)
	}
	_, _ = w.Write(p)
	_ = w.Close()
	return b.Bytes()
}

// Inflate undoes Deflate.
func Inflate(p []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
