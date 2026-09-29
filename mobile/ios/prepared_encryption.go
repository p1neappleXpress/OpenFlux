//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"strings"

	mobile "openflux-mobile"
)

// packetSessionSpecs follows the same classic/multiple-document rules for key
// preparation and packet startup. Tokens do not affect the encryption context.
func packetSessionSpecs(typ, value, token, uid string) (string, bool) {
	if specs := classicSpecs(typ, value); len(specs) > 1 {
		b, _ := json.Marshal(specs)
		return string(b), false
	}
	return mobile.ClassicSessionSpecs(
		typ,
		value,
		token,
		uid,
	), true
}

// OpenFluxPreparePacketTunnelKeys returns {"keys":...} or {"error":...}.
// Call ONLY in the containing app. Store keys in the shared Keychain and free
// the result with OpenFluxFreeString. Never log or persist it as VPN config.
//
//export OpenFluxPreparePacketTunnelKeys
func OpenFluxPreparePacketTunnelKeys(transportType, url, secret *C.char) *C.char {
	typ := normalizeType(C.GoString(transportType))
	value := strings.TrimSpace(C.GoString(url))
	specs, _ := packetSessionSpecs(
		typ,
		value,
		"",
		"",
	)
	return preparePacketKeys(specs, strings.TrimSpace(C.GoString(secret)))
}

// OpenFluxPrepareSessionPacketTunnelKeys prepares a Session profile in the app.
// It has the same private result format as OpenFluxPreparePacketTunnelKeys.
//
//export OpenFluxPrepareSessionPacketTunnelKeys
func OpenFluxPrepareSessionPacketTunnelKeys(specsJSON, secret *C.char) *C.char {
	return preparePacketKeys(C.GoString(specsJSON), strings.TrimSpace(C.GoString(secret)))
}

func preparePacketKeys(specs, secret string) *C.char {
	keys, err := mobile.PrepareSessionKeys(specs, secret)
	if err != nil {
		return jsonString(map[string]string{"error": err.Error()})
	}
	return jsonString(map[string]string{"keys": keys})
}

// OpenFluxSetPreparedEncryption opts the next packet start into prepared keys.
// Call after OpenFluxSetEncryption, which clears this setting. Empty input opts
// in but fails encrypted startup closed. Apps which never call this setter keep
// their existing startup behavior; updated extensions must always call it.
//
//export OpenFluxSetPreparedEncryption
func OpenFluxSetPreparedEncryption(bundle *C.char) {
	settings.mu.Lock()
	defer settings.mu.Unlock()
	settings.prepared = C.GoString(bundle)
	settings.preparedSet = true
}
