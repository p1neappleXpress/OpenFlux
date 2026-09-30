//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"log"
	"time"

	mobile "openflux-mobile"
)

// OpenFluxStartStreamClient starts the SOCKS5 client on socksAddr in the
// phpbox stream mode: the exit is a PHP node on an ordinary web host, reached
// over a cups.online room (transportType "cupsonline", url the room's address)
// or a Mail.ru document ("mailru", its public link). TCP only (ports 80 and
// 443 at the exit); no key, no session. Returns a start code; the rest of the
// client (OpenFluxStop, OpenFluxIsConnected, OpenFluxStatsJSON) is shared.
//
//export OpenFluxStartStreamClient
func OpenFluxStartStreamClient(transportType, url, socksAddr *C.char) (rc C.int) {
	defer recoverStart(&rc, "OpenFluxStartStreamClient")
	if mobile.ProxyIsRunning() {
		return startAlreadyRunning
	}
	msg := mobile.StartStreamProxy(normalizeType(C.GoString(transportType)), C.GoString(url), C.GoString(socksAddr), "", "", "")
	if msg != "" {
		log.Printf("[BRIDGE] start: %s", msg)
		return startCode(msg)
	}
	proxyStarted = time.Now()
	return startOK
}

// OpenFluxStartStreamPacketTunnel starts the packet tunnel in the stream mode:
// the Network Extension's packets go into a local stack that opens a stream at
// the PHP exit for each TCP connection (see mobile.StartStreamPacket). Returns
// a start code.
//
//export OpenFluxStartStreamPacketTunnel
func OpenFluxStartStreamPacketTunnel(transportType, url *C.char) (rc C.int) {
	defer recoverStart(&rc, "OpenFluxStartStreamPacketTunnel")
	typ := normalizeType(C.GoString(transportType))
	value := C.GoString(url)
	return startPacketTunnel(func() string { return mobile.StartStreamPacket(typ, value) })
}

// OpenFluxPhpCall runs one step of putting the PHP exit on a web host over FTP
// ("probe", "deploy", "check", "start", "stop", "node", "newRoom", "link",
// "remove"): paramsJSON is phphost.Params, the answer is JSON {"ok":true,
// "data":...} or {"ok":false,"error":...,"code":...,"param":...}; the app
// words the code. It blocks: call it off the main thread. Free with
// OpenFluxFreeString.
//
//export OpenFluxPhpCall
func OpenFluxPhpCall(method, paramsJSON *C.char) *C.char {
	return C.CString(mobile.PhpCall(C.GoString(method), C.GoString(paramsJSON)))
}

// OpenFluxPhpProgress returns the upload progress events (one JSON object per
// line) since the last call, "" when none. Free with OpenFluxFreeString.
//
//export OpenFluxPhpProgress
func OpenFluxPhpProgress() *C.char {
	return C.CString(mobile.PhpProgress())
}

// OpenFluxPhpCancel abandons the step OpenFluxPhpCall is running.
//
//export OpenFluxPhpCancel
func OpenFluxPhpCancel() {
	mobile.PhpCancel()
}
