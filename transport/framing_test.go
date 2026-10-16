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

func TestDecodeBatchRejectsUnknownFlags(t *testing.T) {
	// v2 header, unknown flag bit set.
	if _, err := decodeBatch([]byte{batchFormatVersion, 0x80}); err == nil {
		t.Fatal("expected unknown flags error")
	}
}

func TestCompressGateBacksOffOnIncompressibleAndRecovers(t *testing.T) {
	rnd := func() [][]byte {
		b := make([]byte, 1800)
		_, _ = cryptoRandRead(b)
		return [][]byte{b[:1200], b[1200:]}
	}
	var g compressGate
	tried := 0
	for i := 0; i < 400; i++ {
		before := g.skip
		frame := encodeBatchGated(rnd(), &g)
		if before == 0 {
			tried++
		}
		pk, err := decodeBatch(frame)
		if err != nil || len(pk) != 2 {
			t.Fatalf("round trip failed: %v (%d packets)", err, len(pk))
		}
	}
	if tried > 25 {
		t.Fatalf("incompressible traffic was offered to zstd %d times in 400 batches", tried)
	}
	// Compressible traffic is picked up again within one backoff window.
	zeros := [][]byte{make([]byte, 1200), make([]byte, 1200)}
	compressed := false
	for i := 0; i <= gateMaxSkip+1 && !compressed; i++ {
		frame := encodeBatchGated(zeros, &g)
		compressed = frame[1]&batchFlagZstd != 0
	}
	if !compressed {
		t.Fatal("compression never resumed for compressible traffic")
	}
	for i := 0; i < 5; i++ {
		if frame := encodeBatchGated(zeros, &g); frame[1]&batchFlagZstd == 0 {
			t.Fatal("compression stopped again for compressible traffic")
		}
	}
}

func TestDecodeBatchPacketsAreIndependent(t *testing.T) {
	a, b := []byte("first packet"), []byte("second packet")
	pk, err := decodeBatch(encodeBatchGated([][]byte{a, b}, &compressGate{skip: 5}))
	if err != nil || len(pk) != 2 {
		t.Fatalf("decode: %v", err)
	}
	// Appending to one packet must not overwrite its neighbour.
	pk[0] = append(pk[0], []byte("XXXXXXXXXXXXXXXX")...)
	if string(pk[1]) != string(b) {
		t.Fatalf("second packet corrupted by append to the first: %q", pk[1])
	}
}

func TestCheapBytesGateSkipsHeaderOnlyRedundancy(t *testing.T) {
	// Six packets that are random except for a repeated 78-byte envelope, as
	// a TLS-carrying tunnel looks: zstd shrinks it by a few percent only.
	env := make([]byte, 78)
	_, _ = cryptoRandRead(env)
	var pkts [][]byte
	for i := 0; i < 6; i++ {
		p := make([]byte, 1500)
		_, _ = cryptoRandRead(p)
		copy(p, env)
		pkts = append(pkts, p)
	}
	plain := encodeBatchGated(pkts, &compressGate{})
	if plain[1]&batchFlagZstd == 0 {
		t.Skip("envelope redundancy did not reach the default threshold on this data")
	}
	cheap := encodeBatchGated(pkts, &compressGate{cheapBytes: true})
	if cheap[1]&batchFlagZstd != 0 {
		t.Fatal("a cheap-bytes carrier compressed a batch that shrinks by only a few percent")
	}
	// Both decode to the same packets.
	a, err1 := decodeBatch(plain)
	b, err2 := decodeBatch(cheap)
	if err1 != nil || err2 != nil || len(a) != len(b) {
		t.Fatalf("decode mismatch: %v %v", err1, err2)
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			t.Fatalf("packet %d differs", i)
		}
	}
	// Really compressible data is still compressed on a cheap carrier.
	zeros := [][]byte{make([]byte, 1400), make([]byte, 1400)}
	if encodeBatchGated(zeros, &compressGate{cheapBytes: true})[1]&batchFlagZstd == 0 {
		t.Fatal("compressible data was not compressed on a cheap-bytes carrier")
	}
}

func TestBufPoolClassesAndReuse(t *testing.T) {
	for _, n := range []int{1, 100, 2048, 2049, 16000, 16384, 65078, 65536} {
		b := getBuf(n)
		if len(b) != n {
			t.Fatalf("getBuf(%d) has length %d", n, len(b))
		}
		if cap(b) < n {
			t.Fatalf("getBuf(%d) has capacity %d", n, cap(b))
		}
		putBuf(b)
	}
	// Foreign buffers are not pooled.
	putBuf(make([]byte, 3000))
	putBuf(nil)
	if b := getBuf(70000); len(b) != 70000 {
		t.Fatal("oversized buffer")
	}
}
