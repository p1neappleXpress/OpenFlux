package utils

import (
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sync"
)

var (
	debugLog *log.Logger
	verbose  bool
	output   io.Writer = os.Stderr
	logMu    sync.RWMutex
)

var logURLs = regexp.MustCompile(`(?i)(?:https?|wss?)://[^\s"<>]+`)

// RedactURLs also covers error strings produced by net/http and WebSocket
// dialers, whose URL paths/query parameters often carry document credentials.
func RedactURLs(s string) string { return logURLs.ReplaceAllString(s, "[redacted URL]") }

// SetOutput redirects all debug and standard log output to w.
// Used by the mobile bridge to pipe logs into the app UI.
func SetOutput(w io.Writer) {
	logMu.Lock()
	defer logMu.Unlock()
	output = w
	log.SetOutput(w)
	if debugLog != nil {
		debugLog.SetOutput(w)
	}
}

func EnableDebug() {
	logMu.Lock()
	defer logMu.Unlock()
	verbose = true
	debugLog = log.New(output, "", log.LstdFlags|log.Lmicroseconds)
	log.SetOutput(output)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
}

func Debugf(format string, args ...interface{}) {
	logMu.RLock()
	defer logMu.RUnlock()
	if verbose {
		debugLog.Output(2, RedactURLs(fmt.Sprintf(format, args...)))
	}
}

// SetDebug toggles verbose logging at runtime (off = Debugf becomes a no-op).
func SetDebug(on bool) {
	if on {
		EnableDebug()
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	verbose = false
}

func IsVerbose() bool {
	logMu.RLock()
	defer logMu.RUnlock()
	return verbose
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
