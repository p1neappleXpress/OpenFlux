//go:build ios

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"openflux/internal/transportstack"
)

// OpenFluxResolveBypassIPv4 must be called BEFORE setTunnelNetworkSettings.
// Returns a JSON array of /32 addresses or nil (compatibility API). The V2
// provider uses the full route plan, including CIDRs used by its dial guard.
// Release the result with OpenFluxFreeString. Never log its inputs.
//
//export OpenFluxResolveBypassIPv4
func OpenFluxResolveBypassIPv4(transportType, url *C.char) *C.char {
	p := preparePacketBypass(C.GoString(transportType), C.GoString(url), false)
	if p == nil {
		return nil
	}
	data, err := json.Marshal(p.Addresses())
	if err != nil {
		return nil
	}
	return C.CString(string(data))
}

// OpenFluxResolveBypassIPv4V2 prepares an immutable carrier DNS/route snapshot.
// System DNS is allowed only here, before the packet tunnel is active. Neither
// the global DoT resolver nor device DNS handling acquires a system fallback.
//
//export OpenFluxResolveBypassIPv4V2
func OpenFluxResolveBypassIPv4V2(transportType, url *C.char) *C.char {
	p := preparePacketBypass(C.GoString(transportType), C.GoString(url), true)
	if p == nil {
		return nil
	}
	data, err := json.Marshal(p.Routes())
	if err != nil {
		return nil
	}
	return C.CString(string(data))
}

var packetBypass *transportstack.BypassPlan // guarded by ptMu
var packetBypassKind, packetBypassURL string

func preparePacketBypass(kind, docURL string, includePrefixes bool) *transportstack.BypassPlan {
	ptMu.Lock()
	defer ptMu.Unlock()
	if ptSession != nil {
		return nil
	}
	packetBypass = nil
	packetBypassKind, packetBypassURL = "", ""
	hosts, err := transportstack.BypassHosts(kind, docURL)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// A fresh resolver with no custom Dial uses the native iOS resolver. Do not
	// use net.DefaultResolver here for system DNS: init replaces it with DoT.
	system := &net.Resolver{PreferGo: false}
	plan, err := transportstack.ResolveBypassIPv4(ctx, kind, hosts, net.DefaultResolver.LookupIPAddr, system.LookupIPAddr)
	if err != nil {
		return nil
	}
	if !includePrefixes {
		plan = plan.HostRoutesOnly()
	}
	packetBypass, packetBypassKind, packetBypassURL = plan, kind, docURL
	return plan
}
