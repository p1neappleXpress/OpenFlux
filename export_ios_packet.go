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

	"openflux/network"
	"openflux/transport"
	"openflux/transport/oneme"
	"openflux/transport/yandex"
	"openflux/utils"
)

// Packet-tunnel (NEPacketTunnelProvider) mode — pure L3 forwarding.
//
// The device is given tunnel address 10.10.10.2, which is exactly what the exit
// node expects (it hardcodes returns to 10.10.10.2). So we forward the device's
// raw IPv4 TCP and UDP packets straight over the transport — no gVisor stack on
// the client, which keeps the extension well under its memory cap. UDP DNS
// retains the existing local DNS-over-TLS path for compatibility with older
// TCP-only exits. Other UDP requires explicit opt-in for a UDP-capable exit.
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
		network.LogPacket("PKT", network.DirInbound, data)
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

var ptUDPEnabled atomic.Bool

// OpenFluxTunSetUDPEnabled enables non-DNS UDP for a known UDP-capable exit.
// Disabled by default: legacy exits otherwise silently blackhole QUIC traffic.
//
//export OpenFluxTunSetUDPEnabled
func OpenFluxTunSetUDPEnabled(enabled C.int) { ptUDPEnabled.Store(enabled != 0) }

// OpenFluxTunWritePacket forwards one device IPv4 TCP or UDP packet.
//
//export OpenFluxTunWritePacket
func OpenFluxTunWritePacket(buf *C.char, length C.int) {
	defer func() { _ = recover() }() // never let a bad packet crash the extension
	if buf == nil || length < 20 || length > 65535 {
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
	ihl := int(pkt[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(pkt[2:4]))
	if ihl < 20 || total < ihl || total > len(pkt) {
		return
	}
	pkt = pkt[:total]
	network.LogPacket("PKT", network.DirOutbound, pkt)
	switch pkt[9] { // protocol
	case 6: // TCP
		t.Send(pkt)
	case 17: // UDP
		if len(pkt) < ihl+8 || binary.BigEndian.Uint16(pkt[6:8])&0x3fff != 0 {
			return
		}
		udpLen := int(binary.BigEndian.Uint16(pkt[ihl+4 : ihl+6]))
		if udpLen < 8 || udpLen > len(pkt)-ihl {
			return
		}
		dstPort := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
		if dstPort != 53 {
			if ptUDPEnabled.Load() {
				_ = t.Send(pkt)
			} else {
				sendICMPPortUnreachable(pkt, outQ)
			}
			return
		}
		pkt = pkt[:ihl+udpLen]
		select {
		case dnsSem <- struct{}{}:
			go func() { defer func() { <-dnsSem }(); handleDNSPacket(pkt, outQ) }()
		default:
		}
	}
}

var dnsSem = make(chan struct{}, 16)

func sendICMPPortUnreachable(orig []byte, outQ chan []byte) {
	ihl := int(orig[0]&0x0f) * 4
	quote := orig[:ihl+8]
	icmp := make([]byte, 8+len(quote))
	icmp[0], icmp[1] = 3, 3
	copy(icmp[8:], quote)
	binary.BigEndian.PutUint16(icmp[2:4], network.IPChecksum(icmp))
	pkt := make([]byte, 20+len(icmp))
	pkt[0], pkt[8], pkt[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	copy(pkt[12:16], orig[16:20])
	copy(pkt[16:20], orig[12:16])
	binary.BigEndian.PutUint16(pkt[10:12], network.IPChecksum(pkt[:20]))
	copy(pkt[20:], icmp)
	select {
	case outQ <- pkt:
	default:
	}
}

// OpenFluxTunReadPacket blocks for the next packet destined to the device.
//
//export OpenFluxTunReadPacket
func OpenFluxTunReadPacket(buf *C.char, max C.int) C.int {
	if buf == nil || max <= 0 {
		return 0
	}
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
	resp[8] = 64
	resp[9] = 17
	copy(resp[12:16], dstIP)
	copy(resp[16:20], srcIP)
	ipck := network.IPChecksum(resp[:20])
	resp[10] = byte(ipck >> 8)
	resp[11] = byte(ipck)
	copy(resp[ihl:ihl+2], dstPort)
	copy(resp[ihl+2:ihl+4], srcPort)
	binary.BigEndian.PutUint16(resp[ihl+4:ihl+6], uint16(udpLen))
	copy(resp[ihl+8:], answer)

	select {
	case outQ <- resp:
	default:
	}
}

func dnsOverTLS(query []byte) ([]byte, error) {
	var lastErr error
	for _, s := range dotServers {
		answer, err := dotQueryOne(s, query)
		if err == nil {
			return answer, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func dotQueryOne(s dotServer, query []byte) ([]byte, error) {
	dialer := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 6 * time.Second},
		Config:    &tls.Config{ServerName: s.sni, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
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
