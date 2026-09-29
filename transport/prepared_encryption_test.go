package transport

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestPreparedEncryptionInteroperatesWithExistingPeer(t *testing.T) {
	const secret = "public test encryption secret"
	contexts := []string{"document", ContextPlaceholder, "vyandex"}
	bundle, err := PrepareEncryption(secret, contexts)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := ParsePreparedEncryption(bundle, secret, contexts)
	if err != nil {
		t.Fatal(err)
	}
	for _, context := range contexts {
		t.Run(context, func(t *testing.T) {
			wire := &testTransport{}
			client, err := newEncryptedTransport(
				wire,
				secret,
				contexts[0],
				false,
				prepared,
			)
			if err != nil {
				t.Fatal(err)
			}
			client.SetAlternateContexts(contexts[1:])
			client.ring.deriveMore()
			deadline := time.Now().Add(time.Second)
			for {
				keys, pending := client.ring.snapshot()
				if len(keys) == len(contexts) && !pending {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("prepared alternate keys did not become available")
				}
				time.Sleep(time.Millisecond)
			}
			exitWire := &testTransport{}
			exit, err := NewEncryptedTransport(
				exitWire,
				secret,
				context,
				true,
			)
			if err != nil {
				t.Fatal(err)
			}
			classicWire := &testTransport{}
			classic := client.SharingKeys(classicWire)
			var received [][]byte
			client.Receive(func(p []byte) { received = append(received, p) })
			if err := exit.Send([]byte("exit response")); err != nil {
				t.Fatal(err)
			}
			wire.deliver(exitWire.sent)
			wire.deliver(exitWire.sent)
			if len(received) != 1 || string(received[0]) != "exit response" {
				t.Fatal("prepared keys did not decrypt once with replay protection")
			}
			if client.Context() != context || classic.Context() != context {
				t.Fatal("Session/classic did not adopt the peer's context")
			}
			var response []byte
			exit.Receive(func(p []byte) { response = p })
			if err := classic.Send([]byte("classic reply")); err != nil {
				t.Fatal(err)
			}
			exitWire.deliver(classicWire.sent)
			if string(response) != "classic reply" {
				t.Fatal("existing exit could not decrypt prepared client's reply")
			}
		})
	}
}

func TestPreparedEncryptionRejectsStaleOrCorruptBundle(t *testing.T) {
	const secret = "public test encryption secret"
	contexts := []string{"document", ContextPlaceholder}
	bundle, err := PrepareEncryption(secret, contexts)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(bundle, []byte("document"), []byte("modified"), 1)
	for _, tc := range []struct {
		name     string
		data     []byte
		secret   string
		contexts []string
	}{
		{name: "missing", secret: secret, contexts: contexts},
		{name: "malformed", data: []byte("{"), secret: secret, contexts: contexts},
		{name: "oversized", data: make([]byte, 65<<10), secret: secret, contexts: contexts},
		{name: "key changed", data: bundle, secret: "different public test key", contexts: contexts},
		{name: "key removed", data: bundle, contexts: contexts},
		{name: "profile changed", data: bundle, secret: secret, contexts: []string{"another document"}},
		{name: "tampered", data: tampered, secret: secret, contexts: contexts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePreparedEncryption(tc.data, tc.secret, tc.contexts); err == nil {
				t.Fatal("invalid bundle was accepted")
			}
		})
	}
}

func TestPreparedSessionRefusesUnpreparedContext(t *testing.T) {
	const secret = "public test encryption secret"
	bundle, err := PrepareEncryption(secret, []string{"document"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := ParsePreparedEncryption(bundle, secret, []string{"document"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(PeerParameters{Capabilities: CapabilityIPv4 | CapabilityTCP, MaxPacketSize: 1500}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPreparedEncryption(prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTransport(
		"unknown",
		&testTransport{},
		secret,
		"unknown context",
		100,
	); err == nil {
		t.Fatal("unprepared primary context was accepted")
	}
	if err := s.AddTransport(
		"wrong-key",
		&testTransport{},
		"different public test secret",
		"document",
		100,
	); err == nil {
		t.Fatal("prepared keys accepted for a different carrier secret")
	}
	if err := s.AddTransport(
		"known",
		&testTransport{},
		secret,
		"document",
		100,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.links["known"].encrypted.ring.derive("unknown alternate"); err == nil {
		t.Fatal("unprepared alternate context was derived")
	}
	if err := s.SetPreparedEncryption(prepared); err == nil {
		t.Fatal("keys changed after adding a carrier")
	}
}

func TestPrepareEncryptionValidatesContextsAndSecret(t *testing.T) {
	for _, tc := range []struct {
		name     string
		secret   string
		contexts []string
	}{
		{name: "short key", secret: "short", contexts: []string{"document"}},
		{name: "no contexts", secret: "public test encryption secret"},
		{name: "empty context", secret: "public test encryption secret", contexts: []string{""}},
		{name: "duplicate context", secret: "public test encryption secret", contexts: []string{"doc", "doc"}},
		{name: "too many contexts", secret: "public test encryption secret", contexts: make([]string, 9)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PrepareEncryption(tc.secret, tc.contexts); err == nil {
				t.Fatal("invalid preparation inputs were accepted")
			}
		})
	}
}

func TestPreparedEncryptionBundleContainsNoRawSecret(t *testing.T) {
	const secret = "public test encryption secret"
	bundle, err := PrepareEncryption(secret, []string{"document"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bundle, []byte(secret)) || !json.Valid(bundle) {
		t.Fatal("bundle exposes raw secret or is not JSON")
	}
}

func TestPreparedSessionCompatibility(t *testing.T) {
	contexts := []string{"document", ContextPlaceholder}
	bundle, err := PrepareEncryption(compatSecret, contexts)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := ParsePreparedEncryption(bundle, compatSecret, contexts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		classic bool
	}{
		{name: "Session exit"},
		{name: "legacy classic exit with alternate context", classic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := asyncPair()
			client, err := NewSession(PeerParameters{
				Capabilities:  CapabilityIPv4 | CapabilityTCP | CapabilityUDP,
				MaxPacketSize: MaxNegotiatedPacket,
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.SetPreparedEncryption(prepared); err != nil {
				t.Fatal(err)
			}
			client.SetClassic(CodecBatched)
			client.SetAlternateContexts(contexts[1:])
			if err := client.AddTransport(
				"carrier",
				a,
				compatSecret,
				contexts[0],
				100,
			); err != nil {
				t.Fatal(err)
			}
			var exit peer
			if tc.classic {
				exit = oldClassic(
					t,
					b,
					CodecLegacy,
					ContextPlaceholder,
					true,
				)
			} else {
				exit = compatSession(
					t,
					b,
					contexts[0],
					nil,
					true,
					false,
					"",
				)
			}
			roundTrip(
				t,
				client,
				exit,
				20*time.Second,
			)
		})
	}
}
