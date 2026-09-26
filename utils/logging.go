package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
)

// Debug levels. Each level prints everything the ones below it do.
//
//	0  (default)   off: only Infof and log.* status lines
//	1  -d          packet movement: one line per IPv4 packet crossing the
//	               tunnel, "-> 52 bytes - UDP 10.10.10.2:53000 -> 8.8.8.8:53 ..."
//	2  -dd         operational logs: sessions, carriers, handshakes, crypto,
//	               control messages, errors
//	3  -ddd        hexdumps of packets and frames
//
// --sensitive is separate from the level: it adds key material and cookie
// jars to whatever the level prints.
const (
	LevelOff     = 0
	LevelPackets = 1
	LevelDebug   = 2
	LevelHexdump = 3
)

// The logger is created once and never replaced: the mobile bridges change
// the level and the output on every connect while goroutines of the
// previous connection may still be logging.
var (
	level     atomic.Int32
	sensitive atomic.Bool
	output    = &swapWriter{}
	debugLog  = log.New(output, "", log.LstdFlags|log.Lmicroseconds)
	logSinkMu sync.RWMutex
	logSink   func(string)
)

func init() {
	output.Set(os.Stderr)
}

// swapWriter lets SetOutput redirect a logger that is already in use.
type swapWriter struct {
	w atomic.Pointer[io.Writer]
}

func (s *swapWriter) Set(w io.Writer) { s.w.Store(&w) }

func (s *swapWriter) Write(p []byte) (int, error) { return (*s.w.Load()).Write(p) }

// SetOutput redirects debug and standard log output to w.
// Used by the iOS bridge to keep logs in its ring buffer.
func SetOutput(w io.Writer) {
	output.Set(w)
	log.SetOutput(w)
}

// SetLogSink mirrors every debug message (packet lines included) to an
// embedding application, e.g. the Android log screen. nil stops it.
func SetLogSink(sink func(string)) {
	logSinkMu.Lock()
	logSink = sink
	logSinkMu.Unlock()
}

// SetLevel sets the debug level, clamped to LevelOff..LevelHexdump.
func SetLevel(n int) {
	n = min(max(n, LevelOff), LevelHexdump)
	level.Store(int32(n))
	if n > LevelOff {
		log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	}
}

// Level returns the current debug level.
func Level() int {
	return int(level.Load())
}

// EnableDebug turns on operational logs (LevelDebug), what debug meant
// before there were levels.
func EnableDebug() {
	SetLevel(LevelDebug)
}

// SetDebug turns operational logs on (LevelDebug) or all debug output off.
func SetDebug(on bool) {
	if on {
		SetLevel(LevelDebug)
		return
	}
	SetLevel(LevelOff)
}

// IsVerbose reports whether hexdumps are enabled (LevelHexdump).
func IsVerbose() bool {
	return Level() >= LevelHexdump
}

// SetSensitive toggles logging of key material and cookie jars.
func SetSensitive(on bool) {
	sensitive.Store(on)
}

// Sensitive reports whether key material and cookie jars may be logged.
func Sensitive() bool {
	return sensitive.Load()
}

// Verbosef logs at hexdump level (LevelHexdump / -ddd). Kept for call sites
// that emit their own detailed dumps instead of network.LogPacket.
func Verbosef(format string, args ...interface{}) {
	if Level() >= LevelHexdump {
		emit(fmt.Sprintf(format, args...))
	}
}

// Sensitivef logs only when --sensitive is on and operational logs are
// enabled (LevelDebug and up). For secrets that must never appear otherwise.
func Sensitivef(format string, args ...interface{}) {
	if Level() >= LevelDebug && sensitive.Load() {
		emit(fmt.Sprintf(format, args...))
	}
}

// Redact renders a secret for logs: "" when debug is off, a short SHA-256 at
// LevelDebug, and the raw bytes as hex only when --sensitive is on.
func Redact(label string, b []byte) string {
	switch {
	case Level() < LevelDebug:
		return ""
	case sensitive.Load():
		return fmt.Sprintf("%s[len=%d hex=%s]", label, len(b), hex.EncodeToString(b))
	default:
		return fmt.Sprintf("%s[len=%d sha256=%s]", label, len(b), Sha256Short(b))
	}
}

// Packetf logs a packet-movement line (LevelPackets and up).
func Packetf(format string, args ...interface{}) {
	if Level() >= LevelPackets {
		emit(fmt.Sprintf(format, args...))
	}
}

// Debugf logs an operational message (LevelDebug and up).
func Debugf(format string, args ...interface{}) {
	if Level() >= LevelDebug {
		emit(fmt.Sprintf(format, args...))
	}
}

func emit(message string) {
	debugLog.Output(3, message)

	logSinkMu.RLock()
	sink := logSink
	logSinkMu.RUnlock()
	if sink != nil {
		sink(message)
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
