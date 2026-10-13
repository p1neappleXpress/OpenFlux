package provision

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

// The installer's config for every transport, its checks, node.conf and the
// .flux packages, under the shell a VDS runs (deploy/test/config-test.sh).
func TestInstallerConfig(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command(sh, "../deploy/test/config-test.sh", "../deploy/node-install.sh", t.TempDir()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

const testKey = "abababababababababababababababababababababababababababababababab"

// Telemost and Avito go to the installer as their own lines; a script goes as
// one line with its parameter in base64url, and the link leaves it out.
func TestTransportsForEveryCarrier(t *testing.T) {
	pkg := base64.StdEncoding.EncodeToString([]byte("flux"))
	ts, err := CheckTransports([]ChannelTransport{
		{Type: "script", Script: "mtslink", Pubkey: testKey, URL: "https://my.mts-link.ru/j/a,b", Params: "eyJ4IjoiMSJ9", Package: pkg},
		{Type: "avito", URL: testRooms},
		{Type: "telemost", URL: "https://telemost.yandex.ru/j/88936358186517"},
		{Type: "mailru", URL: testMailru},
	})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, x := range ts {
		order = append(order, x.Type)
	}
	if strings.Join(order, ",") != "mailru,telemost,avito,script" {
		t.Fatalf("order %v", order)
	}
	lines := transportLines(ts)
	for _, want := range []string{
		"telemost=https://telemost.yandex.ru/j/88936358186517\n",
		"avito=" + testRooms + "\n",
		"script=mtslink," + testKey + "," + base64.RawURLEncoding.EncodeToString([]byte("https://my.mts-link.ru/j/a,b")) + ",eyJ4IjoiMSJ9," + pkg + "\n",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %q in\n%s", want, lines)
		}
	}
	if strings.Contains(ts[3].String(), pkg) || strings.Contains(ts[3].String(), "eyJ4") {
		t.Errorf("String() shows the package or settings: %s", ts[3])
	}
	link, err := ShareLink("Нода", testKey, "203.0.113.5", 8445, ts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "openflux://") {
		t.Fatalf("link %q", link)
	}
}

func TestScriptsAreChecked(t *testing.T) {
	good := ChannelTransport{Type: "script", Script: "mtslink", Pubkey: testKey}
	if _, err := CheckTransports([]ChannelTransport{good, {Type: "script", Script: "bitrix", Pubkey: testKey}}); err != nil {
		t.Fatalf("two different scripts: %v", err)
	}
	for name, bad := range map[string]ChannelTransport{
		"twice":    good,
		"id":       {Type: "script", Script: "Bad/Id", Pubkey: testKey},
		"key":      {Type: "script", Script: "x", Pubkey: "nothex"},
		"hash":     {Type: "script", Script: "x", Pubkey: testKey, URL: "https://a#b"},
		"newline":  {Type: "script", Script: "x", Pubkey: testKey, URL: "a\nkey=0"},
		"settings": {Type: "script", Script: "x", Pubkey: testKey, Params: "a,b"},
		"package":  {Type: "script", Script: "x", Pubkey: testKey, Package: "***"},
	} {
		ts := []ChannelTransport{bad}
		if name == "twice" {
			ts = append(ts, good)
		}
		if _, err := CheckTransports(ts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := CheckTransports([]ChannelTransport{{Type: "avito", URL: "not a list"}}); err == nil {
		t.Error("a bad Avito list is accepted")
	}
}
