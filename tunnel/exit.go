package tunnel

import (
	"fmt"
	"runtime"

	"openflux/transport"
	"openflux/tunnel/l3"
	"openflux/utils"
)

type ExitNode interface {
	Start() error
	Stop() error
	Mode() string
}

func NewExitNode(trans transport.Transport, mode string, upstreamProxy ...string) (ExitNode, error) {
	proxy := ""
	if len(upstreamProxy) > 0 {
		proxy = upstreamProxy[0]
	}
	switch mode {
	case "l3":
		if proxy != "" {
			return nil, fmt.Errorf("l3 mode cannot be used with upstream proxy, use mode l4/proxy")
		}
		node, err := l3.New(trans)
		if err != nil {
			return nil, fmt.Errorf("l3: %w", err)
		}
		utils.Debugf("[EXIT] using L3 (platform=%s)", runtime.GOOS)
		return node, nil
	case "l4", "proxy", "":
		// "proxy" is a deprecated alias kept for one release.
		return newProxyExit(trans, proxy), nil
	default:
		return nil, fmt.Errorf("unknown exit mode %q (want l3|l4)", mode)
	}
}
