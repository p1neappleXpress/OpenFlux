// Package yandexcbir carries OpenFlux packets through Yandex Images'
// search-by-image storage: the payload rides in a PNG chunk, the cbirId of
// that PNG travels in a Yandex suggest slot.
package yandexcbir

import (
	"errors"
	"strings"
)

// Slot is one suggest identity: the cookie pair whose suggest history is the
// mailbox, plus the id the pool names it by.
type Slot struct {
	ID string // base64url token, as the pool spells it
	I  string // the "i" cookie, case sensitive
	YU string // the "yandexuid" cookie
}

// Pool is the slot set both ends share: two halves that never overlap, so the
// two sides never write the same slot.
type Pool struct {
	C2S []Slot // slots the client writes
	S2C []Slot // slots the exit writes
	Raw string
}

// ParsePool reads "ofx1:<c2s>|<s2c>", each side a list of "<id>.<yu>" entries
// separated by commas or semicolons. The id is the i cookie, base64url.
func ParsePool(s string) (*Pool, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "ofx1:") {
		return nil, errors.New("yandexcbir: pool must look like ofx1:<c2s>|<s2c>")
	}
	body := strings.TrimPrefix(s, "ofx1:")
	c2s, s2c := body, ""
	if bar := strings.Index(body, "|"); bar >= 0 {
		c2s, s2c = body[:bar], body[bar+1:]
	}
	p := &Pool{Raw: s, C2S: parseSlots(c2s), S2C: parseSlots(s2c)}
	if len(p.C2S) < 4 || len(p.S2C) < 4 {
		return nil, errors.New("yandexcbir: each side of the pool needs at least 4 slots")
	}
	return p, nil
}

func parseSlots(s string) []Slot {
	var out []Slot
	for _, e := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		dot := strings.LastIndex(e, ".")
		if dot <= 0 || dot == len(e)-1 {
			continue
		}
		out = append(out, Slot{ID: e[:dot], I: unURL(e[:dot]), YU: e[dot+1:]})
	}
	return out
}

// unURL turns base64url back into standard base64: the pool spells ids in the
// URL alphabet so they survive a link, the cookie wants the plain one.
func unURL(s string) string {
	s = strings.ReplaceAll(s, "-", "+")
	return strings.ReplaceAll(s, "_", "/")
}
