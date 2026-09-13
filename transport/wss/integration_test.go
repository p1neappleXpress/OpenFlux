package wss

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"universal-bypass-tool/socks5"

	"github.com/gorilla/websocket"
	xproxy "golang.org/x/net/proxy"
)

const testAuthToken = "test-openflux-token-0123456789-abcdef"

type testWSSExit struct {
	server      *Server
	endpoint    string
	caFile      string
	serveDone   chan error
	shutdown    sync.Once
	shutdownErr error
}

func startTestWSSExit(t *testing.T, resolver ipResolver, dial dialContextFunc) *testWSSExit {
	t.Helper()
	certFile, keyFile, caFile := writeTestCertificates(t)
	server, err := NewServer(ServerConfig{
		CertFile:         certFile,
		KeyFile:          keyFile,
		AuthToken:        testAuthToken,
		DialTimeout:      time.Second,
		HandshakeTimeout: time.Second,
		RequestTimeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolver != nil {
		server.policy = newTestPolicy(t, resolver, nil)
	}
	if dial != nil {
		server.dialContext = dial
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exit := &testWSSExit{
		server:    server,
		endpoint:  "wss://" + listener.Addr().String() + TunnelPath,
		caFile:    caFile,
		serveDone: make(chan error, 1),
	}
	go func() {
		exit.serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		if err := exit.stop(); err != nil {
			t.Errorf("shutdown test WSS exit: %v", err)
		}
	})
	return exit
}

func (e *testWSSExit) newClient(t *testing.T, token string) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		Endpoint:         e.endpoint,
		AuthToken:        token,
		CAFile:           e.caFile,
		DialTimeout:      time.Second,
		HandshakeTimeout: time.Second,
		RequestTimeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (e *testWSSExit) stop() error {
	e.shutdown.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		e.shutdownErr = e.server.Shutdown(ctx)
		if serveErr := <-e.serveDone; serveErr != nil {
			e.shutdownErr = errors.Join(e.shutdownErr, serveErr)
		}
	})
	return e.shutdownErr
}

func writeTestCertificates(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "OpenFlux test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "OpenFlux test exit"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCertificate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	certFile = filepath.Join(directory, "exit-cert.pem")
	keyFile = filepath.Join(directory, "exit-key.pem")
	caFile = filepath.Join(directory, "ca.pem")
	certificatePEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
	)
	if err := os.WriteFile(certFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, caFile
}

func echoPipeDialer(expectedAddress string) dialContextFunc {
	return func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp4" || address != expectedAddress {
			return nil, fmt.Errorf("unexpected dial %s %s", network, address)
		}
		client, peer := net.Pipe()
		go func() {
			_, _ = io.Copy(peer, peer)
			_ = peer.Close()
		}()
		return client, nil
	}
}

func TestWSSClientServerRemoteDNSAndLargeStream(t *testing.T) {
	resolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	exit := startTestWSSExit(t, resolver, echoPipeDialer("8.8.8.8:443"))
	conn, err := exit.newClient(t, testAuthToken).DialTCP("echo.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload := bytes.Repeat([]byte("openflux"), maxFramePayload/4)
	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		writeDone <- err
	}()
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatal("echo response differs from payload")
	}
	if resolver.calls != 1 || resolver.network != "ip4" || resolver.host != "echo.example" {
		t.Fatalf("resolver calls=%d network=%q host=%q", resolver.calls, resolver.network, resolver.host)
	}
}

func TestSOCKS5ThroughWSSExit(t *testing.T) {
	resolver := &fakeResolver{addresses: []netip.Addr{netip.MustParseAddr("8.8.8.8")}}
	exit := startTestWSSExit(t, resolver, echoPipeDialer("8.8.8.8:443"))
	wssClient := exit.newClient(t, testAuthToken)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	socksServer := socks5.NewSOCKS5Server(listener.Addr().String(), wssClient)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- socksServer.Serve(listener)
	}()
	t.Cleanup(func() {
		if err := socksServer.Close(); err != nil {
			t.Errorf("close SOCKS5 server: %v", err)
		}
		if err := <-serveDone; err != nil {
			t.Errorf("serve SOCKS5: %v", err)
		}
	})

	proxyDialer, err := xproxy.SOCKS5("tcp", listener.Addr().String(), nil, &net.Dialer{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := proxyDialer.Dial("tcp", "echo.example:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("full-path"), 12*1024)
	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		writeDone <- err
	}()
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatal("SOCKS5/WSS echo response differs from payload")
	}
}

func TestWSSRejectsWrongBearerToken(t *testing.T) {
	exit := startTestWSSExit(t, nil, nil)
	_, err := exit.newClient(t, strings.Repeat("x", minimumAuthTokenLength)).DialTCP("8.8.8.8:443")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("DialTCP() error = %v, want HTTP 401", err)
	}
}

