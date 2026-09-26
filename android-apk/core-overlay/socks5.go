// Adapted from p1neappleXpress/OpenFlux socks5/socks5.go.
// Android client overlay: framed reads and TCP half-close propagation.
package socks5

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"
	"universal-bypass-tool/utils"
)

type Dialer interface {
	DialTCP(address string) (net.Conn, error)
}
type SOCKS5Server struct {
	listenAddr string
	dialer     Dialer
}

func NewSOCKS5Server(addr string, dialer Dialer) *SOCKS5Server { return &SOCKS5Server{addr, dialer} }
func (s *SOCKS5Server) Start() error {
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	utils.Debugf("[SOCKS5] Listening on %s", s.listenAddr)
	for {
		c, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.handleConnection(c)
	}
}
func (s *SOCKS5Server) handleConnection(client net.Conn) {
	defer client.Close()
	client.SetDeadline(time.Now().Add(30 * time.Second))
	var hello [2]byte
	if _, err := io.ReadFull(client, hello[:]); err != nil || hello[0] != 5 || hello[1] == 0 {
		return
	}
	methods := make([]byte, int(hello[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	noAuth := false
	for _, m := range methods {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		client.Write([]byte{5, 255})
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	var request [4]byte
	if _, err := io.ReadFull(client, request[:]); err != nil {
		return
	}
	failure := func(code byte) { client.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}) }
	if request[0] != 5 || request[2] != 0 {
		return
	}
	if request[1] != 1 {
		failure(7)
		return
	}
	var host string
	switch request[3] {
	case 1:
		var addr [4]byte
		if _, err := io.ReadFull(client, addr[:]); err != nil {
			return
		}
		host = net.IP(addr[:]).String()
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(client, size[:]); err != nil || size[0] == 0 {
			return
		}
		name := make([]byte, int(size[0]))
		if _, err := io.ReadFull(client, name); err != nil {
			return
		}
		host = string(name)
	default:
		failure(8)
		return
	}
	var port [2]byte
	if _, err := io.ReadFull(client, port[:]); err != nil {
		return
	}
	address := net.JoinHostPort(host, strconv.Itoa(int(port[0])<<8|int(port[1])))
	target, err := s.dialer.DialTCP(address)
	if err != nil {
		utils.Debugf("[SOCKS5] Dial failed: %v", err)
		failure(4)
		return
	}
	defer target.Close()
	if _, err = client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	client.SetDeadline(time.Time{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, err := io.Copy(target, client)
		if c, ok := target.(interface{ CloseWrite() error }); ok {
			c.CloseWrite()
		} else {
			target.Close()
		}
		if err != nil {
			target.Close()
		}
	}()
	_, err = io.Copy(client, target)
	if c, ok := client.(interface{ CloseWrite() error }); ok {
		c.CloseWrite()
	} else {
		client.Close()
	}
	if err != nil {
		client.Close()
	}
	client.SetReadDeadline(time.Now().Add(30 * time.Second))
	workers.Wait()
}
