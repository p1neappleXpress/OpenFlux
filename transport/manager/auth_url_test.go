package manager

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

func TestAuthURLLoggingPreservesFunctionalPayload(t *testing.T) {
	for _, level := range []int{utils.LevelOff, utils.LevelDebug} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			client, exit := connectedManagers(t, &fakeCookieProvider{})
			const document = "https://docs.example/original-document"
			const original = "https://host/path?secret=TOKEN#fragment"
			exit.SetURL("yandex", document)
			client.SetURL("yandex", document)
			var mu sync.Mutex
			var logs []string
			oldLevel := utils.Level()
			utils.SetLevel(level)
			utils.SetLogSink(func(line string) { mu.Lock(); logs = append(logs, line); mu.Unlock() })
			defer func() { utils.SetLogSink(nil); utils.SetLevel(oldLevel) }()
			remote := make(chan [4]string, 1)
			client.SetRemoteAuthNotifier(func(name, url, html, reason string) { remote <- [4]string{name, url, html, reason} })
			payload := make(chan *control.AuthRequiredPayload, 1)
			client.session.SetControlHandler(func(sub control.Subtype, body []byte) {
				client.DispatchControl(sub, body)
				if sub == control.SubtypeAuthRequired {
					p, err := control.DecodeAuthRequired(body)
					if err == nil {
						payload <- p
					}
				}
			})
			exit.NotifyCaptcha("yandex", original, "<p>synthetic setup HTML</p>", "smartcaptcha")
			select {
			case p := <-payload:
				if p.URL != original || p.Doc != document || p.HTML != "<p>synthetic setup HTML</p>" {
					t.Fatal("functional URL/Doc payload was modified")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing AuthRequired payload")
			}
			select {
			case got := <-remote:
				if got != [4]string{"yandex", original, "<p>synthetic setup HTML</p>", "smartcaptcha"} {
					t.Fatal("browser notifier did not receive original URL")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing browser notifier")
			}
			mu.Lock()
			defer mu.Unlock()
			text := strings.Join(logs, "\n")
			for _, forbidden := range []string{"TOKEN", "fragment", "secret=", original} {
				if strings.Contains(text, forbidden) {
					t.Fatal("auth URL secret reached Info/Debug log sink")
				}
			}
			if !strings.Contains(text, "https://host/[path]") {
				t.Fatal("safe path class missing from log")
			}
		})
	}
}

func TestAuthURLLogPathClasses(t *testing.T) {
	for raw, want := range map[string]string{
		"https://host/showcaptcha?secret=TOKEN#fragment":        "https://host/showcaptcha",
		"https://host/auth?secret=TOKEN#fragment":               "https://host/auth",
		"https://host/private-TOKEN?secret=TOKEN#fragment":      "https://host/[path]",
		"https://user:password@host/path?secret=TOKEN#fragment": "https://host/[path]",
		"%invalidTOKEN": "<invalid auth URL>",
	} {
		if redactedAuthURL(raw) != want {
			t.Fatal("unsafe or incorrect log path class")
		}
	}
}