func TestWSSRejectsMissingSubprotocolAndBrowserOrigin(t *testing.T) {
	exit := startTestWSSExit(t, nil, nil)
	client := exit.newClient(t, testAuthToken)

	tests := []struct {
		name        string
		subprotocol bool
		origin      string
		status      int
	}{
		{name: "missing subprotocol", status: http.StatusBadRequest},
		{name: "browser origin", subprotocol: true, origin: "https://example.com", status: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := *client.dialer
			if !test.subprotocol {
				dialer.Subprotocols = nil
			}
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer "+testAuthToken)
			if test.origin != "" {
				headers.Set("Origin", test.origin)
			}
			conn, response, err := dialer.Dial(client.endpoint, headers)
			if conn != nil {
				_ = conn.Close()
			}
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if err == nil || response == nil || response.StatusCode != test.status {
				t.Fatalf("Dial() error=%v status=%v, want status %d", err, responseStatus(response), test.status)
			}
		})
	}
}

func TestWSSRejectsMalformedAndUnsupportedControlRequests(t *testing.T) {
	exit := startTestWSSExit(t, nil, nil)
	client := exit.newClient(t, testAuthToken)

	tests := []struct {
		name string
		data []byte
		code string
	}{
		{name: "malformed", data: []byte(`{"version":`), code: responseBadRequest},
		{name: "unsupported version", data: []byte(`{"version":2,"command":"CONNECT","target":"8.8.8.8:443"}`), code: responseUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer "+testAuthToken)
			conn, response, err := client.dialer.Dial(client.endpoint, headers)
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.WriteMessage(websocket.TextMessage, test.data); err != nil {
				t.Fatal(err)
			}
			var got connectResponse
			if err := readControl(conn, &got); err != nil {
				t.Fatal(err)
			}
			if got.OK || got.Code != test.code {
				t.Fatalf("response = %#v, want code %q", got, test.code)
			}
		})
	}
}

func TestWSSReportsPolicyResolveAndConnectFailures(t *testing.T) {
	t.Run("policy", func(t *testing.T) {
		exit := startTestWSSExit(t, nil, nil)
		_, err := exit.newClient(t, testAuthToken).DialTCP("127.0.0.1:80")
		assertRemoteCode(t, err, responsePolicyDenied)
	})
	t.Run("resolve", func(t *testing.T) {
		exit := startTestWSSExit(t, &fakeResolver{}, nil)
		_, err := exit.newClient(t, testAuthToken).DialTCP("missing.example:80")
		assertRemoteCode(t, err, responseResolveFailed)
	})
	t.Run("connect", func(t *testing.T) {
		exit := startTestWSSExit(t, nil, func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("refused")
		})
		_, err := exit.newClient(t, testAuthToken).DialTCP("8.8.8.8:443")
		assertRemoteCode(t, err, responseConnectFailed)
	})
}

func TestWSSConcurrentStreams(t *testing.T) {
	exit := startTestWSSExit(t, nil, echoPipeDialer("8.8.8.8:443"))
	client := exit.newClient(t, testAuthToken)
	const streamCount = 8
	result := make(chan error, streamCount)
	for index := 0; index < streamCount; index++ {
		go func(index int) {
			conn, err := client.DialTCP("8.8.8.8:443")
			if err != nil {
				result <- err
				return
			}
			defer conn.Close()
			message := []byte(fmt.Sprintf("stream-%d", index))
			if _, err := conn.Write(message); err != nil {
				result <- err
				return
			}
			response := make([]byte, len(message))
			if _, err := io.ReadFull(conn, response); err != nil {
				result <- err
				return
			}
			if !bytes.Equal(response, message) {
				result <- errors.New("echo mismatch")
				return
			}
			result <- nil
		}(index)
	}
	for index := 0; index < streamCount; index++ {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
}

func TestWSSShutdownClosesActiveStream(t *testing.T) {
	exit := startTestWSSExit(t, nil, echoPipeDialer("8.8.8.8:443"))
	conn, err := exit.newClient(t, testAuthToken).DialTCP("8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	if err := exit.stop(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("active stream remained readable after shutdown")
	}
	_ = conn.Close()
}

func assertRemoteCode(t *testing.T, err error, code string) {
	t.Helper()
	var remoteError *RemoteError
	if !errors.As(err, &remoteError) || remoteError.Code != code {
		t.Fatalf("error = %v, want RemoteError code %q", err, code)
	}
}

func responseStatus(response *http.Response) any {
	if response == nil {
		return nil
	}
	return response.StatusCode
}
