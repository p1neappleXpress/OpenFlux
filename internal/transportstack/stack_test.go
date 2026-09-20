package transportstack

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"openflux/internal/packettunnel"
	"openflux/transport"
)

// Entirely in-memory: no document, account, token or network is used by tests.
type endpoint struct {
	mu        sync.Mutex
	cb        func([]byte)
	peer      *endpoint
	delivered chan struct{}
}

func TestStackOwnsSendBuffer(t *testing.T) {
	for _, codec := range []string{Batched, Legacy} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", codec, encrypted), func(t *testing.T) {
				o := Options{Transport: "vyandex", URL: testURL, Codec: codec, Mobile: true}
				if encrypted {
					o.EncryptionSecret = testSecret
				}
				exit := o
				exit.ExitNode = true
				c, _, _, recv := startPair(t, o, exit)
				payload := bytes.Repeat([]byte{0x45}, 1400)
				want := append([]byte(nil), payload...)
				if err := c.Send(payload); err != nil {
					t.Fatal(err)
				}
				clear(payload)
				expectPacket(t, recv, want)
			})
		}
	}
}

func BenchmarkPreparedMobileStackStartup(b *testing.B) {
	// A synthetic derived key stands in for Keychain. No KDF occurs in the NE.
	o := Options{Transport: "vyandex", URL: testURL, Codec: Batched, Mobile: true, PreparedKey: make([]byte, 32)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := pair()
		stack, err := Wrap(raw, o)
		if err != nil {
			b.Fatal(err)
		}
		s := packettunnel.New(stack)
		if err := s.Start(); err != nil {
			b.Fatal(err)
		}
		s.Stop()
	}
}

func (*endpoint) Start() error                    { return nil }
func (*endpoint) Stop() error                     { return nil }
func (*endpoint) IsConnected() bool               { return true }
func (*endpoint) Stats() transport.TransportStats { return transport.TransportStats{} }
func (e *endpoint) Receive(cb func([]byte))       { e.mu.Lock(); e.cb = cb; e.mu.Unlock() }
func (e *endpoint) Send(p []byte) error {
	e.peer.mu.Lock()
	cb := e.peer.cb
	e.peer.mu.Unlock()
	if cb != nil {
		cb(append([]byte(nil), p...))
	}
	if e.delivered != nil {
		e.delivered <- struct{}{}
	}
	return nil
}
func pair() (*endpoint, *endpoint) {
	a, b := &endpoint{}, &endpoint{}
	a.peer = b
	b.peer = a
	return a, b
}

const testSecret = "synthetic-test-secret-not-a-deployment-key"
const testURL = "https://document.invalid/test-fixture"

func startPair(t *testing.T, client, exit Options) (transport.Transport, transport.Transport, chan []byte, chan []byte) {
	c, s, cr, sr, _ := startObservedPair(t, client, exit)
	return c, s, cr, sr
}

func startObservedPair(t *testing.T, client, exit Options) (transport.Transport, transport.Transport, chan []byte, chan []byte, func()) {
	t.Helper()
	a, b := pair()
	a.delivered, b.delivered = make(chan struct{}, 8), make(chan struct{}, 8)
	c, err := Wrap(a, client)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Wrap(b, exit)
	if err != nil {
		t.Fatal(err)
	}
	cr, sr := make(chan []byte, 8), make(chan []byte, 8)
	c.Receive(func(p []byte) { cr <- p })
	s.Receive(func(p []byte) { sr <- p })
	if err = c.Start(); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(); s.Stop() })
	waitDelivery := func() {
		t.Helper()
		for _, done := range []chan struct{}{a.delivered, b.delivered} {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("fake carrier did not deliver the test frame")
			}
		}
	}
	return c, s, cr, sr, waitDelivery
}
func expectPacket(t *testing.T, ch chan []byte, want []byte) {
	t.Helper()
	select {
	case got := <-ch:
		if !bytes.Equal(got, want) {
			t.Fatal("wire payload changed")
		}
	case <-time.After(time.Second):
		t.Fatal("packet not delivered")
	}
}

func TestMobileCLIWireCompatibility(t *testing.T) {
	for _, kind := range []string{"yandex", "vyandex"} {
		for _, codec := range []string{Batched, Legacy} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/encrypted=%v", kind, codec, encrypted), func(t *testing.T) {
					client := Options{Transport: kind, URL: testURL, Codec: codec, Mobile: true}
					if encrypted {
						client.EncryptionSecret = testSecret
					}
					exit := client
					exit.Mobile = false
					exit.ExitNode = true
					c, s, cr, sr := startPair(t, client, exit)
					for _, p := range [][]byte{[]byte("short packet"), bytes.Repeat([]byte{0x45, 0, 0x41}, 500)} {
						if err := c.Send(p); err != nil {
							t.Fatal(err)
						}
						expectPacket(t, sr, p)
						if err := s.Send(p); err != nil {
							t.Fatal(err)
						}
						expectPacket(t, cr, p)
					}
				})
			}
		}
	}
}

