// Package subscription is OpenFlux's subscription: a key (link or QR) the user
// pastes into the app, and a document the subscription server answers with, the
// list of profiles (openflux:// links) the user may connect through, and until
// when. The format is in docs/subscriptions.ru.md.
//
// The core is the one place that fetches and checks a subscription, so the
// desktop app (the CLI) and the Android app (package mobile) behave alike.
package subscription

import (
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strings"
)

// Link prefix of a subscription key; profile links are openflux://v1/….
const invitePrefix = "openflux://sub"

// Invite is what a subscription key says: where to fetch the document and which
// certificate to trust there.
type Invite struct {
	// URL is the https address of the subscription document. Its path holds the
	// secret token: treat it like a key.
	URL string `json:"url"`
	// Pin is the hex SHA-256 of the server's certificate for a self-signed one
	// (an IP address without a domain); "" checks the certificate the usual way.
	Pin string `json:"pin,omitempty"`
	// Name is a hint for the title until the server answers.
	Name string `json:"name,omitempty"`
}

// ErrInvite says a text is not a subscription key.
var ErrInvite = errors.New("subscription: not a subscription key")

// LooksLikeInvite is the cheap check an app uses to route pasted text: a
// subscription key rather than a profile link.
func LooksLikeInvite(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, invitePrefix) {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && strings.Contains(u.Path, "/sub/")
}

// ParseInvite reads a subscription key: openflux://sub?u=<url>&p=<pin>&n=<name>,
// or the plain document address, with the pin after the # (…/sub/<token>#pin=<hex>).
func ParseInvite(s string) (Invite, error) {
	s = strings.TrimSpace(s)
	var inv Invite
	switch {
	case strings.HasPrefix(s, invitePrefix):
		q, err := url.ParseQuery(strings.TrimPrefix(strings.TrimPrefix(s, invitePrefix), "?"))
		if err != nil {
			return Invite{}, ErrInvite
		}
		inv = Invite{URL: q.Get("u"), Pin: q.Get("p"), Name: strings.TrimSpace(q.Get("n"))}
	default:
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" {
			return Invite{}, ErrInvite
		}
		frag, _ := url.ParseQuery(u.Fragment)
		inv = Invite{Pin: frag.Get("pin")}
		u.Fragment = ""
		inv.URL = u.String()
	}
	return inv.normalize()
}

func (inv Invite) normalize() (Invite, error) {
	u, err := url.Parse(strings.TrimSpace(inv.URL))
	if err != nil || u.Host == "" || u.User != nil {
		return Invite{}, ErrInvite
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain http carries the token in the clear: only for this machine (tests, a local server).
		if !isLoopback(u.Hostname()) {
			return Invite{}, errors.New("subscription: the address must be https")
		}
	default:
		return Invite{}, ErrInvite
	}
	if !strings.Contains(u.Path, "/sub/") {
		return Invite{}, ErrInvite
	}
	u.Fragment = ""
	inv.URL = u.String()
	pin := strings.ToLower(strings.TrimSpace(inv.Pin))
	pin = strings.TrimPrefix(pin, "sha256:")
	pin = strings.NewReplacer(":", "", " ", "").Replace(pin)
	if pin != "" {
		if b, err := hex.DecodeString(pin); err != nil || len(b) != 32 {
			return Invite{}, errors.New("subscription: the certificate pin must be a SHA-256 in hex")
		}
	}
	inv.Pin = pin
	inv.Name = strings.TrimSpace(inv.Name)
	if len(inv.Name) > 80 {
		inv.Name = inv.Name[:80]
	}
	return inv, nil
}

// Link is the key as people share it: one openflux://sub link.
func (inv Invite) Link() string {
	q := url.Values{}
	q.Set("u", inv.URL)
	if inv.Pin != "" {
		q.Set("p", inv.Pin)
	}
	if inv.Name != "" {
		q.Set("n", inv.Name)
	}
	return invitePrefix + "?" + q.Encode()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
