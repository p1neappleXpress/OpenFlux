package mobile

import (
	"encoding/json"
	"testing"
)

func TestPhpCallAnswersInJSON(t *testing.T) {
	var r struct {
		OK    bool   `json:"ok"`
		Code  string `json:"code"`
		Param string `json:"param"`
	}
	if err := json.Unmarshal([]byte(PhpCall("teleport", `{}`)), &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Code != "bad_params" || r.Param != "teleport" {
		t.Errorf("unknown method: %+v", r)
	}
	if err := json.Unmarshal([]byte(PhpCall("probe", `{"ftp":{"host":"127.0.0.1","port":1,"user":"u","password":"p","tls":"none"}}`)), &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Code != "ftp_connect" {
		t.Errorf("nothing listening: %+v", r)
	}
	if p := PhpProgress(); p != "" {
		t.Errorf("progress without an upload: %q", p)
	}
	PhpCancel() // with nothing running: must not panic
}

func TestPhpLinkMakesAStreamLink(t *testing.T) {
	var r struct {
		OK   bool `json:"ok"`
		Data struct {
			Link string `json:"link"`
		} `json:"data"`
	}
	out := PhpCall("link", `{"carrier":"mailru","target":"https://cloud.mail.ru/public/Vuri/d5nuZ5aQp","name":"Free"}`)
	if err := json.Unmarshal([]byte(out), &r); err != nil || !r.OK || r.Data.Link == "" {
		t.Fatalf("link: %s (%v)", out, err)
	}
	var back struct {
		Config struct {
			Mode string `json:"mode"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(ReadShareLink(r.Data.Link)), &back); err != nil || back.Config.Mode != "stream" {
		t.Errorf("the link the app gets reads back as %+v (%v)", back, err)
	}
}

func TestStartStreamProxyRefusesWhatItCannotRun(t *testing.T) {
	if msg := StartStreamProxy("direct", "1.2.3.4:5", "127.0.0.1:0", "", "", ""); msg == "" {
		t.Error("direct is not a carrier of the PHP exit")
	}
	if msg := StartStreamProxy("mailru", "  ", "127.0.0.1:0", "", "", ""); msg == "" {
		t.Error("no address must be refused")
	}
	if ProxyIsRunning() {
		t.Error("a refused start left the proxy running")
	}
}

func TestStartStreamPacketRefusesWhatItCannotRun(t *testing.T) {
	if msg := StartStreamPacket("vyandex", "https://docs.yandex.ru/x"); msg == "" {
		t.Error("yandex is not a carrier of the PHP exit")
	}
	if msg := StartStreamPacket("cupsonline", ""); msg == "" {
		t.Error("no address must be refused")
	}
	if IsConnected() {
		t.Error("a refused start left the packet tunnel up")
	}
}
