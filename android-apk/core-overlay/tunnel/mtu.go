package tunnel

import (
	"fmt"
	"log"
	"os"
	"strconv"
)

// OpenFlux creates TCP packets again after receiving SOCKS5 streams. Android's
// VpnService MTU alone does not constrain this independent TCP stack.
func openFluxLinkMTU() uint32 {
	value := os.Getenv("OPENFLUX_MTU")
	mtu := 1200
	if value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 576 || n > 1500 {
			panic(fmt.Sprintf("OPENFLUX_MTU must be between 576 and 1500, got %q", value))
		}
		mtu = n
	}
	log.Printf("[TUNNEL] OpenFlux MTU=%d", mtu)
	return uint32(mtu)
}
