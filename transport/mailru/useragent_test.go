package mailru

import (
	"strings"
	"testing"
)

// Mail.ru's document server refuses the WebSocket of a client that claims to
// be Chrome but does not shake hands like Chrome (409 Conflict), so the
// carrier must not claim to be Chrome.
func TestMailruUserAgentIsNotChrome(t *testing.T) {
	if strings.Contains(mailruUserAgent, "Chrome/") || !strings.Contains(mailruUserAgent, "Firefox/") {
		t.Fatalf("mailruUserAgent = %q: the document server answers a Chrome agent from Go with 409", mailruUserAgent)
	}
}
