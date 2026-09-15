// Package bridge forwards Android TUN IPv4/TCP through OpenFlux's local SOCKS5
// listener. DNS UDP is translated to DNS TCP; no Internet socket is opened here.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	MTU          = 1200
	DNSAddress   = "10.77.0.2"
	PrimaryDNS   = "1.1.1.1:53"
	SecondaryDNS = "9.9.9.9:53"
)

type Session struct {
	mtu                                        int
	ctx                                        context.Context
	cancel                                     context.CancelFunc
	tun                                        *os.File
	link                                       *channel.Endpoint
	stack                                      *stack.Stack
	dialer                                     proxy.ContextDialer
	stopOnce                                   sync.Once
	writeMu                                    sync.Mutex
	mu                                         sync.Mutex
	closing                                    bool
	conns                                      map[net.Conn]struct{}
	workers                                    sync.WaitGroup
	dnsSlots                                   chan struct{}
	tcpSlots                                   chan struct{}
	fatal                                      error
	lastError                                  string
	up, down, tcpOK, dnsOK, dnsFailed, dropped atomic.Uint64
}

// New duplicates fd: Java retains ownership of the original ParcelFileDescriptor.
func New(fd, port, mtu int) (*Session, error) {
	if mtu < 576 || mtu > 1500 {
		return nil, errors.New("MTU must be between 576 and 1500")
	}
	if port < 1024 || port > 65535 {
		return nil, errors.New("invalid SOCKS port")
	}
	cd := &localSOCKS{address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, fmt.Errorf("dup TUN: %w", err)
	}
	unix.CloseOnExec(dup)
	if err = unix.SetNonblock(dup, true); err != nil {
		unix.Close(dup)
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "openflux-tun")
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{mtu: mtu, ctx: ctx, cancel: cancel, tun: f, dialer: cd,
		conns: make(map[net.Conn]struct{}), dnsSlots: make(chan struct{}, 32),
		tcpSlots: make(chan struct{}, 256)}
	s.link = channel.New(512, uint32(mtu), "")
	s.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	if e := s.stack.CreateNIC(1, s.link); e != nil {
		s.Stop()
		return nil, fmt.Errorf("create NIC: %s", e)
	}
	if e := s.stack.SetPromiscuousMode(1, true); e != nil {
		s.Stop()
		return nil, fmt.Errorf("promiscuous mode: %s", e)
	}
	if e := s.stack.SetSpoofing(1, true); e != nil {
		s.Stop()
		return nil, fmt.Errorf("spoofing: %s", e)
	}
	s.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	fwd := tcp.NewForwarder(s.stack, 256*1024, 256, s.forwardTCP)
	s.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	return s, nil
}

func (s *Session) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.workers.Add(1)
	return true
}

func (s *Session) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Session) release(c net.Conn) {
	c.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Session) noteError(err error) {
	if err == nil || s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.lastError = err.Error()
	if len(s.lastError) > 300 {
		s.lastError = s.lastError[:300]
	}
	s.mu.Unlock()
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	if !s.closing && s.fatal == nil {
		s.fatal = err
	}
	s.mu.Unlock()
	s.Stop()
}

// Stop cancels active dials, copies and pollable TUN I/O. It does not wait for Run.
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		connections := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			connections = append(connections, c)
		}
		s.mu.Unlock()
		s.cancel()
		s.tun.Close()
		for _, c := range connections {
			c.Close()
		}
		s.link.Close()
		s.stack.Close()
	})
}

