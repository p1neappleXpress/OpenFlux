package mobile

import (
	"testing"

	"openflux/transport"
)

func TestPreparedClassicKeysUseSessionContexts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		typ      string
		url      string
		contexts []string
	}{
		{name: "volga", typ: "vyandex", url: "https://docs.example/test", contexts: []string{"https://docs.example/test", "http://#", "vyandex"}},
		{name: "direct", typ: "direct", url: "127.0.0.1:12345", contexts: []string{"http://#", "direct"}},
		{name: "max", typ: "oneme", contexts: []string{"http://#", "oneme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := ClassicSessionSpecs(
				tc.typ,
				tc.url,
				"test-token",
				"test-uid",
			)
			bundle, err := PrepareSessionKeys(specs, testSecret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := transport.ParsePreparedEncryption(
				[]byte(bundle), testSecret, tc.contexts,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartPreparedSessionFailsBeforeNetworkForMissingOrStaleKeys(t *testing.T) {
	specs := ClassicSessionSpecs(
		"vyandex",
		"https://docs.example/test",
		"",
		"",
	)
	bundle, err := PrepareSessionKeys(specs, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		bundle string
		secret string
	}{
		{name: "missing", secret: testSecret},
		{name: "malformed", bundle: "{}", secret: testSecret},
		{name: "key changed", bundle: bundle, secret: "another public test secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if msg := StartPreparedSession(
				specs,
				tc.secret,
				tc.bundle,
				transport.CodecBatched,
			); msg == "" {
				t.Fatal("invalid material started a client")
			}
			if IsConnected() || Send([]byte("probe")) == "" {
				t.Fatal("client became active for invalid material")
			}
		})
	}
}
