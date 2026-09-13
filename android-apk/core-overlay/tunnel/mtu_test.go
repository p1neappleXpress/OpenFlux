package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// Use the actual OpenFlux endpoint and DialTCP path, not Android's separate
// TUN bridge. The peer advertises a 1500-byte link; our client must still limit
// its own segmentation and SYN MSS to OPENFLUX_MTU.
func TestCoreTCPRespectsMTU(t *testing.T) {
	for _, test := range []struct {
		env string
		mtu int
	}{{"", 1200}, {"576", 576}, {"1280", 1280}, {"1500", 1500}} {
		t.Run(fmt.Sprintf("env=%q,mtu=%d", test.env, test.mtu), func(t *testing.T) {
			t.Setenv("OPENFLUX_MTU", test.env)
			ctx, cancel := context.WithCancel(context.Background())
			newStack := func() *stack.Stack {
				return stack.New(stack.Options{
					NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
					TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
				})
			}
			client := &TCPTunnel{gvisorStack: newStack()}
			endpoint := NewTunnelLinkEndpoint()
			client.tunnelEP = endpoint
			outgoing := make(chan []byte, 1024)
			endpoint.onOutgoingPacket = func(p []byte) {
				select {
				case outgoing <- append([]byte(nil), p...):
				case <-ctx.Done():
				}
			}
			if e := client.gvisorStack.CreateNIC(1, endpoint); e != nil {
				t.Fatal(e)
			}
			client.setupClient(1)
			remote := newStack()
			link := channel.New(1024, 1500, "")
			if e := remote.CreateNIC(1, link); e != nil {
				t.Fatal(e)
			}
			address := tcpip.AddrFrom4([4]byte{203, 0, 113, 7})
			if e := remote.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
				AddressWithPrefix: tcpip.AddressWithPrefix{Address: address, PrefixLen: 24}}, stack.AddressProperties{}); e != nil {
				t.Fatal(e)
			}
			remote.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
			listener, err := gonet.ListenTCP(remote, tcpip.FullAddress{NIC: 1, Addr: address, Port: 443}, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			var pumps sync.WaitGroup
			var largest, clientMSS atomic.Int64
			pumps.Add(2)
			go func() {
				defer pumps.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case p := <-outgoing:
						size := int64(len(p))
						for old := largest.Load(); size > old; old = largest.Load() {
							if largest.CompareAndSwap(old, size) {
								break
							}
						}
						if len(p) > test.mtu {
							t.Errorf("core emitted %d bytes, MTU=%d", len(p), test.mtu)
						}
						ihl := int(p[0]&15) * 4
						if len(p) >= ihl+20 && p[ihl+13]&2 != 0 {
							end := ihl + int(p[ihl+12]>>4)*4
							for i := ihl + 20; i < end; {
								kind := p[i]
								if kind == 0 {
									break
								}
								if kind == 1 {
									i++
									continue
								}
								if i+1 >= end || p[i+1] < 2 || i+int(p[i+1]) > end {
									break
								}
								if kind == 2 && p[i+1] == 4 {
									clientMSS.Store(int64(binary.BigEndian.Uint16(p[i+2 : i+4])))
								}
								i += int(p[i+1])
							}
						}
						pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(p)})
						link.InjectInbound(ipv4.ProtocolNumber, pb)
						pb.DecRef()
					}
				}
			}()
			go func() {
				defer pumps.Done()
				for {
					p := link.ReadContext(ctx)
					if p == nil {
						return
					}
					v := p.ToView()
					endpoint.InjectInbound(v.AsSlice())
					v.Release()
					p.DecRef()
				}
			}()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, e := listener.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(8 * time.Second))
				io.Copy(c, c)
			}()
			defer func() {
				cancel()
				listener.Close()
				client.gvisorStack.Close()
				remote.Close()
				link.Close()
				pumps.Wait()
				client.gvisorStack.Wait()
				remote.Wait()
				<-done
			}()
			type dialResult struct {
				c   net.Conn
				err error
			}
			dialed := make(chan dialResult, 1)
			go func() { c, e := client.DialTCP("203.0.113.7:443"); dialed <- dialResult{c, e} }()
			var c net.Conn
			select {
			case r := <-dialed:
				if r.err != nil {
					t.Fatal(r.err)
				}
				c = r.c
			case <-time.After(5 * time.Second):
				t.Fatal("core TCP dial timed out")
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(8 * time.Second))
			payload := bytes.Repeat([]byte("OpenFlux MTU regression\n"), 16000)
			written := make(chan error, 1)
			go func() {
				_, e := io.Copy(c, bytes.NewReader(payload))
				c.(interface{ CloseWrite() error }).CloseWrite()
				written <- e
			}()
			reply, e := io.ReadAll(c)
			if e != nil {
				t.Fatal(e)
			}
			if e = <-written; e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(payload, reply) {
				t.Fatal("payload lost or corrupted")
			}
			if mss := clientMSS.Load(); mss != int64(test.mtu-40) {
				t.Fatalf("SYN MSS=%d, expected %d", mss, test.mtu-40)
			}
			if largest.Load() > int64(test.mtu) || largest.Load() < int64(test.mtu-40) {
				t.Fatal("unexpected packet sizes", largest.Load())
			}
			t.Logf("largest IP packet=%d; SYN MSS=%d; payload=%d", largest.Load(), clientMSS.Load(), len(payload))
		})
	}
}

func TestInvalidCoreMTURejected(t *testing.T) {
	for _, value := range []string{"575", "1501", "abc", "-1"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			t.Setenv("OPENFLUX_MTU", value)
			defer func() {
				if recover() == nil {
					t.Error("invalid MTU silently accepted")
				}
			}()
			NewTunnelLinkEndpoint()
		})
	}
}
