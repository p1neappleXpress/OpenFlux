package transport

import (
	"strings"
	"testing"
)

func TestCodecMismatchRecognizesTheOtherCodec(t *testing.T) {
	legacyFrame := compress([]byte("a packet"))
	batchFrame := encodeBatch([][]byte{[]byte("a packet")})

	if _, err := decodeBatch(legacyFrame); err == nil {
		t.Fatal("a legacy frame decoded as a batch")
	}
	if msg := codecMismatch(legacyFrame, false); !strings.Contains(msg, "--codec") {
		t.Fatalf("batched side got no hint for a legacy frame: %q", msg)
	}
	if msg := codecMismatch(batchFrame, true); !strings.Contains(msg, "--codec") {
		t.Fatalf("legacy side got no hint for a batched frame: %q", msg)
	}
	if msg := codecMismatch(batchFrame, false); msg != "" {
		t.Fatalf("hint for a frame of our own codec: %q", msg)
	}
	if msg := codecMismatch(legacyFrame, true); msg != "" {
		t.Fatalf("hint for a frame of our own codec: %q", msg)
	}
}
