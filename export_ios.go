//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/tunnel"
	"universal-bypass-tool/utils"
)

// ---- log ring buffer piped into the app UI ----

type ringLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *ringLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, strings.TrimRight(string(p), "\n"))
	if len(r.lines) > 1000 {
		r.lines = r.lines[len(r.lines)-1000:]
	}
	return len(p), nil
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

// ---- running client state ----

var (
	stateMu sync.Mutex
	running bool
	socks   *socks5.SOCKS5Server
	trans   transport.Transport
)

func init() {
	// Route log output into the ring buffer, but leave verbose logging OFF by
	// default (production). The app can turn it on via OpenFluxSetDebug; the
	// per-packet logging is expensive.
	utils.SetOutput(logbuf)

	// The client's local (mobile) DNS may be poisoned for censored hosts
	// (observed: ifconfig.me -> 240.0.1.72, a reserved address). Resolve names
	// over DNS-over-TLS instead so DialTCP gets real IPs to hand the exit node.
	net.DefaultResolver = &net.Resolver{
		PreferGo:     true,
		StrictErrors: false,
		Dial:         dialSecureDNS,
	}
}

// dotServer is a DNS-over-TLS endpoint (addr:853 + TLS SNI).
type dotServer struct {
	addr string
	sni  string
}

func defaultDoTServers() []dotServer {
	return []dotServer{
		{"77.88.8.8:853", "common.dot.dns.yandex.net"}, // Yandex, reachable in-region
		{"8.8.8.8:853", "dns.google"},
		{"1.1.1.1:853", "cloudflare-dns.com"},
	}
}

var (
	dotMu      sync.RWMutex
	dotServers = defaultDoTServers()
)

// getDoTServers returns a snapshot of the configured DoT resolvers. Callers must
// not mutate the result; it is safe to read concurrently with OpenFluxSetDoTResolver.
func getDoTServers() []dotServer {
	dotMu.RLock()
	defer dotMu.RUnlock()
	out := make([]dotServer, len(dotServers))
	copy(out, dotServers)
	return out
}

// OpenFluxSetDoTResolver overrides the DNS-over-TLS upstreams used to resolve
// names (defeating local DNS poisoning). spec is a ";"-separated list of
// "addr[:port]@sni" entries, e.g. "1.1.1.1@cloudflare-dns.com". Port defaults to
// 853 and SNI defaults to the host if omitted. An empty spec restores the
// built-in defaults (Yandex/Google/Cloudflare). Call before starting a tunnel.
//
//export OpenFluxSetDoTResolver
func OpenFluxSetDoTResolver(spec *C.char) {
	s := strings.TrimSpace(C.GoString(spec))
	dotMu.Lock()
	defer dotMu.Unlock()
	if s == "" {
		dotServers = defaultDoTServers()
		utils.Debugf("[DNS] resolver reset to defaults")
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
		utils.Debugf("[DNS] resolver set: %v", servers)
	}
}

// dialSecureDNS opens a DNS-over-TLS connection for net.Resolver, trying the
// configured servers in order.
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

// Return codes for OpenFluxStartClient.
const (
	startOK             = 0
	startAlreadyRunning = 1
	startBadTransport   = 2
	startTransportError = 3
	startAddrInUse      = 4 // SOCKS5 port could not be bound (e.g. already in use)
	startPanic          = 5
)

