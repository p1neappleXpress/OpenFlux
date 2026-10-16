package subscription

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// Statuses of a subscription. Anything but active comes with no profiles.
const (
	StatusActive  = "active"
	StatusExpired = "expired"
	StatusRevoked = "revoked"
)

// Document is the subscription server's answer.
type Document struct {
	Version int    `json:"version"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	// Expires is unix seconds; 0 never.
	Expires int64 `json:"expires"`
	// UpdateHours is how often the app should ask again.
	UpdateHours int `json:"updateHours"`
	// Support is where to go to renew or ask: a link the app opens.
	Support string `json:"support,omitempty"`
	// Announce is a message the owner wants shown.
	Announce string    `json:"announce,omitempty"`
	Profiles []Profile `json:"profiles"`
}

// Profile is one location: a stable id (the app recognises it on updates), what
// the user sees, and the openflux:// link with the key.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Link string `json:"link"`
}

const (
	maxProfiles = 64
	maxTextLen  = 300
)

var profileID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Check validates a document from the network: the version, the status, every
// profile's id and link (read by the core's own parser). A profile that does not
// pass is dropped, not trusted; skipped counts them. The document is cleaned in
// place.
func (d *Document) Check() (skipped int, err error) {
	if d.Version != 1 {
		return 0, fmt.Errorf("subscription: unsupported document version %d", d.Version)
	}
	switch d.Status {
	case StatusActive, StatusExpired, StatusRevoked:
	default:
		return 0, fmt.Errorf("subscription: unknown status %q", d.Status)
	}
	d.Title = clip(d.Title)
	d.Support = clip(d.Support)
	d.Announce = clip(d.Announce)
	if d.UpdateHours < 1 {
		d.UpdateHours = 6
	}
	if d.UpdateHours > 24*7 {
		d.UpdateHours = 24 * 7
	}
	if d.Status != StatusActive {
		d.Profiles = []Profile{}
		return 0, nil
	}
	seen := map[string]bool{}
	kept := make([]Profile, 0, len(d.Profiles))
	for _, p := range d.Profiles {
		p.Name = clip(p.Name)
		p.Link = strings.TrimSpace(p.Link)
		if !profileID.MatchString(p.ID) || seen[p.ID] || p.Name == "" || len(kept) >= maxProfiles {
			skipped++
			continue
		}
		if r := share.Read(p.Link); r.Error != "" || r.Config == nil {
			skipped++
			continue
		}
		seen[p.ID] = true
		kept = append(kept, p)
	}
	d.Profiles = kept
	if len(kept) == 0 {
		return skipped, errors.New("subscription: the document has no usable profiles")
	}
	return skipped, nil
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxTextLen {
		s = string(r[:maxTextLen])
	}
	return s
}

// Result is what every entry point answers about a fetch, as the same JSON.
type Result struct {
	OK           bool      `json:"ok"`
	Subscription *Document `json:"subscription,omitempty"`
	// Skipped counts profiles the document held that did not pass the check.
	Skipped int `json:"skipped,omitempty"`
	// Invite is the key as normalised (the apps keep it).
	Invite *Invite `json:"invite,omitempty"`
	Error  string  `json:"error,omitempty"`
	// Code lets the app word the problem itself: bad_invite, network, bad_pin,
	// not_found, bad_response, too_large.
	Code string `json:"code,omitempty"`
}

// JSON is r as the apps receive it.
func (r Result) JSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"ok":false,"error":"marshal failed"}`
	}
	return string(b)
}

func failed(code string, err error) Result {
	return Result{OK: false, Code: code, Error: err.Error()}
}
