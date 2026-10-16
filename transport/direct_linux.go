//go:build linux || android

package transport

import (
	"net"
	"syscall"
)

// tcpNotSentLowat is TCP_NOTSENT_LOWAT (linux/tcp.h): the most data the kernel
// keeps queued in a socket that has not been sent yet. Without it the send
// buffer autotunes to megabytes and everything written ahead of a fresh packet
// is latency; with it a write blocks once that much waits, and the tunnel's own
// queues (which are bounded) and the stack above feel the pressure instead.
const tcpNotSentLowat = 25

func setNotSentLowat(c net.Conn, bytes int) {
	tc, ok := c.(*net.TCPConn)
	if !ok || bytes <= 0 {
		return
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpNotSentLowat, bytes)
	})
}
