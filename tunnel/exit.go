package tunnel

import (
	"fmt"
	"runtime"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/tunnel/l3"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

type ExitNode interface {
	Start() error
	Stop() error
	Mode() string
}

func NewExitNode(trans transport.Transport, mode string) (ExitNode, error) {
	return NewExitNodeWithUpstream(trans, mode, nil)
}

func NewExitNodeWithUpstream(trans transport.Transport, mode string, upstream *UpstreamProxy) (ExitNode, error) {
	switch mode {
	case "l3":
		node, err := l3.New(trans)
		if err != nil {
			return nil, fmt.Errorf("l3: %w", err)
		}
		if upstream != nil {
			utils.Debugf("[EXIT] upstream proxy ignored in l3 mode (TCP is forwarded at L3, not dialed)")
		}
		utils.Debugf("[EXIT] using L3 (platform=%s)", runtime.GOOS)
		return node, nil
	case "l4", "proxy", "":
		// "proxy" is a deprecated alias kept for one release.
		return newProxyExitWithUpstream(trans, upstream), nil
	default:
		return nil, fmt.Errorf("unknown exit mode %q (want l3|l4)", mode)
	}
}
