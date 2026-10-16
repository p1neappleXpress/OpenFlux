//go:build !linux && !android

package transport

import "net"

// setNotSentLowat is Linux's TCP_NOTSENT_LOWAT; elsewhere the socket is left
// as the system tunes it.
func setNotSentLowat(net.Conn, int) {}