func TestPreparedKeyMatchesSecretWire(t *testing.T) {
	key, err := transport.DeriveEncryptionKey(testSecret, EncryptionContext("vyandex", testURL))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	for _, codec := range []string{Batched, Legacy} {
		t.Run(codec, func(t *testing.T) {
			client := Options{Transport: "vyandex", URL: testURL, Codec: codec, PreparedKey: key, Mobile: true}
			exit := Options{Transport: "vyandex", URL: testURL, Codec: codec, EncryptionSecret: testSecret, ExitNode: true}
			c, s, cr, sr := startPair(t, client, exit)
			p := []byte("prepared key uses the upstream KDF")
			if err := c.Send(p); err != nil {
				t.Fatal(err)
			}
			expectPacket(t, sr, p)
			if err := s.Send(p); err != nil {
				t.Fatal(err)
			}
			expectPacket(t, cr, p)
		})
	}
}

// Pin the pre-refactor CLI order independently of Wrap. A round trip between
// two Wrap calls alone would miss an accidental, symmetric protocol change.
func TestHistoricalCLIWireOrder(t *testing.T) {
	for _, codec := range []string{Batched, Legacy} {
		t.Run(codec, func(t *testing.T) {
			a, b := pair()
			client, err := Wrap(a, Options{Transport: "vyandex", URL: testURL, Codec: codec, EncryptionSecret: testSecret, Mobile: true})
			if err != nil {
				t.Fatal(err)
			}
			var oldCodec transport.Transport = transport.NewCompressedTransport(b)
			if codec == Batched {
				oldCodec = transport.NewBatchedTransport(b)
			}
			exit, err := transport.NewEncryptedTransport(oldCodec, testSecret, testURL, true)
			if err != nil {
				t.Fatal(err)
			}
			cr, sr := make(chan []byte, 1), make(chan []byte, 1)
			client.Receive(func(p []byte) { cr <- p })
			exit.Receive(func(p []byte) { sr <- p })
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			if err := exit.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Stop(); exit.Stop() })
			p := []byte("pre-refactor CLI fixture")
			if err := client.Send(p); err != nil {
				t.Fatal(err)
			}
			expectPacket(t, sr, p)
			if err := exit.Send(p); err != nil {
				t.Fatal(err)
			}
			expectPacket(t, cr, p)
		})
	}
}

func TestWrongSecretAndContextFailClosed(t *testing.T) {
	for _, codec := range []string{Batched, Legacy} {
		for _, mismatch := range []string{"secret", "context"} {
			t.Run(codec+"/"+mismatch, func(t *testing.T) {
				copts := Options{Transport: "vyandex", URL: testURL, Codec: codec, EncryptionSecret: testSecret, Mobile: true}
				sopts := copts
				sopts.ExitNode = true
				if mismatch == "secret" {
					sopts.EncryptionSecret = "different-synthetic-test-secret"
				} else {
					sopts.URL = "https://document.invalid/other"
				}
				c, s, cr, sr, waitDelivery := startObservedPair(t, copts, sopts)
				if err := c.Send([]byte("must not authenticate")); err != nil {
					t.Fatal(err)
				}
				if err := s.Send([]byte("must not authenticate")); err != nil {
					t.Fatal(err)
				}
				waitDelivery()
				select {
				case <-cr:
					t.Fatal("wrong key accepted")
				case <-sr:
					t.Fatal("wrong key accepted")
				default:
				}
			})
		}
	}
}

func TestCodecMismatchDropsWithoutPanic(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			client := Options{Transport: "yandex", URL: testURL, Codec: Batched, Mobile: true}
			if encrypted {
				client.EncryptionSecret = testSecret
			}
			exit := client
			exit.Codec = Legacy
			exit.ExitNode = true
			c, s, cr, sr, waitDelivery := startObservedPair(t, client, exit)
			if err := c.Send([]byte("wrong codec")); err != nil {
				t.Fatal(err)
			}
			if err := s.Send([]byte("wrong codec")); err != nil {
				t.Fatal(err)
			}
			waitDelivery()
			select {
			case <-cr:
				t.Fatal("mismatched codec accepted")
			case <-sr:
				t.Fatal("mismatched codec accepted")
			default:
			}
		})
	}
}

func TestStackValidatesWithoutDisclosingInputs(t *testing.T) {
	for _, o := range []Options{{Codec: "private-bad-codec"}, {Codec: Legacy, EncryptionSecret: "short-secret"}, {Codec: Batched, PreparedKey: []byte("bad-key")}} {
		_, err := Wrap(&endpoint{}, o)
		if err == nil {
			t.Fatal("invalid settings accepted")
		}
		for _, secret := range []string{o.EncryptionSecret, string(o.PreparedKey), "private-bad-codec"} {
			if secret != "" && bytes.Contains([]byte(err.Error()), []byte(secret)) {
				t.Fatal("input leaked in error")
			}
		}
	}
}

func TestFactoryUsesRequestedYandexTransport(t *testing.T) {
	for _, kind := range []string{"yandex", "vyandex"} {
		for _, codec := range []string{Batched, Legacy} {
			t.Run(kind+"/"+codec, func(t *testing.T) {
				tr, err := New(Options{Transport: kind, URL: testURL, Codec: codec, Mobile: true})
				if err != nil {
					t.Fatal(err)
				}
				var raw transport.Transport
				switch v := tr.(type) {
				case *transport.BatchedTransport:
					raw = v.Transport
				case *transport.CompressedTransport:
					raw = v.Transport
				default:
					t.Fatal("wrong codec constructor")
				}
				want := "*yandex.YandexDocsTransport"
				if kind == "vyandex" {
					want = "*yandex.YandexVolgaTransport"
				}
				if fmt.Sprintf("%T", raw) != want {
					t.Fatal("wrong underlying transport")
				}
			})
		}
	}
}
