package tunnel

import (
	"os"
	"strings"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// tcpProtocol is the TCP the tunnel's netstack runs. gVisor defaults to Reno,
// whose additive growth (one MSS per RTT) never fills a path with real spare
// bandwidth but an RTT of tens to hundreds of ms - exactly what a tunnel over
// a relay looks like. CUBIC keeps every other default and ramps the window
// up much faster, so the stack starts as CUBIC; wantedCC then picks per
// moment which of the two connections opened from now on use.
func tcpProtocol() stack.TransportProtocolFactory {
	if strings.EqualFold(os.Getenv("OPENFLUX_TCP_CC"), "reno") {
		return tcp.NewProtocol
	}
	return tcp.NewProtocolCUBIC
}

// wantedCC is the congestion control for connections opened now:
//
//   - OPENFLUX_TCP_CC=reno or cubic forces one;
//   - otherwise Reno while the traffic goes over reliable byte streams (a direct TCP
//     connection): nothing is ever lost inside the tunnel there, and CUBIC's HyStart
//     takes the carrier's delay jitter for congestion, leaves slow start at half the
//     window the path takes and then needs seconds to catch up (a single download over a
//     real path: 6-7 MB/s with Reno, 3.5-4.5 with CUBIC);
//   - CUBIC for carriers that drop packets (a document relay, a video call), where its
//     faster recovery matters.
func wantedCC(trans transport.Transport) string {
	switch strings.ToLower(os.Getenv("OPENFLUX_TCP_CC")) {
	case "reno":
		return "reno"
	case "cubic":
		return "cubic"
	}
	if s, ok := trans.(transport.StreamCarrierer); ok && s.OnReliableStream() {
		return "reno"
	}
	return "cubic"
}

// setCC makes cc the congestion control of the connections the stack opens or accepts from now on.
func setCC(s *stack.Stack, cc string) {
	opt := tcpip.CongestionControlOption(cc)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt); err != nil {
		utils.Debugf("[TUNNEL] congestion control %s: %v", cc, err)
	}
}
