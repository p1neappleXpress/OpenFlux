package utils

import (
	"strings"
	"testing"
)

func TestRedactNetworkErrors(t *testing.T) {
	for _, s := range []string{`Get "https://document.invalid/private?token=private": failed`, `dial wss://push.invalid/ws?sign=private failed`} {
		out := RedactURLs(s)
		if strings.Contains(out, "private") || strings.Contains(out, ".invalid") {
			t.Fatal("URL credential leak")
		}
	}
}
