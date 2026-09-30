//go:build ios

// Command ios is the iOS app's C library (go build -buildmode=c-archive
// -tags ios, see build_ios.sh). It exposes the C API the app calls, built
// on package mobile: the same Session, context rule, codec and link
// handling as Android, so an iOS client behaves like every other one. The
// app links this core as a submodule instead of carrying a copy.
//
// Everything here is iOS glue: C strings, the log buffer the app drains,
// DNS-over-TLS against poisoned resolvers, the packet flow of the Network
// Extension. Nothing decides how the tunnel works.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
	mobile "openflux-mobile"
)

func main() {}

// ---- log buffer the app drains ----

type ringLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *ringLog) Write(p []byte) (int, error) {
	r.add(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (r *ringLog) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > 1000 {
		r.lines = r.lines[len(r.lines)-1000:]
	}
}

func (r *ringLog) drain() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) == 0 {
		return ""
	}
	out := strings.Join(r.lines, "\n")
	r.lines = r.lines[:0]
	return out
}

var logbuf = &ringLog{}

func init() {
	// The core's operational lines reach the app through package mobile's
	// log (ReadLogs); the standard log (plain log.Printf from the core)
	// goes to the ring buffer. The debug logger itself writes nowhere
	// else, so no line shows up twice.
	utils.SetOutput(io.Discard)
	log.SetOutput(logbuf)
	log.SetFlags(log.Ltime)
	setupCrashCapture()
	// The phone's own resolver may be poisoned for the hosts the carriers
	// use; resolve over DNS-over-TLS instead.
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: dialSecureDNS}
}

// crashFile keeps the fatal-crash sink open for the process lifetime; the
// previous run's crash is shown in the log on the next launch.
var crashFile *os.File

func setupCrashCapture() {
	path := filepath.Join(os.TempDir(), "oflux-crash.log")
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		logbuf.add("===== PREVIOUS CRASH (Go traceback) =====")
		logbuf.add(string(b))
		logbuf.add("===== END PREVIOUS CRASH =====")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	crashFile = f
	debug.SetCrashOutput(f, debug.CrashOptions{})
}

// OpenFluxReadLog drains buffered log lines (newline-separated). Free the
// result with OpenFluxFreeString.
//
//export OpenFluxReadLog
func OpenFluxReadLog() *C.char {
	parts := make([]string, 0, 2)
	if s := logbuf.drain(); s != "" {
		parts = append(parts, s)
	}
	if s := mobile.ReadLogs(); s != "" {
		parts = append(parts, s)
	}
	return C.CString(strings.Join(parts, "\n"))
}

//export OpenFluxFreeString
func OpenFluxFreeString(s *C.char) {
	C.free(unsafe.Pointer(s))
}

// OpenFluxSetDebug turns operational logs on (level 2) or off.
//
//export OpenFluxSetDebug
func OpenFluxSetDebug(on C.int) {
	level := utils.LevelOff
	if on != 0 {
		level = utils.LevelDebug
	}
	OpenFluxSetDebugLevel(C.int(level))
}

// OpenFluxSetDebugLevel sets the core's log level: 0 off, 1 packets, 2
// operational logs, 3 hexdumps (the CLI's --debug=N). Takes effect now and
// on the next start.
//
//export OpenFluxSetDebugLevel
func OpenFluxSetDebugLevel(n C.int) {
	mobile.SetDebugLevel(int(n))
	utils.SetLevel(int(n))
}

// ---- DNS over TLS ----

type dotServer struct {
	addr string
	sni  string
}

func defaultDoTServers() []dotServer {
	return []dotServer{
		{"77.88.8.8:853", "common.dot.dns.yandex.net"},
		{"8.8.8.8:853", "dns.google"},
		{"1.1.1.1:853", "cloudflare-dns.com"},
	}
}

var (
	dotMu      sync.RWMutex
	dotServers = defaultDoTServers()
)

func getDoTServers() []dotServer {
	dotMu.RLock()
	defer dotMu.RUnlock()
	return append([]dotServer(nil), dotServers...)
}

// OpenFluxSetDoTResolver sets the DNS-over-TLS upstreams: ";"-separated
// "addr[:port]@sni" entries ("1.1.1.1@cloudflare-dns.com"); port defaults
// to 853, SNI to the host. "" restores the defaults. Call before starting.
//
//export OpenFluxSetDoTResolver
func OpenFluxSetDoTResolver(spec *C.char) {
	s := strings.TrimSpace(C.GoString(spec))
	dotMu.Lock()
	defer dotMu.Unlock()
	if s == "" {
		dotServers = defaultDoTServers()
		return
	}
	var servers []dotServer
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, sni := part, ""
		if i := strings.LastIndex(part, "@"); i >= 0 {
			addr, sni = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		if !strings.Contains(addr, ":") {
			addr += ":853"
		}
		if sni == "" {
			if host, _, err := net.SplitHostPort(addr); err == nil {
				sni = host
			} else {
				sni = addr
			}
		}
		servers = append(servers, dotServer{addr: addr, sni: sni})
	}
	if len(servers) > 0 {
		dotServers = servers
		log.Printf("[DNS] DNS-over-TLS resolvers: %v", servers)
	}
}

