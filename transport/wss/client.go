package wss

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	xproxy "golang.org/x/net/proxy"
)

const minimumAuthTokenLength = 32

type ClientConfig struct {
	Endpoint         string
	AuthToken        string
	CAFile           string
	ServerName       string
	ProxyURL         string
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	RequestTimeout   time.Duration
}

// Client is a SOCKS-compatible TCP dialer that opens one authenticated WSS
// connection per target connection.
type Client struct {
	endpoint       string
	authToken      string
	dialer         *websocket.Dialer
	requestTimeout time.Duration
}

func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := parseEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	if err := validateClientToken(config.AuthToken); err != nil {
		return nil, err
	}

	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = 10 * time.Second
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 10 * time.Second
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: config.ServerName,
	}
	if config.CAFile != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		pemData, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read WSS CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pemData) {
			return nil, errors.New("WSS CA file does not contain a valid certificate")
		}
		tlsConfig.RootCAs = roots
	}

	baseDialer := &net.Dialer{
		Timeout:   config.DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	wsDialer := &websocket.Dialer{
		NetDialContext:    baseDialer.DialContext,
		HandshakeTimeout:  config.HandshakeTimeout,
		TLSClientConfig:   tlsConfig,
		Subprotocols:      []string{Subprotocol},
		ReadBufferSize:    32 * 1024,
		WriteBufferSize:   32 * 1024,
		EnableCompression: false,
	}

	if config.ProxyURL != "" {
		proxyURL, err := url.Parse(config.ProxyURL)
		if err != nil || proxyURL.Hostname() == "" {
			return nil, errors.New("invalid WSS proxy URL")
		}
		switch proxyURL.Scheme {
		case "http":
			wsDialer.Proxy = http.ProxyURL(proxyURL)
		case "socks5":
			if proxyURL.Port() == "" {
				return nil, errors.New("SOCKS5 proxy URL requires an explicit port")
			}
			var auth *xproxy.Auth
			if proxyURL.User != nil {
				password, _ := proxyURL.User.Password()
				auth = &xproxy.Auth{User: proxyURL.User.Username(), Password: password}
			}
			proxyDialer, err := xproxy.SOCKS5("tcp", proxyURL.Host, auth, baseDialer)
			if err != nil {
				return nil, errors.New("configure SOCKS5 bootstrap proxy")
			}
			wsDialer.Proxy = nil
			wsDialer.NetDialContext = contextDialFunc(proxyDialer)
		default:
			return nil, errors.New("WSS proxy scheme must be http or socks5")
		}
	}

	return &Client{
		endpoint:       endpoint.String(),
		authToken:      config.AuthToken,
		dialer:         wsDialer,
		requestTimeout: config.RequestTimeout,
	}, nil
}

func (c *Client) DialTCP(address string) (net.Conn, error) {
	if len(address) == 0 || len(address) > maxTargetLength {
		return nil, ErrInvalidTarget
	}

	totalTimeout := c.dialer.HandshakeTimeout + c.requestTimeout
	ctx, cancel := context.WithTimeout(context.Background(), totalTimeout)
	defer cancel()

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+c.authToken)
	conn, response, err := c.dialer.DialContext(ctx, c.endpoint, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("WSS handshake rejected with HTTP %d", response.StatusCode)
		}
		return nil, fmt.Errorf("WSS connection failed: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = conn.Close()
		}
	}()

	if conn.Subprotocol() != Subprotocol {
		return nil, errors.New("WSS server did not negotiate the OpenFlux subprotocol")
	}
	conn.SetReadLimit(maxControlMessage)
	if err := conn.SetWriteDeadline(time.Now().Add(c.requestTimeout)); err != nil {
		return nil, err
	}
	request := connectRequest{Version: ProtocolVersion, Command: connectCommand, Target: address}
	if err := writeControl(conn, request); err != nil {
		return nil, fmt.Errorf("send WSS connect request: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(c.requestTimeout)); err != nil {
		return nil, err
	}
	var tunnelResponse connectResponse
	if err := readControl(conn, &tunnelResponse); err != nil {
		return nil, fmt.Errorf("read WSS connect response: %w", err)
	}
	if tunnelResponse.Version != ProtocolVersion {
		return nil, errors.New("WSS server returned an unsupported protocol version")
	}
	if !tunnelResponse.OK || tunnelResponse.Code != responseCodeOK {
		return nil, &RemoteError{Code: tunnelResponse.Code}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxFramePayload)

	closeOnError = false
	return newStreamConn(conn), nil
}

func parseEndpoint(rawURL string) (*url.URL, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || !endpoint.IsAbs() || endpoint.Host == "" {
		return nil, errors.New("WSS endpoint must be an absolute URL")
	}
	if endpoint.Scheme != "wss" {
		return nil, errors.New("WSS endpoint must use wss://")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("WSS endpoint must not contain user information, a query, or a fragment")
	}
	if endpoint.Path != TunnelPath {
		return nil, fmt.Errorf("WSS endpoint path must be %s", TunnelPath)
	}
	return endpoint, nil
}

func validateClientToken(token string) error {
	if len(token) < minimumAuthTokenLength {
		return errors.New("WSS authentication token must contain at least 32 characters")
	}
	if len(token) > 4096 || token != strings.TrimSpace(token) || strings.ContainsAny(token, "\r\n\t ") {
		return errors.New("invalid WSS authentication token")
	}
	return nil
}

func contextDialFunc(dialer xproxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
		return contextDialer.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type dialResult struct {
			conn net.Conn
			err  error
		}
		result := make(chan dialResult, 1)
		go func() {
			conn, err := dialer.Dial(network, address)
			result <- dialResult{conn: conn, err: err}
		}()
		select {
		case <-ctx.Done():
			go func() {
				completed := <-result
				if completed.conn != nil {
					_ = completed.conn.Close()
				}
			}()
			return nil, ctx.Err()
		case completed := <-result:
			return completed.conn, completed.err
		}
	}
}
