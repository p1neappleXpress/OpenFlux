//go:build linux

package l3

import (
	"fmt"
	"net"
	"sync"
	"syscall"

	"openflux/utils"
)

type rawBackend struct {
	sendFd  int
	recvFds []int
	egress  [4]byte

	closeOnce sync.Once
	closed    chan struct{}
	recvMu    sync.Mutex
	fdMu      sync.RWMutex
}

func newBackend() (L3Backend, error) {
	localIPMu.Lock()
	egress := localIPOverride
	hasOverride := hasLocalIPOverride
	localIPMu.Unlock()
	if !hasOverride {
		var err error
		egress, err = detectEgressIPv4()
		if err != nil {
			return nil, err
		}
	}

	sendFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return nil, fmt.Errorf("l3: send socket: %w (need root or CAP_NET_RAW)", err)
	}
	if err := syscall.SetsockoptInt(sendFd, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 1); err != nil {
		syscall.Close(sendFd)
		return nil, fmt.Errorf("l3: IP_HDRINCL: %w", err)
	}
	// Large send buffer: SOCK_RAW with IP_HDRINCL does not get kernel
	// auto-tuning, so the default (208 KiB) caps BDP and causes drops
	// at RTT ~100ms and >30 Mbps.
	syscall.SetsockoptInt(sendFd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 16*1024*1024)

	var recvFds []int
	for _, proto := range []int{syscall.IPPROTO_TCP, syscall.IPPROTO_UDP} {
		recvFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, proto)
		if err != nil {
			for _, fd := range recvFds {
				syscall.Close(fd)
			}
			syscall.Close(sendFd)
			return nil, fmt.Errorf("l3: recv socket protocol %d: %w (need root or CAP_NET_RAW)", proto, err)
		}
		syscall.SetsockoptInt(recvFd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 16*1024*1024)
		recvFds = append(recvFds, recvFd)
		// close(2) alone does not reliably wake a blocking recvfrom on Linux.
		if err := syscall.SetsockoptTimeval(recvFd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &syscall.Timeval{Sec: 1}); err != nil {
			for _, fd := range recvFds {
				_ = syscall.Close(fd)
			}
			_ = syscall.Close(sendFd)
			return nil, fmt.Errorf("l3: receive timeout: %w", err)
		}
	}
	if err := syscall.SetsockoptTimeval(sendFd, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &syscall.Timeval{Sec: 1}); err != nil {
		for _, fd := range recvFds {
			_ = syscall.Close(fd)
		}
		_ = syscall.Close(sendFd)
		return nil, fmt.Errorf("l3: send timeout: %w", err)
	}

	b := &rawBackend{
		sendFd:  sendFd,
		recvFds: recvFds,
		egress:  egress,
		closed:  make(chan struct{}),
	}
	utils.Debugf("[L3/linux] raw backend ready, egress=%s", ipStr(ipU32(egress)))
	return b, nil
}

func (b *rawBackend) EgressIP() [4]byte { return b.egress }

func (b *rawBackend) Send(pkt []byte) error {
	var ok bool
	pkt, ok = sliceIPv4(pkt)
	if !ok {
		return fmt.Errorf("l3: invalid IPv4 packet")
	}
	b.fdMu.RLock()
	defer b.fdMu.RUnlock()
	select {
	case <-b.closed:
		return net.ErrClosed
	default:
	}
	var dst [4]byte
	copy(dst[:], pkt[16:20])
	addr := &syscall.SockaddrInet4{Addr: dst}
	return syscall.Sendto(b.sendFd, pkt, 0, addr)
}

func (b *rawBackend) Recv(cb func([]byte)) {
	for _, fd := range b.recvFds {
		go b.recvLoop(fd, cb)
	}
}

func (b *rawBackend) recvLoop(fd int, cb func([]byte)) {
	buf := make([]byte, 65535)
	for {
		b.fdMu.RLock()
		select {
		case <-b.closed:
			b.fdMu.RUnlock()
			return
		default:
		}
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		b.fdMu.RUnlock()
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				continue
			}
			select {
			case <-b.closed:
				return
			default:
			}
			utils.Debugf("[L3/linux] recv: %v", err)
			continue
		}
		if n < 28 || buf[0]>>4 != 4 || (buf[9] != 6 && buf[9] != 17) {
			continue
		}
		if buf[16] != b.egress[0] || buf[17] != b.egress[1] ||
			buf[18] != b.egress[2] || buf[19] != b.egress[3] {
			continue
		}
		cp := make([]byte, n)
		copy(cp, buf[:n])
		b.recvMu.Lock()
		cb(cp)
		b.recvMu.Unlock()
	}
}

func (b *rawBackend) Close() error {
	b.closeOnce.Do(func() {
		close(b.closed)
		b.fdMu.Lock()
		defer b.fdMu.Unlock()
		syscall.Close(b.sendFd)
		for _, fd := range b.recvFds {
			syscall.Close(fd)
		}
	})
	return nil
}

func detectEgressIPv4() ([4]byte, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return [4]byte{}, fmt.Errorf("l3: detect egress: %w", err)
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	if ip == nil {
		return [4]byte{}, fmt.Errorf("l3: no IPv4 egress address")
	}
	var out [4]byte
	copy(out[:], ip)
	return out, nil
}
