package yandexcbir

import (
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The suggest mailbox: a slot remembers the last few search strings typed with
// its cookie pair, and answers them back. That history is the channel: one
// short lower-case token per message.
const (
	suggestURL = "https://yandex.ru/suggest-images/suggest-ya.cgi?srv=images_ru_desktop" +
		"&lr=10466&uil=ru&nl=1&v=4&n=10&portal=1&fact=1&icon=1&hl=1" +
		"&nav_favicon=1&nav_text=1&verified_nav=1&history=1"
	searchURL = "https://yandex.ru/images/search?text="
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0"
	// tag prefixes every message so a slot's own history can be told apart
	tag = "ofx"
	// maxToken is the longest string the suggest history keeps (118 chars).
	maxToken = 118
)

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// publish writes id into the slot and makes the suggest endpoint remember it.
func Publish(c *http.Client, s Slot, id string) error {
	tok := tag + b32.EncodeToString([]byte(id))
	if len(tok) > maxToken {
		return fmt.Errorf("yandexcbir: token too long (%d)", len(tok))
	}
	h := map[string]string{
		"User-Agent":      userAgent,
		"Accept":          "text/html,application/xhtml+xml",
		"Accept-Language": "ru-RU,ru;q=0.9,en;q=0.8",
	}
	// The search page notes the text into the slot's history; the suggest call
	// right after is what makes it stick ("pers":1).
	if _, err := slotDo(c, s, "GET", searchURL+tok, h); err != nil {
		return err
	}
	// The images page in between is what makes the history entry stick: the
	// reference does the same, and without it the suggest call answers nothing.
	if _, err := slotDo(c, s, "GET", "https://yandex.ru/images/", h); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	u := suggestURL + "&yu=" + s.YU + "&part=" + tok
	if _, err := slotDo(c, s, "GET", u, h); err != nil {
		return err
	}
	return nil
}

// read returns the ids the slot currently remembers, newest first.
func Read(c *http.Client, s Slot) []string {
	u := suggestURL + "&yu=" + s.YU + "&part=" + tag
	body, err := slotDo(c, s, "GET", u, map[string]string{"User-Agent": userAgent})
	if err != nil {
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(body, &arr) != nil || len(arr) < 2 {
		return nil
	}
	var items [][3]json.RawMessage
	if json.Unmarshal(arr[1], &items) != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		var text string
		if json.Unmarshal(it[1], &text) != nil || !strings.HasPrefix(text, tag) {
			continue
		}
		var meta struct {
			Pers int `json:"pers"`
		}
		if json.Unmarshal(it[2], &meta) != nil || meta.Pers != 1 {
			continue
		}
		raw, err := b32.DecodeString(strings.TrimPrefix(text, tag))
		if err != nil || len(raw) < 8 {
			continue
		}
		out = append(out, string(raw))
	}
	return out
}

func slotDo(c *http.Client, s Slot, method, url string, h map[string]string) ([]byte, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	// A fresh cookie pair per request: the jar would add a second, foreign i=.
	req.Header.Set("Cookie", "i="+s.I+"; yandexuid="+s.YU)
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}
