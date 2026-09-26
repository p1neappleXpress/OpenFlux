package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
)

// Debug levels:
//
//	0 — off (only Infof and log.* go out)
//	1 — -d  : operational logs (connection events, state changes, errors)
//	2 — -dd : everything from 1 plus hexdumps of every packet
//
// --sensitive is a separate boolean: when on, functions that would
// otherwise redact secrets (raw session keys, secret file bytes, decrypted
// payloads, cookie jars) print them in full. It is independent of level:
// you can have -dd --sensitive or -d --sensitive.
var (
	debugLog  *log.Logger
	level     atomic.Int32
	sensitive atomic.Bool
	output    io.Writer = os.Stderr
)

// SetOutput redirects all debug and standard log output to w.
// Used by the mobile bridge to pipe logs into the app UI.
func SetOutput(w io.Writer) {
	output = w
	log.SetOutput(w)
	if debugLog != nil {
		debugLog.SetOutput(w)
	}
}

// SetLevel sets the debug level (0/1/2). Level >=1 enables Debugf;
// level >=2 additionally enables verbose hexdumps (IsVerbose).
func SetLevel(n int) {
	if n < 0 {
		n = 0
	}
	if n > 2 {
		n = 2
	}
	level.Store(int32(n))
	if n >= 1 {
		debugLog = log.New(output, "", log.LstdFlags|log.Lmicroseconds)
		log.SetOutput(output)
		log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	}
}

// Level returns the current debug level (0/1/2).
func Level() int {
	return int(level.Load())
}

// SetSensitive toggles sensitive-data output (secrets, keys, plaintext).
func SetSensitive(on bool) {
	sensitive.Store(on)
}

// Sensitive reports whether sensitive data may be logged.
func Sensitive() bool {
	return sensitive.Load()
}

// EnableDebug is a compatibility shim for older call sites; it is
// equivalent to SetLevel(2).
func EnableDebug() {
	SetLevel(2)
}

// SetDebug is a compatibility shim: on=true -> level 2, off -> level 0.
func SetDebug(on bool) {
	if on {
		SetLevel(2)
		return
	}
	SetLevel(0)
}

// IsVerbose reports whether hexdump-level output is enabled.
// Kept for compatibility with code that already calls it.
func IsVerbose() bool {
	return Level() >= 2
}

func Debugf(format string, args ...interface{}) {
	if Level() >= 1 {
		debugLog.Output(2, fmt.Sprintf(format, args...))
	}
}

// Verbosef only logs at level >= 2. Prefer wrapping hexdumps with
// "if utils.IsVerbose()" for clarity, but this is available.
func Verbosef(format string, args ...interface{}) {
	if Level() >= 2 {
		debugLog.Output(2, fmt.Sprintf(format, args...))
	}
}

// Sensitivef logs only when --sensitive is on AND level >= 1.
func Sensitivef(format string, args ...interface{}) {
	if Level() >= 1 && sensitive.Load() {
		debugLog.Output(2, fmt.Sprintf(format, args...))
	}
}

// Infof always logs, regardless of debug level. Used for user-facing status
// lines (e.g. cups room open/close) that must be visible without --debug.
func Infof(format string, args ...interface{}) {
	log.Output(2, fmt.Sprintf(format, args...))
}

// SafeGo runs fn in a new goroutine, recovering from any panic so a crash in
// one worker cannot take down the whole process (critical when this code runs
// embedded as a library inside a mobile app).
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				Debugf("[PANIC] recovered in %s: %v", name, r)
			}
		}()
		fn()
	}()
}

// Sha256Hex returns the full SHA-256 of b as lowercase hex.
func Sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Sha256Short returns the first 16 hex chars of SHA-256 of b.
func Sha256Short(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// Redact returns "" at level <1, a short hash at level 1, and the raw bytes
// at level 2 with --sensitive. Used for secrets that should not be logged
// in full unless explicitly requested.
func Redact(label string, b []byte) string {
	switch {
	case level.Load() < 1:
		return ""
	case sensitive.Load():
		return fmt.Sprintf("%s[len=%d hex=%s]", label, len(b), hex.EncodeToString(b))
	default:
		return fmt.Sprintf("%s[len=%d sha256=%s]", label, len(b), Sha256Short(b))
	}
}
