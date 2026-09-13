package wss

import (
	"strings"
	"testing"
)

func TestNewClientRejectsHTTPSBootstrapProxy(t *testing.T) {
	_, err := NewClient(ClientConfig{
		Endpoint:  "wss://exit.example.test/openflux/v1/tunnel",
		AuthToken: testAuthToken,
		ProxyURL:  "https://proxy.example.test:443",
	})
	if err == nil || !strings.Contains(err.Error(), "http or socks5") {
		t.Fatalf("NewClient() error = %v, want unsupported proxy scheme error", err)
	}
}