// OpenFluxStartClient starts the SOCKS5 client tunnel.
//
// transportType: "yandex" or "oneme".
// url:           Yandex.Docs document URL (yandex transport).
// socksAddr:     e.g. "127.0.0.1:1080".
// maxToken/maxUid: credentials for the "oneme" (MAX) transport; pass "" for yandex.
//
// Returns 0 on success, non-zero on error (see start* codes; details go to the log).
//
//export OpenFluxStartClient
func OpenFluxStartClient(transportType, url, socksAddr, maxToken, maxUid *C.char) (rc C.int) {
	tt := C.GoString(transportType)
	docURL := C.GoString(url)
	addr := C.GoString(socksAddr)
	mToken := C.GoString(maxToken)
	mUid := C.GoString(maxUid)

	// Never let a panic unwind into the C/Swift caller and crash the app.
	defer func() {
		if r := recover(); r != nil {
			utils.Debugf("[BRIDGE] Recovered from panic in start: %v", r)
			rc = C.int(startPanic)
		}
	}()

	stateMu.Lock()
	defer stateMu.Unlock()
	if running {
		utils.Debugf("[BRIDGE] Start ignored: already running")
		return C.int(startAlreadyRunning)
	}

	// Bind the SOCKS5 port up front so "address already in use" is reported
	// cleanly to the UI instead of failing later in a background goroutine.
	probe, err := net.Listen("tcp", addr)
	if err != nil {
		utils.Debugf("[BRIDGE] Cannot bind %s: %v", addr, err)
		return C.int(startAddrInUse)
	}
	probe.Close()

	config := transport.DefaultConfig()
	var t transport.Transport
	switch tt {
	case "yandex", "":
		t = transport.NewCompressedTransport(yandex.NewYandexDocsTransport(docURL, config))
	case "oneme":
		uidint, _ := strconv.ParseInt(mUid, 10, 64)
		t = transport.NewCompressedTransport(oneme.NewOneMeTransport(false, mToken, uidint, config))
	default:
		utils.Debugf("[BRIDGE] Unknown transport type: %s", tt)
		return C.int(startBadTransport)
	}

	if err := t.Start(); err != nil {
		utils.Debugf("[BRIDGE] Failed to start transport: %v", err)
		return C.int(startTransportError)
	}

	tun := tunnel.NewTCPTunnel(t, false)
	srv := socks5.NewSOCKS5Server(addr, tun)
	if err := srv.Bind(); err != nil {
		utils.Debugf("[BRIDGE] Cannot bind %s: %v", addr, err)
		t.Stop()
		return C.int(startAddrInUse)
	}

	trans = t
	socks = srv
	running = true

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[BRIDGE] Recovered from panic in SOCKS5 loop: %v", r)
			}
		}()
		utils.Debugf("[BRIDGE] Client running (SOCKS5 on %s, transport %s)", addr, tt)
		if err := srv.Start(); err != nil {
			utils.Debugf("[BRIDGE] SOCKS5 server stopped: %v", err)
		}
	}()

	return C.int(startOK)
}

// OpenFluxStop stops the running client (transport + SOCKS5 listener).
//
//export OpenFluxStop
func OpenFluxStop() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if !running {
		return
	}
	if socks != nil {
		socks.Close()
	}
	if trans != nil {
		trans.Stop()
	}
	socks = nil
	trans = nil
	running = false
	utils.Debugf("[BRIDGE] Stopped")
}

// OpenFluxIsRunning returns 1 if the client is running, 0 otherwise.
//
//export OpenFluxIsRunning
func OpenFluxIsRunning() C.int {
	stateMu.Lock()
	defer stateMu.Unlock()
	if running {
		return C.int(1)
	}
	return C.int(0)
}

// OpenFluxIsConnected returns 1 if the transport reports a live connection.
//
//export OpenFluxIsConnected
func OpenFluxIsConnected() C.int {
	stateMu.Lock()
	defer stateMu.Unlock()
	if trans != nil && trans.IsConnected() {
		return C.int(1)
	}
	return C.int(0)
}

// OpenFluxStatsJSON returns a small JSON blob with transport stats.
// The returned string is C-allocated; free it with OpenFluxFreeString.
//
//export OpenFluxStatsJSON
func OpenFluxStatsJSON() *C.char {
	stateMu.Lock()
	defer stateMu.Unlock()
	if trans == nil {
		return C.CString(`{"running":false}`)
	}
	s := trans.Stats()
	js := fmt.Sprintf(
		`{"running":%t,"connected":%t,"bytesSent":%d,"bytesReceived":%d,"packetsSent":%d,"packetsRecv":%d,"reconnects":%d,"uptimeSec":%d}`,
		running, s.Connected, s.BytesSent, s.BytesReceived, s.PacketsSent, s.PacketsRecv, s.Reconnects,
		int64(s.Uptime/time.Second),
	)
	return C.CString(js)
}

// OpenFluxReadLog drains buffered log lines (newline-separated).
// The returned string is C-allocated; free it with OpenFluxFreeString.
//
//export OpenFluxReadLog
func OpenFluxReadLog() *C.char {
	return C.CString(logbuf.drain())
}

// OpenFluxFreeString frees a string returned by this library.
//
//export OpenFluxFreeString
func OpenFluxFreeString(s *C.char) {
	C.free(unsafe.Pointer(s))
}

// OpenFluxSetDebug toggles verbose (per-packet) logging at runtime. Off by
// default; enabling it costs CPU, so only turn it on while debugging.
//
//export OpenFluxSetDebug
func OpenFluxSetDebug(on C.int) {
	utils.SetDebug(on != 0)
}
