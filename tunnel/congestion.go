package tunnel

import (
	"os"

	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// tcpProtocol is the TCP the tunnel's netstack runs. gVisor defaults to Reno,
// whose additive growth (one MSS per RTT) never fills a path with real spare
// bandwidth but an RTT of tens to hundreds of ms - exactly what a tunnel over
// a relay looks like. CUBIC keeps every other default and ramps the window
// up much faster. OPENFLUX_TCP_CC=reno restores the old behavior.
func tcpProtocol() stack.TransportProtocolFactory {
	if os.Getenv("OPENFLUX_TCP_CC") == "reno" {
		return tcp.NewProtocol
	}
	return tcp.NewProtocolCUBIC
}
