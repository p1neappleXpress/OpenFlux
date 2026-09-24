package transport

import "testing"

// The legacy (--codec=legacy) LZ4 path had no cap on decompressed output
// size, unlike the batched zstd path (framing.go), which explicitly bounds
// decoder memory to guard against a malformed/hostile frame. A small,
// highly-compressible LZ4 block can expand to an arbitrarily large output;
// this reproduces that with a payload just past the intended cap and checks
// decompress() refuses it instead of allocating it.
func TestDecompressRejectsOversizedOutput(t *testing.T) {
	oversized := make([]byte, maxDecompressedSize+1) // all zero bytes: compresses to a few hundred bytes
	compressed := compress(oversized)
	if compressed[0] != CompressionMarker {
		t.Fatalf("test payload wasn't actually compressed (marker=0x%02x), can't exercise the LZ4 path", compressed[0])
	}

	_, err := decompress(compressed)
	if err == nil {
		t.Fatalf("decompress() accepted a %d-byte output with no size cap", len(oversized))
	}
}

func TestDecompressAllowsUnderCapOutput(t *testing.T) {
	data := make([]byte, maxDecompressedSize-1)
	for i := range data {
		data[i] = byte(i)
	}
	compressed := compress(data)

	out, err := decompress(compressed)
	if err != nil {
		t.Fatalf("decompress() rejected a legitimate under-cap payload: %v", err)
	}
	if len(out) != len(data) {
		t.Fatalf("decompress() returned %d bytes, want %d", len(out), len(data))
	}
}
