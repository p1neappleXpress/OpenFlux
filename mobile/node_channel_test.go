package mobile

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// Without an SSH connection the channel calls answer with an error, not a panic.
func TestNodeChannelCallsNeedConnection(t *testing.T) {
	for name, out := range map[string]string{
		"show": NodeShowChannel("of-abc123", ""),
		"set":  NodeSetTransports("of-abc123", `[{"type":"mailru","url":"https://cloud.mail.ru/public/DEmN/ETbZW2MPY"}]`, "", "", ""),
	} {
		var r struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil || r.OK || r.Error == "" {
			t.Fatalf("%s: %s", name, out)
		}
	}
}

// A link for an installed channel keeps the channel's context.
func TestNodeShareLinkContextKeepsContext(t *testing.T) {
	key := strings.Repeat("ab", 32)
	doc := "https://docs.yandex.ru/edit/d/S3sjFjA-ZmVWgXQPTmVwdSPegnqahzm72s0qoIz-cKg6OTJnZ2wza1d2UQ"
	link, err := NodeShareLinkContext("Нода", `[{"type":"mailru","url":"https://cloud.mail.ru/public/DEmN/ETbZW2MPY"}]`, key, "203.0.113.5", 8445, doc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != doc || c.Transports[0].Type != "mailru" {
		t.Fatalf("link: %+v", c)
	}
}
