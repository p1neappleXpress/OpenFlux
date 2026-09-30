package mobile

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
)

// The phone as an l4 exit: a CLI-style Session client reaches it over
// direct and its HTTP request leaves through the phone.
func TestExitServesSessionClient(t *testing.T) {
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via phone exit")
	}))
	ln, err := net.Listen("tcp4", net.JoinHostPort(lanIPv4(t).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	web.Listener = ln
	web.Start()
	defer web.Close()

	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := probe.Addr().String()
	probe.Close()

	specs := fmt.Sprintf(`[{"name":"direct","type":"direct","url":"","priority":100,"params":{"dial":%q}}]`, addr)
	if msg := StartSessionExit(specs, testSecret); msg != "" {
		t.Fatal(msg)
	}
	defer StopExit()
	if !ExitIsRunning() || ExitIsConnected() {
		t.Fatal("exit should run and wait for a client")
	}

	client, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	dcfg := transport.DefaultDirectConfig()
	dcfg.DialAddr = addr
	// Context: no transport has a URL, so both sides fall back to "http://#".
	if err := client.AddTransport("direct", transport.NewDirectTransport(transport.DefaultConfig(), dcfg), testSecret, "http://#", 100); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("handshake with the phone exit: %v", err)
	}
	defer client.Stop()
	waitUntil(t, "the exit to see the client", ExitIsConnected)

	tun := tunnel.NewTCPTunnel(client, false)
	defer tun.Close()
	conn, err := tun.DialTCP(strings.TrimPrefix(web.URL, "http://"))
	if err != nil {
		t.Fatalf("dial through the phone exit: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	body, _ := io.ReadAll(conn)
	if !strings.Contains(string(body), "via phone exit") {
		t.Fatalf("response = %q", body)
	}
	if ExitBytesReceived() == 0 || ExitBytesSent() == 0 {
		t.Fatal("exit traffic counters stayed at zero")
	}
	if CurrentTransport() != "direct" {
		t.Fatalf("CurrentTransport = %q", CurrentTransport())
	}
}
