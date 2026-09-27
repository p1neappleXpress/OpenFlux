package transport

import (
	"bytes"
	"errors"
	"io"

	"github.com/pierrec/lz4/v4"
)

const (
	MinCompressSize   = 200
	CompressionMarker = 0x1F

	// maxDecompressed bounds what one legacy frame may inflate to, the same
	// cap the batched zstd codec uses. A frame carries one packet, so honest
	// peers stay far below it; without a cap a few bytes of crafted LZ4 from
	// anyone on the document could inflate into gigabytes.
	maxDecompressed = 8 << 20
)

var errDecompressedTooLarge = errors.New("lz4: decompressed frame exceeds limit")

type CompressedTransport struct {
	Transport
}

func NewCompressedTransport(inner Transport) Transport {
	return &CompressedTransport{Transport: inner}
}

func (c *CompressedTransport) Send(data []byte) error {
	compressed := compress(data)
	return c.Transport.Send(compressed)
}

func (c *CompressedTransport) Receive(callback func([]byte)) {
	c.Transport.Receive(func(data []byte) {
		decompressed, err := decompress(data)
		if errors.Is(err, errDecompressedTooLarge) {
			return
		}
		if err != nil {
			hintCodecMismatch(data, true)
			callback(data) // fallback
			return
		}
		callback(decompressed)
	})
}

func compress(data []byte) []byte {
	if len(data) <= MinCompressSize {
		out := make([]byte, 1, len(data)+1)
		out[0] = 0x00
		out = append(out, data...)
		return out
	}

	var buf bytes.Buffer
	buf.WriteByte(CompressionMarker)

	w := lz4.NewWriter(&buf)
	w.Write(data)
	w.Close()

	if buf.Len() >= len(data)+1 {
		out := make([]byte, 1, len(data)+1)
		out[0] = 0x00
		out = append(out, data...)
		return out
	}

	return buf.Bytes()
}

func decompress(data []byte) ([]byte, error) {
	if len(data) < 1 {
		return data, nil
	}

	if data[0] == 0x00 {
		return data[1:], nil
	}

	r := lz4.NewReader(bytes.NewReader(data[1:]))
	out, err := io.ReadAll(io.LimitReader(r, maxDecompressed+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDecompressed {
		return nil, errDecompressedTooLarge
	}
	return out, nil
}
