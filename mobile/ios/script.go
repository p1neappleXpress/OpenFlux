//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"unsafe"

	mobile "openflux-mobile"
)

// Script transports (the JS/goja engine) for the iOS app: the calls the
// Android bridge has in mobile/script.go, so the app inspects, trusts, sets
// up and updates a signed transport the way every other client does, and
// carries no idea of its own of a signature, a version or an update.
//
// The engine itself is already in this library (mobile imports
// transport/script). A profile runs an installed transport through a Session
// spec of type "script" with params "path", "pubkey" and "name"
// (registry.newScript); the Network Extension opens that file, so the app
// installs transports into its App Group container, not its own sandbox.
//
// Every string comes back as JSON or text; free it with OpenFluxFreeString.
// The update calls reach the network and block: call them off the main thread.

// goBytes copies a buffer the app passes in; nil or a non-positive length is
// an empty slice, the same as an absent signature on Android.
func goBytes(buf *C.char, length C.int) []byte {
	if buf == nil || length <= 0 {
		return nil
	}
	return C.GoBytes(unsafe.Pointer(buf), length)
}

// OpenFluxScriptEngineAvailable reports whether the JS engine is linked in
// (1) or not (0), for the app to decide whether to show script transports.
//
//export OpenFluxScriptEngineAvailable
func OpenFluxScriptEngineAvailable() C.int {
	if mobile.ScriptEngineAvailable() {
		return 1
	}
	return 0
}

// OpenFluxScriptEngineSelfTest runs the embedded signed echo script through a
// full open -> connected -> send -> emit round trip in this process and
// returns {"ok":...,"engine":"goja","state":...,"sent":N,"recv":N,"error":...}.
// dir is a writable directory (the app's caches folder).
//
//export OpenFluxScriptEngineSelfTest
func OpenFluxScriptEngineSelfTest(dir *C.char) *C.char {
	return C.CString(mobile.ScriptEngineSelfTest(C.GoString(dir)))
}

// OpenFluxInspectTransport reads a downloaded transport before it is trusted
// or run (no open(), no network) and returns the trust report JSON
// (mobile.InspectTransport). data is a .flux package or a bare .js; sig the
// detached signature of a bare .js (ignored for .flux); pubkeyHex the
// candidate author key, or "" to inspect without one.
//
//export OpenFluxInspectTransport
func OpenFluxInspectTransport(data *C.char, dataLen C.int, sig *C.char, sigLen C.int, pubkeyHex *C.char) *C.char {
	return C.CString(mobile.InspectTransport(goBytes(data, dataLen), goBytes(sig, sigLen), C.GoString(pubkeyHex)))
}

// OpenFluxScriptSettings builds the settings wizard page of an installed
// transport (mobile.ScriptSettings): valuesJSON is the current settings as a
// JSON object of strings ("" for none), lang "ru" or "en".
//
//export OpenFluxScriptSettings
func OpenFluxScriptSettings(data *C.char, dataLen C.int, sig *C.char, sigLen C.int, pubkeyHex, valuesJSON, lang *C.char) *C.char {
	return C.CString(mobile.ScriptSettings(goBytes(data, dataLen), goBytes(sig, sigLen),
		C.GoString(pubkeyHex), C.GoString(valuesJSON), C.GoString(lang)))
}

// OpenFluxScriptFingerprint is the SHA-256 (hex) of an author key, the id the
// UI shows; "" when the key does not decode.
//
//export OpenFluxScriptFingerprint
func OpenFluxScriptFingerprint(pubkeyHex *C.char) *C.char {
	return C.CString(mobile.ScriptFingerprint(C.GoString(pubkeyHex)))
}

// OpenFluxCompareScriptVersions orders two versions: -1, 0 or 1.
//
//export OpenFluxCompareScriptVersions
func OpenFluxCompareScriptVersions(a, b *C.char) C.int {
	return C.int(mobile.CompareScriptVersions(C.GoString(a), C.GoString(b)))
}

// OpenFluxOfficialScriptKey is the project's signing key (hex), so the app
// pins official transports without spelling the key out itself.
//
//export OpenFluxOfficialScriptKey
func OpenFluxOfficialScriptKey() *C.char {
	return C.CString(mobile.OfficialScriptKey())
}

// OpenFluxCheckScriptUpdate asks an installed transport's update sources for a
// newer version on channel ("stable" or "nightly") and returns the
// UpdateReport JSON (mobile.CheckScriptUpdate). installedJSON is
// {"id","file","version","wire","pubkey","update":[...]}. Blocks up to a minute.
//
//export OpenFluxCheckScriptUpdate
func OpenFluxCheckScriptUpdate(installedJSON, channel *C.char) *C.char {
	return C.CString(mobile.CheckScriptUpdate(C.GoString(installedJSON), C.GoString(channel)))
}

// OpenFluxApplyScriptUpdate downloads, verifies under the pinned key and
// installs an update into dir, keeping the replaced version as
// <id>.flux.prev (mobile.ApplyScriptUpdate). A wire break installs only with
// allowWireBreak != 0. Blocks up to three minutes.
//
//export OpenFluxApplyScriptUpdate
func OpenFluxApplyScriptUpdate(installedJSON, channel, dir *C.char, allowWireBreak C.int) *C.char {
	return C.CString(mobile.ApplyScriptUpdate(C.GoString(installedJSON), C.GoString(channel),
		C.GoString(dir), allowWireBreak != 0))
}

// OpenFluxRollbackScript makes the previous version of a transport current
// again (mobile.RollbackScript).
//
//export OpenFluxRollbackScript
func OpenFluxRollbackScript(installedJSON, dir *C.char) *C.char {
	return C.CString(mobile.RollbackScript(C.GoString(installedJSON), C.GoString(dir)))
}
