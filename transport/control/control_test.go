package control

import (
	"bytes"
	"testing"
)

func TestEnvelopeHelloRoundTrip(t *testing.T) {
	env := &Envelope{
		Kind:  KindHello,
		Role:  RoleClient,
		Hello: &HelloTail{Capabilities: CapabilityIPv4 | CapabilityTCP, MaxPacketSize: 1500, Ready: 1},
	}
	env.Local[0], env.Peer[0] = 0xAA, 0xBB

	raw, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindHello || got.Role != RoleClient {
		t.Fatalf("kind/role: %+v", got)
	}
	if got.Local[0] != 0xAA || got.Peer[0] != 0xBB {
		t.Fatal("challenges lost")
	}
	if got.Hello == nil || got.Hello.MaxPacketSize != 1500 || got.Hello.Ready != 1 {
		t.Fatalf("hello tail: %+v", got.Hello)
	}
}

func TestEnvelopeIPv4RoundTrip(t *testing.T) {
	env := &Envelope{Kind: KindIPv4, Role: RoleExit, Data: &DataTail{Sequence: 42}}
	raw, _ := env.Encode()
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Data == nil || got.Data.Sequence != 42 {
		t.Fatalf("data tail: %+v", got.Data)
	}
}

func TestEnvelopeControlRoundTrip(t *testing.T) {
	env := &Envelope{
		Kind:    KindControl,
		Role:    RoleExit,
		Control: &ControlTail{Subtype: SubtypeCookiesResponse, PayloadLen: 3},
	}
	raw, _ := env.Encode()
	full := append(raw, 'a', 'b', 'c')
	got, err := Decode(full)
	if err != nil {
		t.Fatal(err)
	}
	if got.Control == nil || got.Control.Subtype != SubtypeCookiesResponse || got.Control.PayloadLen != 3 {
		t.Fatalf("control tail: %+v", got.Control)
	}
	cp, _, err := DecodeWithPayload(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cp.Payload, []byte("abc")) {
		t.Fatalf("payload: %q", cp.Payload)
	}
}

func TestEnvelopeRejectsBadMagicAndReserved(t *testing.T) {
	env := &Envelope{Kind: KindControl, Role: RoleExit, Control: &ControlTail{Subtype: SubtypeCookiesRequest}}
	raw, _ := env.Encode()
	bad := append([]byte(nil), raw...)
	bad[0] = 'X'
	if _, err := Decode(bad); err == nil {
		t.Fatal("accepted bad magic")
	}
	bad = append([]byte(nil), raw...)
	bad[74] = 1
	if _, err := Decode(bad); err == nil {
		t.Fatal("accepted non-zero reserved")
	}
}

func TestCookiesPayloadRoundTrip(t *testing.T) {
	in := &CookiesPayload{Jar: map[string]string{"a": "1", "b": "2"}, Domain: "disk.yandex.ru", Reason: "captcha-solved"}
	raw, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeCookies(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Jar["a"] != "1" || out.Jar["b"] != "2" || out.Domain != "disk.yandex.ru" || out.Reason != "captcha-solved" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
