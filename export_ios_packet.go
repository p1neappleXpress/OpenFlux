//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"openflux/internal/packettunnel"
	"openflux/internal/transportstack"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"openflux/network"
	"openflux/utils"
)

// Packet-tunnel (NEPacketTunnelProvider) mode — pure L3 forwarding.
//
// The device is given tunnel address 10.10.10.2, which is exactly what the exit
// node expects (it hardcodes returns to 10.10.10.2). So we forward the device's
// raw IPv4 TCP and (when enabled) UDP packets over the transport, without a
// client-side gVisor stack. DNS (UDP 53) stays local over DNS-over-TLS.
//
// Uses startOK / start* codes and dotServers from export_ios.go.

const tunClientIP = "10.10.10.2"

var (
	ptMu         sync.Mutex
	ptSession    *packettunnel.Session
	ptStopped    bool
	ptUDPEnabled atomic.Bool
)

// OpenFluxTunSetUDPEnabled opts in to non-DNS UDP when the exit supports it.
// Older TCP-only exits otherwise blackhole UDP. Defaults to off.
//
//export OpenFluxTunSetUDPEnabled
func OpenFluxTunSetUDPEnabled(enabled C.int) { ptUDPEnabled.Store(enabled != 0) }

//export OpenFluxStartPacketTunnel
func OpenFluxStartPacketTunnel(transportType, url, maxToken, maxUid *C.char) C.int {
	return startPacketTunnel(C.GoString(transportType), C.GoString(url), C.GoString(maxToken),
		C.GoString(maxUid), transportstack.Legacy, "", nil)
}

// OpenFluxStartPacketTunnelV2 is the secret-based API. Its KDF uses 32 MiB;
// the production extension uses WithKeyV2 to avoid that transient allocation.
//
//export OpenFluxStartPacketTunnelV2
func OpenFluxStartPacketTunnelV2(transportType, url, maxToken, maxUid, codec, encryptionSecret *C.char) C.int {
	return startPacketTunnel(C.GoString(transportType), C.GoString(url), C.GoString(maxToken),
		C.GoString(maxUid), C.GoString(codec), C.GoString(encryptionSecret), nil)
}

//export OpenFluxStartPacketTunnelWithKeyV2
func OpenFluxStartPacketTunnelWithKeyV2(transportType, url, maxToken, maxUid, codec, preparedKey *C.char) C.int {
	encoded := C.GoString(preparedKey)
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || (encoded != "" && len(key) != 32) {
		return C.int(startBadConfig)
	}
	defer clear(key)
	return startPacketTunnel(C.GoString(transportType), C.GoString(url), C.GoString(maxToken),
		C.GoString(maxUid), C.GoString(codec), "", key)
}

func startPacketTunnel(tt, docURL, mToken, mUid, codec, secret string, key []byte) (rc C.int) {
	defer func() {
		if recover() != nil {
			rc = C.int(startPanic)
		}
	}()
	ptMu.Lock()
	defer ptMu.Unlock()
	if ptSession != nil {
		return C.int(startAlreadyRunning)
	}
	logbuf.SetSecrets(docURL, mToken, secret)
	// Soft runtime budget, NOT an RSS guarantee (Swift/TLS/NE also use memory).
	debug.SetMemoryLimit(24 << 20)
	debug.SetGCPercent(100)
	o, err := mobileOptions(tt, docURL, mToken, mUid, codec, secret)
	if err != nil {
		return C.int(startBadTransport)
	}
	o.PreparedKey = key
	if packetBypass != nil {
		if packetBypassKind != o.Transport || packetBypassURL != docURL {
			return C.int(startBadConfig)
		}
		if o.Transport == "yandex" || o.Transport == "vyandex" {
			o.DialContext = packetBypass.DialContext
		}
		packetBypass = nil // The transport owns the immutable snapshot now.
		packetBypassKind, packetBypassURL = "", ""
	}
	t, err := transportstack.New(o)
	if err != nil {
		return C.int(startBadConfig)
	}
	session := packettunnel.New(t)
	if err := session.Start(); err != nil {
		session.Stop()
		utils.Debugf("[PKT] transport start failed")
		return C.int(startTransportError)
	}
	ptSession = session
	ptStopped = false
	utils.Debugf("[PKT] packet tunnel started")
	return C.int(startOK)
}

//export OpenFluxPacketTunnelIsConnected
func OpenFluxPacketTunnelIsConnected() C.int {
	ptMu.Lock()
	s := ptSession
	ptMu.Unlock()
	if s != nil && s.Connected() {
		return 1
	}
	return 0
}

// OpenFluxTunWritePacket forwards IPv4 TCP and optional non-DNS UDP. DNS
// (UDP 53) is answered locally; unsupported UDP gets an ICMP error.
//
//export OpenFluxTunWritePacket
func OpenFluxTunWritePacket(buf *C.char, length C.int) {
	defer func() { _ = recover() }() // never let a bad packet crash the extension
	if buf == nil || length < 20 || length > packettunnel.MaxPacketSize {
		return
	}
	ptMu.Lock()
	session := ptSession
	ptMu.Unlock()
	if session == nil {
		return
	}
	// The shared codec/encryption stack owns bytes before Send returns. Borrow
	// Swift's buffer only for this synchronous call; DNS below makes its own copy.
	pkt := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(length))
	action, pkt := packettunnel.ClassifyIPv4(pkt, ptUDPEnabled.Load())
	switch action {
	case packettunnel.Forward:
		session.Send(pkt)
	case packettunnel.ResolveDNS:
		// Bound concurrent DNS resolutions and own the Swift buffer in the goroutine.
		select {
		case dnsSem <- struct{}{}:
			owned := append([]byte(nil), pkt...)
			go func() { defer func() { <-dnsSem }(); handleDNSPacket(session.Context(), owned, session.Enqueue) }()
		default: // too many in flight: drop, the client retries
		}
	case packettunnel.RejectUDP:
		sendICMPPortUnreachable(pkt, session.Enqueue)
	}
}

