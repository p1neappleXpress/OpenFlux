package transport

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestDecompressRejectsBomb(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(CompressionMarker)
	w := lz4.NewWriter(&buf)
	w.Write(make([]byte, maxDecompressed+4096))
	w.Close()
	if buf.Len() > 1<<16 {
		t.Fatalf("bomb is not small: %d bytes", buf.Len())
	}

	if _, err := decompress(buf.Bytes()); !errors.Is(err, errDecompressedTooLarge) {
		t.Fatalf("decompress = %v, want errDecompressedTooLarge", err)
	}
}

func TestDecompressRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("openflux packet "), 100)
	out, err := decompress(compress(data))
	if err != nil || !bytes.Equal(out, data) {
		t.Fatalf("round trip: err=%v equal=%v", err, bytes.Equal(out, data))
	}
}
