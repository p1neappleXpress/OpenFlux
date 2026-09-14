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
	"io"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"universal-bypass-tool/network"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/utils"
)

// Packet-tunnel (NEPacketTunnelProvider) mode — pure L3 forwarding.
//
// The device is given tunnel address 10.10.10.2, which is exactly what the exit
// node expects (it hardcodes returns to 10.10.10.2). So we forward the device's
// raw IP packets straight over the transport — no gvisor stack on the client,
// which keeps the extension well under its memory cap and preserves full TCP
// throughput end-to-end. Only TCP is forwarded (the exit node is TCP-only);
// DNS (UDP 53) is answered locally over DNS-over-TLS.
//
// Uses startOK / start* codes and dotServers from export_ios.go.

const tunClientIP = "10.10.10.2"

var (
	ptMu     sync.Mutex
	ptOn     bool
	ptTrans  transport.Transport
	ptOutQ   chan []byte
	ptCtx    context.Context
	ptCancel context.CancelFunc
)

//export OpenFluxStartPacketTunnel
func OpenFluxStartPacketTunnel(transportType, url, maxToken, maxUid *C.char) (rc C.int) {
	tt := C.GoString(transportType)
	docURL := C.GoString(url)
	mToken := C.GoString(maxToken)
	mUid := C.GoString(maxUid)

	defer func() {
		if r := recover(); r != nil {
			utils.Debugf("[PKT] Recovered from panic in start: %v", r)
			rc = C.int(startPanic)
		}
	}()

	ptMu.Lock()
	defer ptMu.Unlock()
	if ptOn {
		return C.int(startAlreadyRunning)
	}

	// Memory strategy: SetMemoryLimit is the hard backstop that keeps us under
	// the NE cap; GCPercent then only controls how eagerly we collect BELOW that
	// limit. GCPercent=20 forced a GC on every 20% heap growth — under a
	// throughput load (compression + WebSocket framing + packet copies) that
	// pins the phone CPU in near-continuous GC and caps throughput. Raise it so
	// GC is driven by the memory limit, not by needless frequent cycles; peak
	// memory is still bounded by SetMemoryLimit, so this does not risk jetsam.
	debug.SetMemoryLimit(40 << 20)
	debug.SetGCPercent(100)

	config := transport.DefaultConfig()
	var t transport.Transport
	switch tt {
	case "yandex", "":
		t = buildDocTransport(docURL, config, func(u string) transport.Transport {
			return yandex.NewYandexDocsTransport(u, config)
		})
	case "volga", "vyandex":
		// Slim VOLGA profile so the relay worker pool + queues stay under the
		// NE memory cap (the default is a server profile). Note: multiplexing
		// VOLGA multiplies that pool per channel, so keep VOLGA lists short.
		t = buildDocTransport(docURL, config, func(u string) transport.Transport {
			return yandex.NewYandexVolgaTransportWithConfig(u, config, yandex.SlimVolgaConfig())
		})
	case "oneme":
		uidint, _ := strconv.ParseInt(mUid, 10, 64)
		t = transport.NewCompressedTransport(oneme.NewOneMeTransport(false, mToken, uidint, config))
	default:
		return C.int(startBadTransport)
	}

	// Device-bound packet queue. 1024 was too shallow: a download burst fills it
	// faster than the device drains, packets get dropped, and the tunneled TCP
	// treats that as loss and backs off — throttling throughput. A deeper queue
	// absorbs bursts; entries are transient and bounded by the memory limit.
	outQ := make(chan []byte, 4096)
	// Packets coming back from the exit node -> queue for the device.
	t.Receive(func(data []byte) {
		select {
		case outQ <- append([]byte(nil), data...):
		default: // queue full: drop, TCP will retransmit
		}
	})

	if err := t.Start(); err != nil {
		utils.Debugf("[PKT] transport start failed: %v", err)
		return C.int(startTransportError)
	}

	ptTrans = t
	ptOutQ = outQ
	ptCtx, ptCancel = context.WithCancel(context.Background())
	ptOn = true
	utils.Debugf("[PKT] L3 packet tunnel started (transport %s)", tt)
	return C.int(startOK)
}

// OpenFluxTunWritePacket forwards one device IPv4 packet: TCP goes over the
// transport, DNS (UDP 53) is answered locally, other UDP is dropped.
//
//export OpenFluxTunWritePacket
func OpenFluxTunWritePacket(buf *C.char, length C.int) {
	defer func() { _ = recover() }() // never let a bad packet crash the extension
	if buf == nil || length < 20 {
		return
	}
	ptMu.Lock()
	t := ptTrans
	outQ := ptOutQ
	ptMu.Unlock()
	if t == nil {
		return
	}
	pkt := C.GoBytes(unsafe.Pointer(buf), length)
	if pkt[0]>>4 != 4 { // IPv4 only
		return
	}
	switch pkt[9] { // protocol
	case 6: // TCP
		t.Send(pkt)
	case 17: // UDP
		ihl := int(pkt[0]&0x0f) * 4
		if len(pkt) < ihl+8 {
			return
		}
		dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
		if dstPort == 53 {
			// DNS is answered locally over DNS-over-TLS (defeats poisoning and
			// avoids a round-trip through the covert channel for every query).
			// Bound concurrent resolutions so a burst can't spawn an unbounded
			// pile of goroutines + TLS handshakes (memory).
			select {
			case dnsSem <- struct{}{}:
				go func() { defer func() { <-dnsSem }(); handleDNSPacket(pkt, outQ) }()
			default: // too many in flight: drop, the client retries
			}
		} else if tunnelUDP.Load() {
			// UDP tunneling ON: forward all other UDP (QUIC/HTTP3, games, …)
			// over the transport; the exit node NATs it on its raw socket.
			// Requires a UDP-capable exit node.
			t.Send(pkt)
		} else {
			// UDP tunneling OFF (default, legacy-safe): reply ICMP
			// port-unreachable so apps fall back from QUIC/UDP:443 to TCP fast
			// instead of stalling — works against any (TCP-only) exit node.
			sendICMPPortUnreachable(pkt, outQ)
		}
	}
}

