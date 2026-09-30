package share

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// packed wraps raw bytes the way a link carries its JSON.
func packed(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	w.Write(raw)
	w.Close()
	return Prefix + base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

// Every way a link can fail has its code: the apps word the problem from
// it, so a new failure without one would reach users as raw English.
func TestReadCodes(t *testing.T) {
	good, err := Encode(sample())
	if err != nil {
		t.Fatal(err)
	}
	big := `{"name":"` + strings.Repeat("x", maxPayload) + `","transports":[{"type":"yandex","url":"u"}]}`
	for _, tc := range []struct{ name, link, code string }{
		{"not a link", "https://example.com", CodeNotLink},
		{"other version", "openflux://v2/abc", CodeUnsupportedVersion},
		{"case changed", strings.ToUpper(good), CodeCaseChanged},
		{"not base64", Prefix + "!!!", CodeDamaged},
		{"cut short", good[:len(good)-12], CodeDamaged},
		{"too large", packed(t, []byte(big)), CodeTooLarge},
		{"not a config", packed(t, []byte("[1,2]")), CodeBadPayload},
	} {
		r := Read(tc.link)
		if r.Code != tc.code || r.Error == "" || r.Config != nil {
			t.Errorf("%s: %+v, want code %s", tc.name, r, tc.code)
		}
	}
	if r := Read(good); r.Error != "" || r.Config == nil || r.Context != sample().Context {
		t.Fatalf("good link: %+v", r)
	}
}

func TestMakeCodes(t *testing.T) {
	doc := Transport{Type: "yandex", URL: "https://disk.yandex.ru/i/abc"}
	key := "a shared secret of 32 characters"
	for _, tc := range []struct {
		name        string
		c           Config
		code, param string
	}{
		{"no transports", Config{}, CodeNoTransports, ""},
		{"several, classic", Config{Transports: []Transport{doc, doc}}, CodeNeedsSession, ""},
		{"session, short key", Config{Negotiate: true, Secret: "short", Transports: []Transport{doc}}, CodeSessionSecret, "16"},
		{"classic, short key", Config{Secret: "short", Transports: []Transport{doc}}, CodeShortSecret, "16"},
		{"codec", Config{Codec: "zip", Transports: []Transport{doc}}, CodeUnknownCodec, "zip"},
		{"MAX", Config{Transports: []Transport{{Type: "oneme"}}}, CodeNotShareable, "oneme"},
		{"unknown", Config{Transports: []Transport{{Type: "carrier-pigeon"}}}, CodeUnknownTransport, "carrier-pigeon"},
		{"direct, no address", Config{Negotiate: true, Secret: key, Transports: []Transport{{Type: "direct"}}}, CodeDirectNoDial, ""},
		{"direct, classic", Config{Secret: key, Transports: []Transport{{Type: "direct", Dial: "203.0.113.7:1"}}}, CodeDirectNeedsSession, ""},
	} {
		r := Make(tc.c)
		if r.Code != tc.code || r.Param != tc.param || r.Link != "" {
			t.Errorf("%s: %+v, want %s/%q", tc.name, r, tc.code, tc.param)
		}
	}
	if r := MakeJSON("{"); r.Code != CodeBadConfig {
		t.Errorf("bad JSON: %+v", r)
	}
	if _, err := MakeLink(Config{}); err == nil || err.(*Error).Code != CodeNoTransports {
		t.Errorf("MakeLink error %v", err)
	}
}

// One configuration, however an app spells it, gives one link, and that
// link reads back as the configuration that went in.
func TestMakeIsCanonical(t *testing.T) {
	const docURL = "https://docs.yandex.ru/edit/d/AbC"
	key := "a shared secret of 32 characters"
	base := Config{Name: "node", Negotiate: true, Secret: key, Transports: []Transport{
		{Type: "vyandex", URL: docURL, Priority: 100},
		{Type: "direct", Dial: "203.0.113.7:9443", Priority: 50},
	}}
	want := Make(base)
	if want.Error != "" || want.Config.Context != docURL {
		t.Fatalf("context not filled by the rule: %+v", want)
	}
	spelled := base
	spelled.Codec = "batched"
	spelled.Context = " " + docURL + " "
	spelled.Transports = []Transport{
		{Type: "vyandex", Name: "vyandex", URL: docURL + "  ", Priority: 100},
		{Type: " direct", Dial: " 203.0.113.7:9443", Priority: 50},
	}
	if got := Make(spelled); got.Link != want.Link {
		t.Errorf("spelled differently, linked differently:\n%s\n%s", got.Link, want.Link)
	}
	// A lone carrier is not ranked: apps that keep a priority and apps that
	// do not export the same link.
	lone := Config{Secret: key, Transports: []Transport{{Type: "mailru", URL: "https://cloud.mail.ru/public/x", Priority: 100}}}
	unranked := lone
	unranked.Transports = []Transport{{Type: "mailru", URL: "https://cloud.mail.ru/public/x"}}
	if a, b := Make(lone), Make(unranked); a.Link != b.Link || a.Config.Transports[0].Priority != 0 {
		t.Errorf("lone carrier: %+v vs %+v", a, b)
	}
	back := Read(want.Link)
	if !reflect.DeepEqual(back.Config, want.Config) || back.Context != want.Context {
		t.Errorf("read back %+v, made %+v", back.Config, want.Config)
	}

	for _, tc := range []struct {
		name    string
		c       Config
		context string
	}{
		{"classic document with a key", Config{Secret: key, Transports: []Transport{{Type: "mailru", URL: "https://cloud.mail.ru/public/x"}}}, "https://cloud.mail.ru/public/x"},
		{"cups rooms", Config{Secret: key, Transports: []Transport{{Type: "cupsonline", URL: "WyJhIl0"}}}, "http://#"},
		{"direct only", Config{Negotiate: true, Secret: key, Transports: []Transport{{Type: "direct", Dial: "203.0.113.7:1"}}}, "http://#"},
		{"explicit kept", Config{Negotiate: true, Secret: key, Context: "https://x/y", Transports: []Transport{{Type: "yandex", URL: "https://z"}}}, "https://x/y"},
		{"no key, no context", Config{Context: "https://x/y", Transports: []Transport{{Type: "yandex", URL: "https://z"}}}, ""},
	} {
		r := Make(tc.c)
		if r.Error != "" || r.Config.Context != tc.context || r.Context != tc.context {
			t.Errorf("%s: %+v, want context %q", tc.name, r, tc.context)
		}
	}
}

// The JSON the apps parse: field names are the contract.
func TestResultJSON(t *testing.T) {
	var fail map[string]string
	json.Unmarshal([]byte(Read("nope").JSON()), &fail)
	if fail["code"] != CodeNotLink || fail["error"] == "" {
		t.Errorf("failure JSON %v", fail)
	}
	var ok struct {
		Config  *Config `json:"config"`
		Context string  `json:"context"`
		Link    string  `json:"link"`
	}
	json.Unmarshal([]byte(Make(sample()).JSON()), &ok)
	if ok.Config == nil || ok.Link == "" || ok.Context != sample().Context {
		t.Errorf("success JSON %+v", ok)
	}
}
