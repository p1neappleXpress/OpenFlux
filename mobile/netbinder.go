package mobile

import "github.com/p1neappleXpress/OpenFlux/netbind"

// NetworkBinder is the app's way to put a socket on one network: Android
// binds it with Network.bindSocket to the mobile data or Wi-Fi network it
// keeps up, so a bonded Session's carriers leave through both at once.
type NetworkBinder interface {
	// BindSocket binds socket fd to network ("cellular", "wifi"), or
	// fails when that network is not up.
	BindSocket(network string, fd int) error
}

// SetNetworkBinder installs the app's binder; nil removes it (carriers
// with a network then fail to dial).
func SetNetworkBinder(b NetworkBinder) {
	if b == nil {
		netbind.SetNetworkBinder(nil)
		return
	}
	netbind.SetNetworkBinder(func(network string, fd uintptr) error {
		return b.BindSocket(network, int(fd))
	})
}
