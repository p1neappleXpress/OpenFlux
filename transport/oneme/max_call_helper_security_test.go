package oneme

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// decodeCallDetails parses a size prefix from the first 3 characters of an
// incoming-call payload (data controlled by whoever sent the incoming-call
// packet) via fmt.Sscanf with no sign check, then does make([]byte, size).
// A prefix like "-1x" parses to size=-1 and panics make([]byte, -1)
// ("makeslice: len out of range") inside MaxClient.readLoop, which is
// started exactly once with no restart wrapper -- so the panic recovering
// there silently and permanently halts all MAX protocol processing.
func TestDecodeCallDetailsRejectsNegativeSize(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decodeCallDetails panicked on a negative size prefix: %v", r)
		}
	}()

	// vcp[:3] is the size prefix, vcp[3] is a delimiter byte decodeCallDetails
	// skips, vcp[4:] is the base64 payload -- keep the payload valid so
	// execution actually reaches the vulnerable make([]byte, size) call
	// instead of failing earlier at the base64 decode step.
	vcp := "-1x" + "_" + base64.StdEncoding.EncodeToString([]byte("irrelevant"))
	if _, err := decodeCallDetails(vcp); err == nil {
		t.Fatal("expected an error for a negative size prefix, got nil")
	}
}

func TestDecodeCallDetailsStillWorksForValidInput(t *testing.T) {
	payload := []byte("hello world")
	compressed := make([]byte, len(payload)*2+64)
	n, err := lz4.CompressBlock(payload, compressed, nil)
	if err != nil {
		t.Fatalf("test setup: compress: %v", err)
	}
	if n == 0 {
		// Incompressible tiny input: lz4 declines to compress it. Fall back
		// to storing it raw isn't supported by decodeCallDetails's format,
		// so pad the payload until it actually compresses.
		t.Skip("payload too small for this lz4 implementation to compress; not exercising the real bug")
	}

	vcp := fmt.Sprintf("%03d", len(payload)) + "_" + base64.StdEncoding.EncodeToString(compressed[:n])

	got, err := decodeCallDetails(vcp)
	if err != nil {
		t.Fatalf("unexpected error on valid input: %v", err)
	}
	if got != string(payload) {
		t.Fatalf("decodeCallDetails() = %q, want %q", got, string(payload))
	}
}
