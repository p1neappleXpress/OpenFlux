//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/p1neappleXpress/OpenFlux/network"
	"github.com/p1neappleXpress/OpenFlux/utils"
	mobile "openflux-mobile"
)

// Packet-tunnel (NEPacketTunnelProvider) mode: pure L3 forwarding.
//
// The device gets tunnel address 10.10.10.2, which every exit answers to,
// so its IPv4 packets go over the tunnel as they are (package mobile's
// Send/Read, the Android VPN path); no TCP stack runs in the extension.
// DNS (UDP 53) is answered locally over DNS-over-TLS; other UDP goes over
// the tunnel when enabled, otherwise is refused with ICMP so apps fall back
// from QUIC to TCP at once.

var pt struct {
	mu     sync.Mutex
	on     bool
	outQ   chan []byte
	ctx    context.Context
	cancel context.CancelFunc
}

// tunnelUDP: forward non-DNS UDP over the tunnel (the Session negotiates
// whether the exit takes UDP; a classic exit may not).
var tunnelUDP atomic.Bool

//export OpenFluxSetTunnelUDP
func OpenFluxSetTunnelUDP(on C.int) { tunnelUDP.Store(on != 0) }

// extensionLimits keeps the Network Extension under its 50 MB cap: a Go
// heap limit with headroom for what lives outside it (zstd, TLS, WebSocket
// buffers, the runtime), and the phone resource profile of the carriers.
func extensionLimits() {
	debug.SetMemoryLimit(24 << 20)
	debug.SetGCPercent(80)
	mobile.SetLowMemory(true)
	// SetMemoryLimit bounds only the Go heap; jetsam kills by RSS. Under
	// load the freed heap has to go back to the OS or RSS keeps growing
	// (a speed test ended the extension, a captcha hand-off failed):
	// FreeOSMemory (MADV_DONTNEED) on a slow tick trades a little
	// throughput for the extension staying alive. One ticker for the
	// process, however many times the tunnel starts.
	freeOSMemory.Do(func() {
		go func() {
			for range time.Tick(freeOSMemoryEvery) {
				debug.FreeOSMemory()
			}
		}()
	})
}

var freeOSMemory sync.Once

const freeOSMemoryEvery = 8 * time.Second

// OpenFluxStartPacketTunnel starts the packet tunnel for a classic profile
// (see OpenFluxStartClient for the arguments; the secret comes from
// OpenFluxSetEncryption). Returns a start code.
//
//export OpenFluxStartPacketTunnel
func OpenFluxStartPacketTunnel(transportType, url, maxToken, maxUid *C.char) (rc C.int) {
	defer recoverStart(&rc, "OpenFluxStartPacketTunnel")
	typ := normalizeType(C.GoString(transportType))
	value := strings.TrimSpace(C.GoString(url))
	secret, codec := currentSettings()
	if typ == "direct" && secret == "" {
		log.Printf("[PKT] direct needs an encryption key")
		return startBadEncryption
	}
	return startPacketTunnel(func() string {
		if specs := classicSpecs(typ, value); len(specs) > 1 {
			if secret == "" {
				return "для нескольких документов нужен ключ шифрования"
			}
			b, _ := json.Marshal(specs)
			return mobile.StartSession(string(b), secret)
		}
		return mobile.Start(typ, value, secret, codec, C.GoString(maxToken), C.GoString(maxUid))
	})
}

// OpenFluxStartSessionPacketTunnel starts the packet tunnel for a Session
// profile (specsJSON as for OpenFluxStartSession).
//
//export OpenFluxStartSessionPacketTunnel
func OpenFluxStartSessionPacketTunnel(specsJSON, secret *C.char) (rc C.int) {
	defer recoverStart(&rc, "OpenFluxStartSessionPacketTunnel")
	specs := C.GoString(specsJSON)
	key := strings.TrimSpace(C.GoString(secret))
	return startPacketTunnel(func() string { return mobile.StartSession(specs, key) })
}

