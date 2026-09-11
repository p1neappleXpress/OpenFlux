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

	// Keep the extension well under the NE memory cap.
	debug.SetMemoryLimit(40 << 20)
	debug.SetGCPercent(20)

	config := transport.DefaultConfig()
	var t transport.Transport
	switch tt {
	case "yandex", "":
		t = transport.NewCompressedTransport(yandex.NewYandexDocsTransport(docURL, config))
	case "oneme":
		uidint, _ := strconv.ParseInt(mUid, 10, 64)
		t = transport.NewCompressedTransport(oneme.NewOneMeTransport(false, mToken, uidint, config))
	default:
		return C.int(startBadTransport)
	}

	outQ := make(chan []byte, 1024)
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
			// Bound concurrent DNS resolutions so a burst can't spawn an
			// unbounded pile of goroutines + TLS handshakes (memory).
			select {
			case dnsSem <- struct{}{}:
				go func() { defer func() { <-dnsSem }(); handleDNSPacket(pkt, outQ) }()
			default: // too many in flight: drop, the client retries
			}
		}
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
	for _, s := range dotServers {
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