// Run is called once per session, on a Java worker thread.
func (s *Session) Run() error {
	var ioWorkers sync.WaitGroup
	ioWorkers.Add(2)
	go func() { defer ioWorkers.Done(); s.readPackets() }()
	go func() { defer ioWorkers.Done(); s.writePackets() }()
	<-s.ctx.Done()
	s.Stop()
	ioWorkers.Wait()
	s.workers.Wait()
	s.stack.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

func (s *Session) readPackets() {
	buf := make([]byte, 65536)
	for {
		n, err := s.tun.Read(buf)
		if err != nil {
			s.fail(fmt.Errorf("read TUN: %w", err))
			return
		}
		if n == 0 {
			s.fail(io.EOF)
			return
		}
		s.up.Add(uint64(n))
		p := buf[:n]
		if len(p) < 20 || p[0]>>4 != 4 {
			s.dropped.Add(1)
			continue
		}
		switch p[9] {
		case 6:
			pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), p...))})
			s.link.InjectInbound(ipv4.ProtocolNumber, pb)
			pb.DecRef()
		case 17:
			q, ok := parseDNSPacket(p)
			if !ok {
				s.dropped.Add(1)
				continue
			}
			select {
			case s.dnsSlots <- struct{}{}:
				if !s.begin() {
					<-s.dnsSlots
					return
				}
				go func() {
					defer s.workers.Done()
					defer func() { <-s.dnsSlots }()
					s.forwardDNS(q)
				}()
			default:
				s.dropped.Add(1)
			}
		default:
			s.dropped.Add(1)
		}
	}
}

func (s *Session) sendPacket(p []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.tun.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.fail(fmt.Errorf("write TUN: %w", err))
		return err
	}
	s.down.Add(uint64(n))
	return nil
}

func (s *Session) writePackets() {
	for {
		pb := s.link.ReadContext(s.ctx)
		if pb == nil {
			return
		}
		v := pb.ToView()
		err := s.sendPacket(v.AsSlice())
		v.Release()
		pb.DecRef()
		if err != nil {
			return
		}
	}
}

func (s *Session) forwardTCP(r *tcp.ForwarderRequest) {
	if !s.begin() {
		r.Complete(true)
		return
	}
	defer s.workers.Done()
	select {
	case s.tcpSlots <- struct{}{}:
		defer func() { <-s.tcpSlots }()
	default:
		r.Complete(true)
		return
	}
	id := r.ID()
	if id.LocalAddress.BitLen() != 32 {
		r.Complete(true)
		return
	}
	addr := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))
	if addr == DNSAddress+":53" {
		addr = PrimaryDNS
	}
	// Accept the local TCP handshake promptly; connecting to the exit can take time.
	wq := new(waiter.Queue)
	ep, err := r.CreateEndpoint(wq)
	if err != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(wq, ep)
	if !s.track(local) {
		return
	}
	defer s.release(local)
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	remote, dialErr := s.dialer.DialContext(ctx, "tcp", addr)
	cancel()
	if dialErr != nil {
		s.noteError(dialErr)
		return
	}
	if !s.track(remote) {
		return
	}
	defer s.release(remote)
	s.tcpOK.Add(1)
	// Preserve half-close so request EOF does not discard a pending response.
	done := make(chan error, 1)
	go func() {
		_, e := io.Copy(remote, local)
		if c, ok := remote.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		}
		if e != nil {
			remote.Close()
		}
		done <- e
	}()
	_, copyErr := io.Copy(local, remote)
	local.CloseWrite()
	if copyErr != nil {
		local.Close()
	}
	// A peer that has closed its response must not keep an idle copier forever.
	local.SetReadDeadline(time.Now().Add(30 * time.Second))
	<-done
}

func (s *Session) Stats() string {
	s.mu.Lock()
	last := s.lastError
	active := len(s.conns) / 2
	s.mu.Unlock()
	b, _ := json.Marshal(struct {
		Up        uint64 `json:"up"`
		Down      uint64 `json:"down"`
		TCP       uint64 `json:"tcp"`
		DNS       uint64 `json:"dns"`
		DNSFailed uint64 `json:"dns_failed"`
		Dropped   uint64 `json:"dropped"`
		Active    int    `json:"active"`
		Error     string `json:"error"`
	}{s.up.Load(), s.down.Load(), s.tcpOK.Load(), s.dnsOK.Load(), s.dnsFailed.Load(), s.dropped.Load(), active, last})
	return string(b)
}
