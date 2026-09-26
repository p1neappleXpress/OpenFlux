package control

import (
	"testing"
)

func TestTransportConfigRoundTrip(t *testing.T) {
	in := &TransportConfig{
		Name: "yandex-1",
		Type: "yandex",
		URL:  "https://disk.yandex.ru/i/XXX",
		Params: map[string]interface{}{
			"priority": float64(100),
			"region":   "ru",
		},
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeTransportConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "yandex-1" || out.Type != "yandex" || out.URL != "https://disk.yandex.ru/i/XXX" {
		t.Fatalf("mismatch: %+v", out)
	}
	if out.Params["region"] != "ru" {
		t.Fatalf("params lost: %+v", out.Params)
	}
}

func TestTransportStatusListRoundTrip(t *testing.T) {
	in := &TransportStatusList{
		Transports: []TransportStatus{
			{Name: "direct-main", Type: "direct", Connected: true},
			{Name: "yandex-1", Type: "yandex", Connected: false, Error: "captcha"},
		},
	}
	raw, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeTransportStatusList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Transports) != 2 {
		t.Fatalf("expected 2, got %d", len(out.Transports))
	}
	if out.Transports[1].Error != "captcha" {
		t.Fatalf("error lost: %+v", out.Transports[1])
	}
}

func TestControlPacketCarriesTransportStart(t *testing.T) {
	cfg := &TransportConfig{Name: "yandex-1", Type: "yandex", URL: "https://x"}
	body, _ := cfg.Encode()

	env := &Envelope{
		Kind: KindControl,
		Role: RoleClient,
		Control: &ControlTail{
			Subtype:    SubtypeTransportStart,
			PayloadLen: uint16(len(body)),
		},
	}
	raw, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	full := append(raw, body...)

	cp, _, err := DecodeWithPayload(full)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Subtype != SubtypeTransportStart {
		t.Fatalf("subtype = %v", cp.Subtype)
	}
	parsed, err := DecodeTransportConfig(cp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "yandex-1" || parsed.Type != "yandex" {
		t.Fatalf("parsed = %+v", parsed)
	}
}