// sendICMPPortUnreachable enqueues an ICMP "destination/port unreachable" for a
// UDP datagram we won't forward, so the sender falls back to TCP fast.
func sendICMPPortUnreachable(orig []byte, enqueue func([]byte)) {
	ihl := int(orig[0]&0x0f) * 4
	if ihl < 20 || len(orig) < ihl+8 {
		return
	}
	quote := orig[:ihl+8] // original IP header + 8 bytes (per RFC 792)
	icmp := make([]byte, 8+len(quote))
	icmp[0], icmp[1] = 3, 3
	copy(icmp[8:], quote)
	ck := network.IPChecksum(icmp)
	icmp[2] = byte(ck >> 8)
	icmp[3] = byte(ck & 0xFF)

	total := 20 + len(icmp)
	ip := make([]byte, total)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(total))
	ip[8] = 64                   // TTL
	ip[9] = 1                    // ICMP
	copy(ip[12:16], orig[16:20]) // src = original destination
	copy(ip[16:20], orig[12:16]) // dst = original source (the device)
	ck2 := network.IPChecksum(ip[:20])
	ip[10] = byte(ck2 >> 8)
	ip[11] = byte(ck2 & 0xFF)
	copy(ip[20:], icmp)

	enqueue(ip)
}

// dnsSem caps concurrent DNS-over-TLS resolutions.
var dnsSem = make(chan struct{}, 4)

// OpenFluxTunReadPacket: >0 complete packet; 0 transient/no packet/invalid
// buffer; -1 explicit stop. A short buffer DROPS the packet, never truncates.
//
//export OpenFluxTunReadPacket
func OpenFluxTunReadPacket(buf *C.char, max C.int) C.int {
	if buf == nil || max <= 0 {
		return 0
	}
	ptMu.Lock()
	s, stopped := ptSession, ptStopped
	ptMu.Unlock()
	if stopped {
		return -1
	}
	if s == nil || buf == nil || max <= 0 {
		return 0
	}
	size := min(int(max), packettunnel.MaxPacketSize)
	return C.int(s.Read(unsafe.Slice((*byte)(unsafe.Pointer(buf)), size)))
}

//export OpenFluxStopPacketTunnel
func OpenFluxStopPacketTunnel() {
	ptMu.Lock()
	ptUDPEnabled.Store(false)
	packetBypass = nil
	packetBypassKind, packetBypassURL = "", ""
	s := ptSession
	ptStopped = true
	// Keep serialization with Start until every old transport worker is stopped.
	if s != nil {
		s.Stop()
	}
	ptSession = nil
	ptMu.Unlock()
}

// handleDNSPacket answers a device DNS query over DNS-over-TLS and enqueues a
// UDP response packet back to the device.
func handleDNSPacket(ctx context.Context, req []byte, enqueue func([]byte)) {
	defer func() { _ = recover() }()
	ihl := int(req[0]&0x0f) * 4
	if ihl < 20 || len(req) < ihl+8 {
		return
	}
	srcIP := req[12:16]
	dstIP := req[16:20]
	srcPort := req[ihl : ihl+2]
	dstPort := req[ihl+2 : ihl+4]
	query := req[ihl+8:]
	if len(query) == 0 {
		return
	}

	answer, err := dnsOverTLS(ctx, query)
	if err != nil || len(answer) == 0 || len(answer)+ihl+8 > packettunnel.MaxPacketSize {
		utils.Debugf("[DNS] resolve failed: %v", err)
		return
	}

	udpLen := 8 + len(answer)
	if udpLen > 65535-20 {
		return
	}
	// Build a fresh minimal IPv4 header; do not advertise uncopied options.
	ihl = 20
	total := ihl + udpLen
	resp := make([]byte, total)
	resp[0] = 0x45
	resp[1] = req[1]
	binary.BigEndian.PutUint16(resp[2:4], uint16(total))
	resp[8] = 64              // TTL
	resp[9] = 17              // UDP
	copy(resp[12:16], dstIP)  // src = original destination (the resolver)
	copy(resp[16:20], srcIP)  // dst = the device
	resp[10], resp[11] = 0, 0 // checksum field
	ipck := network.IPChecksum(resp[:ihl])
	resp[10] = byte(ipck >> 8)
	resp[11] = byte(ipck)
	copy(resp[ihl:ihl+2], dstPort)
	copy(resp[ihl+2:ihl+4], srcPort)
	binary.BigEndian.PutUint16(resp[ihl+4:ihl+6], uint16(udpLen))
	copy(resp[ihl+8:], answer)

	enqueue(resp)
}

// dnsOverTLS sends a DNS query to a DoT resolver (RFC 7858, length-prefixed)
// and returns the raw DNS answer, trying each server in turn.
func dnsOverTLS(ctx context.Context, query []byte) ([]byte, error) {
	var lastErr error
	for _, s := range dotServers {
		answer, err := dotQueryOne(ctx, s, query)
		if err == nil {
			return answer, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func dotQueryOne(ctx context.Context, s dotServer, query []byte) ([]byte, error) {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 6 * time.Second},
		Config:    &tls.Config{ServerName: s.sni, MinVersion: tls.VersionTLS12},
	}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()
	conn.SetDeadline(time.Now().Add(6 * time.Second))

	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(query)))
	if _, err := conn.Write(append(length[:], query...)); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, length[:]); err != nil {
		return nil, err
	}
	answer := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(conn, answer); err != nil {
		return nil, err
	}
	return answer, nil
}
