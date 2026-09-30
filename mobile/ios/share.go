//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"github.com/p1neappleXpress/OpenFlux/share"
	mobile "openflux-mobile"
)

// Links are read and made by the core only: the app hands the string (or
// the configuration) over and gets share.Result back as JSON, the same
// answer Desktop, Android and the CLI get. The app words its code.

type shareResult struct {
	share.Result
	// Session is the profile ready for OpenFluxStartSession /
	// OpenFluxStartSessionPacketTunnel (with the link's secret): every
	// carrier, its name, priority and address, and the context. A
	// one-carrier link works there too, speaking classic to a classic node.
	Session string `json:"session,omitempty"`
}

// OpenFluxShareDecode reads an openflux:// link: {"config":...,
// "context":...,"session":...} or {"error":...,"code":...,"param":...}.
// Free with OpenFluxFreeString.
//
//export OpenFluxShareDecode
func OpenFluxShareDecode(link *C.char) *C.char {
	if link == nil {
		return C.CString(share.Read("").JSON())
	}
	s := C.GoString(link)
	r := shareResult{Result: share.Read(s)}
	if r.Config != nil {
		r.Session, _ = mobile.ShareSessionSpecs(s)
	}
	return jsonString(r)
}

// OpenFluxShareEncode builds the link for a share.Config JSON the way
// every client exports one: {"link":...,"config":...,"context":...} or the
// error. The core validates it, so no invalid link leaves the app.
//
//export OpenFluxShareEncode
func OpenFluxShareEncode(cfgJSON *C.char) *C.char {
	if cfgJSON == nil {
		return C.CString(share.MakeJSON("").JSON())
	}
	return C.CString(share.MakeJSON(C.GoString(cfgJSON)).JSON())
}
