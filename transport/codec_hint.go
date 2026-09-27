package transport

import (
	"sync/atomic"
	"time"

	"openflux/utils"
)

// A client and an exit started with different codecs cannot read each
// other's frames, and each side used to log only "unknown batch version
// 0x00" or pass garbage on (#77). codecMismatch recognizes the other codec's
// frames so the log can say what to change.

// codecMismatch explains a frame this side could not decode, or returns ""
// when it does not look like the other codec's. legacyReceiver is true on a
// --codec=legacy side.
func codecMismatch(frame []byte, legacyReceiver bool) string {
	if len(frame) == 0 {
		return ""
	}
	if legacyReceiver {
		if frame[0] == batchFormatVersion {
			return "the peer sends batched frames but this side runs --codec=legacy: use the same --codec on both sides"
		}
		return ""
	}
	if frame[0] == 0x00 || frame[0] == CompressionMarker {
		return "the peer sends legacy frames (--codec=legacy) but this side runs --codec=batched: use the same --codec on both sides"
	}
	return ""
}

var lastCodecHint atomic.Int64

// hintCodecMismatch logs codecMismatch's explanation at most once a minute.
func hintCodecMismatch(frame []byte, legacyReceiver bool) {
	msg := codecMismatch(frame, legacyReceiver)
	if msg == "" {
		return
	}
	now := time.Now().UnixNano()
	last := lastCodecHint.Load()
	if now-last < int64(time.Minute) || !lastCodecHint.CompareAndSwap(last, now) {
		return
	}
	utils.Infof("[CODEC] %s", msg)
}
