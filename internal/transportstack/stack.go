// Package transportstack owns the wire-compatible stack shared by the CLI and
// mobile bridges. Resource limits may differ; framing, KDF and wrapper order may not.
package transportstack

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"openflux/transport"
	"openflux/transport/cupsonline"
	"openflux/transport/mailru"
	"openflux/transport/oneme"
	"openflux/transport/yandex"
)

const (
	Batched = "batched"
	Legacy  = "legacy"
)

type Options struct {
	DialContext                      transport.DialContextFunc
	Transport, URL, MAXToken, MAXUID string
	Codec, EncryptionSecret          string
	// PreparedKey is the output of transport.DeriveEncryptionKey, NOT a new
	// secret/KDF. iOS derives it in the containing app and stores it in Keychain
	// so the extension does not incur scrypt's 32 MiB scratch allocation.
	PreparedKey      []byte
	ExitNode, Mobile bool
}

// EncryptionContext preserves the CLI's existing salt selection exactly.
// Do not normalize URLs here: both peers must use the same document URL string.
func EncryptionContext(kind, url string) string {
	if url != "" {
		return url
	}
	return kind
}

func New(o Options) (transport.Transport, error) {
	if err := validate(o); err != nil {
		return nil, err
	}
	if o.Transport == "yandex" || o.Transport == "vyandex" {
		u, err := url.Parse(o.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("invalid document URL")
		}
	}
	cfg := transport.DefaultConfig()
	cfg.DialContext = o.DialContext
	if o.Mobile {
		cfg.MaxQueueSize = 256
	}
	var raw transport.Transport
	switch o.Transport {
	case "yandex":
		raw = yandex.NewYandexDocsTransport(o.URL, cfg)
	case "vyandex":
		if o.Mobile {
			raw = yandex.NewYandexVolgaTransport(o.URL, cfg, yandex.MobileVolgaConfig())
		} else {
			raw = yandex.NewYandexVolgaTransport(o.URL, cfg)
		}
	case "oneme":
		uid, err := strconv.ParseInt(o.MAXUID, 10, 64)
		if err != nil {
			return nil, errors.New("invalid MAX user ID")
		}
		raw = oneme.NewOneMeTransport(o.ExitNode, o.MAXToken, uid, cfg)
	case "cupsonline":
		raw = cupsonline.NewCupsonlineTransport(o.URL, cfg, !o.ExitNode)
	case "mailru":
		raw = mailru.NewMailruDocsTransport(o.URL, cfg)
	default:
		return nil, errors.New("unsupported transport")
	}
	return Wrap(raw, o)
}

func validate(o Options) error {
	if o.Codec != Batched && o.Codec != Legacy {
		return errors.New("unsupported codec")
	}
	secret := strings.TrimSpace(o.EncryptionSecret)
	if secret != "" && (!utf8.ValidString(secret) || utf8.RuneCountInString(secret) < 16) {
		return errors.New("encryption secret must contain at least 16 characters")
	}
	if len(o.PreparedKey) != 0 && (len(o.PreparedKey) != 32 || secret != "") {
		return errors.New("invalid prepared encryption key")
	}
	return nil
}

// Wrap preserves the ACTUAL upstream CLI wire format: raw <- codec <- AES.
// Sending encrypts individual packets, then frames/compresses them. Although
// compress-before-encrypt is more efficient, changing this order would silently
// break existing encrypted CLI peers. Any future reorder needs wire versioning.
// Send on the returned stack always owns its payload before returning, allowing
// the iOS bridge to borrow its C buffer for the duration of the call.
func Wrap(raw transport.Transport, o Options) (transport.Transport, error) {
	if raw == nil {
		return nil, errors.New("missing transport")
	}
	if err := validate(o); err != nil {
		return nil, err
	}
	var t transport.Transport = raw
	if o.Codec == Batched {
		if o.Mobile {
			t = transport.NewBatchedTransportWithLimits(t, 256, 512<<10)
		} else {
			t = transport.NewBatchedTransport(t)
		}
	} else {
		t = transport.NewCompressedTransport(t)
	}
	if len(o.PreparedKey) != 0 {
		return transport.NewEncryptedTransportWithKey(t, o.PreparedKey, o.ExitNode)
	}
	if secret := strings.TrimSpace(o.EncryptionSecret); secret != "" {
		return transport.NewEncryptedTransport(t, secret, EncryptionContext(o.Transport, o.URL), o.ExitNode)
	}
	return t, nil
}