// tunnelUDP controls whether non-DNS UDP is forwarded over the transport (ON,
// needs a UDP-capable exit node) or fast-failed with ICMP (OFF, legacy-safe on
// any exit node). Default OFF so a single build works against both node types.
var tunnelUDP atomic.Bool

// OpenFluxSetTunnelUDP toggles UDP forwarding. Set before starting the tunnel
// (the extension reads it from providerConfiguration).
//
//export OpenFluxSetTunnelUDP
func OpenFluxSetTunnelUDP(on C.int) { tunnelUDP.Store(on != 0) }

// sendICMPPortUnreachable enqueues an ICMP "destination/port unreachable" for a
// UDP datagram we won't forward, so the sender falls back to TCP fast.
func sendICMPPortUnreachable(orig []byte, outQ chan []byte) {
	ihl := int(orig[0]&0x0f) * 4
	if len(orig) < ihl+8 {
		return
	}
	quote := orig[:ihl+8] // original IP header + 8 bytes (per RFC 792)
	icmp := make([]byte, 8+len(quote))
	icmp[0] = 3 // Destination Unreachable
	icmp[1] = 3 // Port Unreachable
	copy(icmp[8:], quote)
	ck := network.IPChecksum(icmp)
	icmp[2] = byte(ck >> 8)
	icmp[3] = byte(ck & 0xFF)

	total := 20 + len(icmp)
	ip := make([]byte, total)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(total))
	ip[8] = 64 // TTL
	ip[9] = 1  // ICMP
	copy(ip[12:16], orig[16:20]) // src = original destination
	copy(ip[16:20], orig[12:16]) // dst = original source (the device)
	ck2 := network.IPChecksum(ip[:20])
	ip[10] = byte(ck2 >> 8)
	ip[11] = byte(ck2 & 0xFF)
	copy(ip[20:], icmp)

	select {
	case outQ <- ip:
	default:
	}
}

// dnsSem caps concurrent DNS-over-TLS resolutions.
var dnsSem = make(chan struct{}, 16)

// OpenFluxTunReadPacket blocks for the next packet destined to the device.
//
//export OpenFluxTunReadPacket
func OpenFluxTunReadPacket(buf *C.char, max C.int) C.int {
	ptMu.Lock()
	outQ := ptOutQ
	ctx := ptCtx
	ptMu.Unlock()
	if outQ == nil || ctx == nil {
		return 0
	}
	select {
	case data := <-outQ:
		n := len(data)
		if n > int(max) {
			n = int(max)
		}
		dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(max))
		copy(dst[:n], data[:n])
		return C.int(n)
	case <-ctx.Done():
		return 0
	}
}

//export OpenFluxStopPacketTunnel
func OpenFluxStopPacketTunnel() {
	ptMu.Lock()
	defer ptMu.Unlock()
	if !ptOn {
		return
	}
	if ptCancel != nil {
		ptCancel()
	}
	if ptTrans != nil {
		ptTrans.Stop()
	}
	ptTrans = nil
	ptOutQ = nil
	ptOn = false
	utils.Debugf("[PKT] L3 packet tunnel stopped")
}

// handleDNSPacket answers a device DNS query over DNS-over-TLS and enqueues a
// UDP response packet back to the device.
func handleDNSPacket(req []byte, outQ chan []byte) {
	defer func() { _ = recover() }()
	ihl := int(req[0]&0x0f) * 4
	if len(req) < ihl+8 {
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

	answer, err := dnsOverTLS(query)
	if err != nil || len(answer) == 0 {
		utils.Debugf("[DNS] resolve failed: %v", err)
		return
	}

	// Build the response: swap addresses/ports (dst<->src), UDP checksum 0.
	udpLen := 8 + len(answer)
	total := ihl + udpLen
	resp := make([]byte, total)
	// IP header: copy version/IHL/TOS, set total length, TTL/proto, addresses.
	resp[0] = req[0]
	resp[1] = req[1]
	binary.BigEndian.PutUint16(resp[2:4], uint16(total))
	resp[8] = 64 // TTL
	resp[9] = 17 // UDP
	copy(resp[12:16], dstIP)  // src = original destination (the resolver)
	copy(resp[16:20], srcIP)  // dst = the device
	resp[10], resp[11] = 0, 0 // checksum field
	ipck := network.IPChecksum(resp[:20])
	resp[10] = byte(ipck >> 8)
	resp[11] = byte(ipck & 0xFF)
	// UDP header
	copy(resp[ihl:ihl+2], dstPort)   // src port = 53
	copy(resp[ihl+2:ihl+4], srcPort) // dst port = device's
	binary.BigEndian.PutUint16(resp[ihl+4:ihl+6], uint16(udpLen))
	// checksum 0 (allowed for IPv4 UDP)
	copy(resp[ihl+8:], answer)

	select {
	case outQ <- resp:
	default:
	}
}

// dnsOverTLS sends a DNS query to a DoT resolver (RFC 7858, length-prefixed)
// and returns the raw DNS answer, trying each server in turn.
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
	conn.SetDeadline(time.Now().Add(6 * time.Second))

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
