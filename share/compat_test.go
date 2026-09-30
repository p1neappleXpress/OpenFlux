package share

import (
	"strings"
	"testing"
)

// Links written by earlier builds stay readable forever. These literals were
// produced by Encode and are frozen here: if a change makes one of them
// unreadable (or changes what it says), a link a user already shared or
// printed as a QR code stops working.
var frozenLinks = []struct {
	name string
	link string
	want func(t *testing.T, c Config)
}{
	{
		"classic cups, no secret",
		"openflux://v1/TMpBCsIwEAXQu_y1aTCJm4B4EHEhNehAnAmTaUVK7y6ii771W2B65d5ErSOfF9i7FWSMU-vClbhgh0krMh5mrWfvia3oTOU1fNPwW77SXNwoN-K7P6nI87j_cyGE4GKM0aWUkjtsYL2snwAAAP__",
		func(t *testing.T, c Config) {
			if len(c.Transports) != 1 || c.Transports[0].Type != "cupsonline" || c.Secret != "" || c.Negotiate {
				t.Errorf("got %+v", c)
			}
		},
	},
	{
		"mailru with a name",
		"openflux://v1/qlbKS8xNVbJS8k3MzNErKlVIK8pMzUtR0lEqKUrMKy7ILyopVrKKrlYqqSwAKctNzMwpKlXSUSotylGyUnJMck5xTXPPMNT3zPLO8c3zLzBSqo2tBQQAAP__",
		func(t *testing.T, c Config) {
			if c.Name != "Mail.ru friend" || c.Transports[0].Type != "mailru" || c.Transports[0].URL != "AbCdEfGh1/IjKlMnOp2" {
				t.Errorf("got %+v", c)
			}
		},
	},
	{
		"session with two transports, secret and context",
		"openflux://v1/rI_BaiwhEEV_pSnonWPb7TweCCFkM-QfQha2FhlJj0pZTkaG_vdgAoHssyw4dc-9d4j2gmDgOV0QBER8SxwsIximigIKOkIGAy4RoePhnKjgsFpmpDYUtnnrjy5FxlsHz8y5mGnyyRXZbPR4k1S_zuka8OOx0vbQ7MGH8n7Idd2CG_XTuJzG5WRXBwKYbCw5ERcwL3fglnvD63cWCKi0_YknU0gUuIGZldrFj8mHvvQX8E8J8MF276K0VHKetfxvjketYX_dPwMAAP__",
		func(t *testing.T, c Config) {
			if !c.Negotiate || c.Secret != "correct horse battery staple" || len(c.Transports) != 2 ||
				c.Transports[1].Type != "direct" || c.Transports[1].Dial != "203.0.113.7:4433" || c.Transports[0].Priority != 100 {
				t.Errorf("got %+v", c)
			}
		},
	},
	{
		"legacy codec",
		"openflux://v1/JMk7DsIwDAbgu_xz1PB--CqIIdgGhqqJbFdqVPXuDMzfCq6iDMKon8IdCa5sGiDs9ofj6Xy53u7lxaJvJISVyVu1cNBjRfSmIPQyiS5ImG0E4RvRnHKWyj78bbA5L9ie2y8AAP__",
		func(t *testing.T, c Config) {
			if c.Codec != "legacy" || c.Transports[0].Type != "yandex" {
				t.Errorf("got %+v", c)
			}
		},
	},
	{
		// Hand-written with only fields the first version (2e07ecd) had, plus
		// fields no version knows: newer links must read on older readers and
		// the other way round, so unknown JSON fields are ignored.
		"unknown fields are ignored",
		"openflux://v1/PY0xCwIxDEb_S2a94loQNydHN3Eod1EDvaSkaUWO---2HJox7328BTjMCB4exYoi7MA0cE6ilsHfFrBP6ngsKQtH4q4Uje31MkvZO0dsqJXwPXRp2CwXqeJ-lIn46U4qMh9DuzbeQtdf5UwYJ_CH9f5Hki5YsSUWCJ2sXw",
		func(t *testing.T, c Config) {
			if c.Name != "future" || c.Transports[0].Type != "cupsonline" {
				t.Errorf("got %+v", c)
			}
		},
	},
}

func TestFrozenLinksStillRead(t *testing.T) {
	for _, f := range frozenLinks {
		t.Run(f.name, func(t *testing.T) {
			r := Read(f.link)
			if r.Error != "" || r.Config == nil {
				t.Fatalf("Read: %s (%s)", r.Error, r.Code)
			}
			f.want(t, *r.Config)
		})
	}
}

// How a link gets mangled on its way (chat, terminal, a QR app) must not
// change what it says: each variant reads to the same configuration.
func TestFrozenLinksSurviveTheWay(t *testing.T) {
	base := frozenLinks[2].link
	want := Read(base)
	if want.Error != "" {
		t.Fatal(want.Error)
	}
	body := strings.TrimPrefix(base, Prefix)
	mangled := map[string]string{
		"wrapped lines":     Prefix + body[:40] + "\n" + body[40:90] + "\r\n  " + body[90:],
		"padding added":     base + strings.Repeat("=", (4-len(body)%4)%4),
		"standard alphabet": Prefix + strings.NewReplacer("-", "+", "_", "/").Replace(body),
		"surrounding space": "  " + base + "\n",
		"zero-width space":  Prefix + body[:30] + "​" + body[30:],
	}
	for name, l := range mangled {
		t.Run(name, func(t *testing.T) {
			got := Read(l)
			if got.Error != "" {
				t.Fatalf("Read: %s (%s)", got.Error, got.Code)
			}
			if got.Config.Secret != want.Config.Secret || len(got.Config.Transports) != len(want.Config.Transports) ||
				got.Context != want.Context {
				t.Errorf("read differently: %+v vs %+v", got, want)
			}
		})
	}
}

// The codes are the contract with the apps (they word each one); a rename
// would make every app show the wrong message.
func TestErrorCodesAreStable(t *testing.T) {
	good := frozenLinks[0].link
	cases := []struct{ name, link, code string }{
		{"not a link", "https://example.com", "not_link"},
		{"future version", "openflux://v2/abc", "unsupported_version"},
		{"case changed", "OPENFLUX://V1/abc", "case_changed"},
		{"truncated", good[:len(good)-30], "damaged"},
	}
	for _, c := range cases {
		if r := Read(c.link); r.Code != c.code {
			t.Errorf("%s: code %q, want %q (%s)", c.name, r.Code, c.code, r.Error)
		}
	}
}