func startPacketTunnel(start func() string) C.int {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if pt.on {
		return startAlreadyRunning
	}
	extensionLimits()
	if msg := start(); msg != "" {
		log.Printf("[PKT] start: %s", msg)
		return startCode(msg)
	}
	// 2048 packets (about 3 MB at full size) absorb a download burst
	// without drops, within the extension's budget.
	pt.outQ = make(chan []byte, 2048)
	pt.ctx, pt.cancel = context.WithCancel(context.Background())
	pt.on = true
	go pumpFromTunnel(pt.ctx, pt.outQ)
	log.Printf("[PKT] packet tunnel started (mode %q)", mobile.ConnectionMode())
	return startOK
}

// pumpFromTunnel moves packets the exit sent into the device queue.
func pumpFromTunnel(ctx context.Context, outQ chan []byte) {
	for ctx.Err() == nil {
		p := mobile.ReadTimeout(250)
		if p == nil {
			continue
		}
		select {
		case outQ <- p:
		default: // device side not keeping up: drop, TCP retransmits
		}
	}
}

// OpenFluxTunWritePacket takes one packet from the device.
//
//export OpenFluxTunWritePacket
func OpenFluxTunWritePacket(buf *C.char, length C.int) {
	defer func() { _ = recover() }()
	if buf == nil || length < 20 {
		return
	}
	pt.mu.Lock()
	on, outQ := pt.on, pt.outQ
	pt.mu.Unlock()
	if !on {
		return
	}
	pkt := C.GoBytes(unsafe.Pointer(buf), length)
	if pkt[0]>>4 != 4 {
		return
	}
	switch pkt[9] {
	case 6: // TCP
		send(pkt)
	case 17: // UDP
		ihl := int(pkt[0]&0x0f) * 4
		if len(pkt) < ihl+8 {
			return
		}
		switch {
		case binary.BigEndian.Uint16(pkt[ihl+2:ihl+4]) == 53:
			select {
			case dnsSem <- struct{}{}:
				go func() { defer func() { <-dnsSem }(); handleDNSPacket(pkt, outQ) }()
			default: // too many in flight: the client retries
			}
		case tunnelUDP.Load():
			send(pkt)
		default:
			sendICMPPortUnreachable(pkt, outQ)
		}
	}
}

func send(pkt []byte) {
	if msg := mobile.Send(pkt); msg != "" && utils.Throttled("pkt.send", 10*time.Second) {
		utils.Debugf("[PKT] send: %s", msg)
	}
}

// dnsSem caps concurrent DNS-over-TLS resolutions.
var dnsSem = make(chan struct{}, 16)

// OpenFluxTunReadPacket blocks for the next packet to the device and
// returns its length (0 once the tunnel stops).
//
//export OpenFluxTunReadPacket
func OpenFluxTunReadPacket(buf *C.char, max C.int) C.int {
	pt.mu.Lock()
	outQ, ctx := pt.outQ, pt.ctx
	pt.mu.Unlock()
	if outQ == nil || ctx == nil {
		return 0
	}
	select {
	case data := <-outQ:
		n := min(len(data), int(max))
		dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(max))
		copy(dst[:n], data[:n])
		return C.int(n)
	case <-ctx.Done():
		return 0
	}
}

// OpenFluxPacketTunnelConnected is 1 while the tunnel reaches the exit;
// the provider polls it to drive reasserting across carrier reconnects.
//
//export OpenFluxPacketTunnelConnected
func OpenFluxPacketTunnelConnected() C.int {
	pt.mu.Lock()
	on := pt.on
	pt.mu.Unlock()
	if on && mobile.IsConnected() {
		return 1
	}
	return 0
}

//export OpenFluxStopPacketTunnel
func OpenFluxStopPacketTunnel() {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if !pt.on {
		return
	}
	pt.cancel()
	mobile.Stop()
	pt.outQ, pt.ctx, pt.cancel = nil, nil, nil
	pt.on = false
	log.Printf("[PKT] packet tunnel stopped")
}

// OpenFluxSetGeositeDirect loads the newline-separated domain suffixes whose
// traffic goes direct (GeoSite split tunneling).
//
//export OpenFluxSetGeositeDirect
func OpenFluxSetGeositeDirect(list *C.char) {
	setGeositeDirect(C.GoString(list))
}