func dialSecureDNS(ctx context.Context, _, _ string) (net.Conn, error) {
	var lastErr error
	for _, s := range getDoTServers() {
		d := tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 6 * time.Second},
			Config:    &tls.Config{ServerName: s.sni, MinVersion: tls.VersionTLS12},
		}
		conn, err := d.DialContext(ctx, "tcp", s.addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		utils.Debugf("[DNS] DoT %s failed: %v", s.addr, err)
	}
	return nil, lastErr
}

// ---- profile settings shared by every start ----

// Start return codes.
const (
	startOK             = 0
	startAlreadyRunning = 1
	startBadTransport   = 2
	startTransportError = 3
	startAddrInUse      = 4
	startPanic          = 5
	startBadEncryption  = 6
)

var settings struct {
	mu     sync.Mutex
	secret string
	codec  string
}

// OpenFluxSetEncryption sets (or clears, with "") the shared secret for the
// next start. With a secret the client runs the Session, falling back to
// the classic layering for a node that does not answer it; without one only
// classic, unencrypted, is possible.
//
//export OpenFluxSetEncryption
func OpenFluxSetEncryption(secret *C.char) {
	s := ""
	if secret != nil {
		s = strings.TrimSpace(C.GoString(secret))
	}
	settings.mu.Lock()
	settings.secret = s
	settings.mu.Unlock()
	if s == "" {
		log.Printf("[BRIDGE] encryption: off")
	} else {
		log.Printf("[BRIDGE] encryption: secret set (%d chars)", utils.SecretChars(s))
	}
}

// OpenFluxSetCodec sets the classic framing preferred for the next start,
// "batched" (default) or "legacy" (an openflux:// link's codec). The other
// one is accepted too and tried when the node does not answer.
//
//export OpenFluxSetCodec
func OpenFluxSetCodec(codec *C.char) {
	c := strings.TrimSpace(C.GoString(codec))
	settings.mu.Lock()
	settings.codec = c
	settings.mu.Unlock()
}

func currentSettings() (secret, codec string) {
	settings.mu.Lock()
	defer settings.mu.Unlock()
	codec = settings.codec
	if codec == "" {
		codec = transport.CodecBatched
	}
	return settings.secret, codec
}

// normalizeType maps the app's transport names onto the core's.
func normalizeType(t string) string {
	switch t {
	case "", "yandex":
		return "yandex"
	case "volga":
		return "vyandex"
	case "mail":
		return "mailru"
	case "max":
		return "oneme"
	}
	return t
}

// classicSpecs turns the app's (type, value) into a Session spec list for
// comma-separated document lists (several documents of one type): one
// carrier each, named as every client names them (type, type-2, ...).
func classicSpecs(typ, value string) []map[string]interface{} {
	var specs []map[string]interface{}
	for i, u := range strings.Split(value, ",") {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		name := typ
		if i > 0 {
			name = fmt.Sprintf("%s-%d", typ, i+1)
		}
		specs = append(specs, map[string]interface{}{
			"name": name, "type": typ, "url": u, "priority": 100 - i,
		})
	}
	return specs
}

// startCode maps package mobile's error text onto a start code.
func startCode(msg string) C.int {
	switch {
	case msg == "":
		return startOK
	case strings.Contains(msg, "занят"):
		return startAddrInUse
	case strings.Contains(strings.ToLower(msg), "ключ"):
		return startBadEncryption
	case strings.Contains(msg, "неизвестный тип"):
		return startBadTransport
	}
	return startTransportError
}

func recoverStart(rc *C.int, where string) {
	if r := recover(); r != nil {
		log.Printf("[BRIDGE] recovered from a panic in %s: %v", where, r)
		*rc = C.int(startPanic)
	}
}

func jsonString(v interface{}) *C.char {
	b, err := json.Marshal(v)
	if err != nil {
		return C.CString(`{"error":"marshal failed"}`)
	}
	return C.CString(string(b))
}

// OpenFluxMode says how traffic currently goes: "session", "classic" (the
// node does not answer the Session handshake: an older or classic node),
// or "". Free with OpenFluxFreeString.
//
//export OpenFluxMode
func OpenFluxMode() *C.char {
	return C.CString(mobile.ConnectionMode())
}

// OpenFluxActiveTransport names the carrier traffic goes through now
// ("direct", "vyandex", ...), following failover. Free with
// OpenFluxFreeString.
//
//export OpenFluxActiveTransport
func OpenFluxActiveTransport() *C.char {
	return C.CString(mobile.CurrentTransport())
}
