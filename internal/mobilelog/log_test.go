package mobilelog

import (
	"strings"
	"testing"
)

func TestSecretsAndURLsNeverEnterBuffer(t *testing.T) {
	b := New()
	b.SetSecrets("synthetic-secret-0123456789", "synthetic-token")
	line := `request https://document.invalid/test?token=synthetic-token ws wss://relay.invalid/private secret synthetic-secret-0123456789`
	b.Write([]byte(line))
	out := b.Drain()
	for _, forbidden := range []string{"synthetic-secret", "synthetic-token", "document.invalid", "relay.invalid"} {
		if strings.Contains(out, forbidden) {
			t.Fatal("sensitive log data retained")
		}
	}
	if b.Drain() != "" {
		t.Fatal("drain retained logs")
	}
}

func TestLogMemoryBounded(t *testing.T) {
	b := New()
	for i := 0; i < 2000; i++ {
		b.Write([]byte(strings.Repeat("x", 5000)))
	}
	if len(b.Drain()) > 66000 {
		t.Fatal("unbounded logs")
	}
}
