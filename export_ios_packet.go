//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"runtime/debug"
	"strconv"
	"sync"
	"unsafe"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/tunnel"
	"universal-bypass-tool/utils"
)

// Packet-tunnel (NEPacketTunnelProvider) mode: the device's raw IP packets are
// pushed in with OpenFluxTunWritePacket and outbound packets pulled out with
// OpenFluxTunReadPacket. TCP is forwarded through the transport to the exit
// node; DNS (UDP 53) is proxied as DNS-over-TCP (see tunnel.PacketTunnel).

var (
	ptMu     sync.Mutex
	ptTun    *tunnel.PacketTunnel
	ptTrans  transport.Transport
	ptCtx    context.Context
	ptCancel context.CancelFunc
	ptOn     bool
)

// OpenFluxStartPacketTunnel starts the transport and the tun2socks stack.
// Returns 0 on success (see start* codes in export_ios.go).
//
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

	// The Network Extension has a hard memory cap (~50MB). Keep the Go heap
	// small: soft-limit memory and GC aggressively so we don't get killed.
	debug.SetMemoryLimit(45 << 20)
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

	if err := t.Start(); err != nil {
		utils.Debugf("[PKT] transport start failed: %v", err)
		return C.int(startTransportError)
	}

	// The client TCPTunnel originates connections toward the exit node; the
	// PacketTunnel terminates the device's flows and dials through it.
	tcpTun := tunnel.NewTCPTunnel(t, false)
	pt := tunnel.NewPacketTunnel(tcpTun, 1500)

	ptTrans = t
	ptTun = pt
	ptCtx, ptCancel = context.WithCancel(context.Background())
	ptOn = true
	utils.Debugf("[PKT] packet tunnel started (transport %s)", tt)
	return C.int(startOK)
}

// OpenFluxTunWritePacket injects one IPv4 packet from the device into the stack.
//
//export OpenFluxTunWritePacket
func OpenFluxTunWritePacket(buf *C.char, length C.int) {
	if buf == nil || length <= 0 {
		return
	}
	ptMu.Lock()
	pt := ptTun
	ptMu.Unlock()
	if pt == nil {
		return
	}
	data := C.GoBytes(unsafe.Pointer(buf), length)
	pt.WriteInbound(data)
}

// OpenFluxTunReadPacket blocks for the next outbound packet, copies up to max
// bytes into buf, and returns the length written (0 when the tunnel stops).
//
//export OpenFluxTunReadPacket
func OpenFluxTunReadPacket(buf *C.char, max C.int) C.int {
	ptMu.Lock()
	pt := ptTun
	ctx := ptCtx
	ptMu.Unlock()
	if pt == nil || ctx == nil {
		return 0
	}
	data := pt.ReadOutbound(ctx)
	if len(data) == 0 {
		return 0
	}
	n := len(data)
	if n > int(max) {
		n = int(max)
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(max))
	copy(dst[:n], data[:n])
	return C.int(n)
}

// OpenFluxStopPacketTunnel tears down the packet tunnel and transport.
//
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
	if ptTun != nil {
		ptTun.Close()
	}
	if ptTrans != nil {
		ptTrans.Stop()
	}
	ptTun = nil
	ptTrans = nil
	ptOn = false
	utils.Debugf("[PKT] packet tunnel stopped")
}