// OpenFluxDrainDirectIPs copies pending direct IPs (newline-joined) into
// buf up to max bytes and returns the byte count; the provider adds them to
// excludedRoutes. What does not fit stays queued.
//
//export OpenFluxDrainDirectIPs
func OpenFluxDrainDirectIPs(buf *C.char, max C.int) C.int {
	s := drainDirectIPsCapped(int(max))
	if s == "" {
		return 0
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(max))
	return C.int(copy(dst, s))
}

// sendICMPPortUnreachable answers a UDP datagram that is not forwarded, so
// the sender falls back to TCP at once instead of timing out.
func sendICMPPortUnreachable(orig []byte, outQ chan []byte) {
	ihl := int(orig[0]&0x0f) * 4
	if len(orig) < ihl+8 {
		return
	}
	quote := orig[:ihl+8]
	icmp := make([]byte, 8+len(quote))
	icmp[0], icmp[1] = 3, 3 // destination unreachable, port unreachable
	copy(icmp[8:], quote)
	ck := network.IPChecksum(icmp)
	icmp[2], icmp[3] = byte(ck>>8), byte(ck)

	total := 20 + len(icmp)
	ip := make([]byte, total)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(total))
	ip[8], ip[9] = 64, 1
	copy(ip[12:16], orig[16:20])
	copy(ip[16:20], orig[12:16])
	ck2 := network.IPChecksum(ip[:20])
	ip[10], ip[11] = byte(ck2>>8), byte(ck2)
	copy(ip[20:], icmp)
	select {
	case outQ <- ip:
	default:
	}
}

// handleDNSPacket answers a device DNS query over DNS-over-TLS.
func handleDNSPacket(req []byte, outQ chan []byte) {
	defer func() { _ = recover() }()
	ihl := int(req[0]&0x0f) * 4
	if len(req) < ihl+8 {
		return
	}
	query := req[ihl+8:]
	if len(query) == 0 {
		return
	}
	answer, err := dnsOverTLS(query)
	if err != nil || len(answer) == 0 {
		if utils.Throttled("pkt.dns", 30*time.Second) {
			log.Printf("[DNS] DNS-over-TLS failed: %v", err)
		}
		return
	}
	geositeNoteAnswer(query, answer)

	udpLen := 8 + len(answer)
	total := ihl + udpLen
	resp := make([]byte, total)
	resp[0], resp[1] = req[0], req[1]
	binary.BigEndian.PutUint16(resp[2:4], uint16(total))
	resp[8], resp[9] = 64, 17
	copy(resp[12:16], req[16:20])
	copy(resp[16:20], req[12:16])
	ipck := network.IPChecksum(resp[:20])
	resp[10], resp[11] = byte(ipck>>8), byte(ipck)
	copy(resp[ihl:ihl+2], req[ihl+2:ihl+4])
	copy(resp[ihl+2:ihl+4], req[ihl:ihl+2])
	binary.BigEndian.PutUint16(resp[ihl+4:ihl+6], uint16(udpLen))
	copy(resp[ihl+8:], answer)
	select {
	case outQ <- resp:
	default:
	}
}

func dnsOverTLS(query []byte) ([]byte, error) {
	var lastErr error
	for _, s := range getDoTServers() {
		ans, err := dotQueryOne(s, query)
		if err == nil {
			return ans, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func dotQueryOne(s dotServer, query []byte) ([]byte, error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 6 * time.Second},
		Config:    &tls.Config{ServerName: s.sni, MinVersion: tls.VersionTLS12},
	}
	conn, err := d.DialContext(context.Background(), "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	var lp [2]byte
	binary.BigEndian.PutUint16(lp[:], uint16(len(query)))
	if _, err := conn.Write(append(lp[:], query...)); err != nil {
		return nil, err
	}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, err
	}
	ans := make([]byte, binary.BigEndian.Uint16(hdr))
	if _, err := io.ReadFull(conn, ans); err != nil {
		return nil, err
	}
	return ans, nil
}
