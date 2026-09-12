package transport

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// round-trips a set of packets through encode/decode and asserts equality.
func assertBatchRoundTrip(t *testing.T, pkts [][]byte) {
	t.Helper()
	wire := encodeBatch(pkts)
	got, err := decodeBatch(wire)
	if err != nil {
		t.Fatalf("decodeBatch error: %v", err)
	}
	if len(got) != len(pkts) {
		t.Fatalf("packet count = %d, want %d", len(got), len(pkts))
	}
	for i := range pkts {
		if !bytes.Equal(got[i], pkts[i]) {
			t.Fatalf("packet %d mismatch:\n got=%v\nwant=%v", i, got[i], pkts[i])
		}
	}
}

func TestBatchRoundTripSinglePacket(t *testing.T) {
	assertBatchRoundTrip(t, [][]byte{[]byte("hello world")})
}

func TestBatchRoundTripManyPackets(t *testing.T) {
	pkts := [][]byte{
		[]byte("first"),
		{0x00, 0x01, 0x02, 0xfe, 0xff},
		[]byte("a much longer third packet with some repetition repetition repetition"),
		{},
		[]byte("last"),
	}
	assertBatchRoundTrip(t, pkts)
}

func TestBatchRoundTripBinaryMTUSized(t *testing.T) {
	// Realistic full-MTU TCP payloads with arbitrary binary content.
	mk := func(fill byte) []byte {
		b := make([]byte, 1460)
		for i := range b {
			b[i] = fill ^ byte(i)
		}
		return b
	}
	assertBatchRoundTrip(t, [][]byte{mk(0x11), mk(0x22), mk(0x33)})
}

func TestBatchRoundTripEmptyList(t *testing.T) {
	got, err := decodeBatch(encodeBatch(nil))
	if err != nil {
		t.Fatalf("decodeBatch error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 packets, got %d", len(got))
	}
}

// A compressible batch must actually use the compressed path and shrink,
// proving zstd is wired in (not just a raw passthrough).
func TestBatchCompressesRepetitiveData(t *testing.T) {
	pkt := bytes.Repeat([]byte("ABCDEFGH"), 512) // 4096 bytes, highly compressible
	pkts := [][]byte{pkt, pkt, pkt}
	wire := encodeBatch(pkts)

	rawFramedLen := 0
	for _, p := range pkts {
		rawFramedLen += 2 + len(p)
	}
	if len(wire) >= rawFramedLen {
		t.Fatalf("expected compression to shrink %d framed bytes, got wire len %d", rawFramedLen, len(wire))
	}
	assertBatchRoundTrip(t, pkts)
}

// Incompressible data must fall back to the uncompressed path and still
// round-trip (zstd would otherwise inflate it).
func TestBatchIncompressibleFallsBackAndRoundTrips(t *testing.T) {
	pkt := make([]byte, 1200)
	if _, err := rand.Read(pkt); err != nil {
		t.Fatalf("rand: %v", err)
	}
	assertBatchRoundTrip(t, [][]byte{pkt})
}

func TestDecodeBatchRejectsUnknownVersion(t *testing.T) {
	if _, err := decodeBatch([]byte{0xFF, 0x00}); err == nil {
		t.Fatal("expected error for unknown version byte")
	}
}

func TestDecodeBatchRejectsTruncatedLengthPrefix(t *testing.T) {
	// valid header, flags=0 (uncompressed), then a length prefix claiming 10
	// bytes but only 2 present.
	bad := []byte{batchFormatVersion, 0x00, 0x00, 0x0A, 0x01, 0x02}
	if _, err := decodeBatch(bad); err == nil {
		t.Fatal("expected error for truncated length-prefixed packet")
	}
}

func TestDecodeBatchRejectsShortFrame(t *testing.T) {
	if _, err := decodeBatch([]byte{batchFormatVersion}); err == nil {
		t.Fatal("expected error for frame shorter than header")
	}
}
