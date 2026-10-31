package ok

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	okOrigin = "https://ok.ru"
	okUA     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"
	// Known from user capture — adjust after HAR:
	getCallTokenPath = "https://ok.ru/web-api/v2/videochat/getCallToken"
	// Body {"id": N} — not peer uid. Captured values: 30, 38.
	callTokenAPIID = 30
)

func NewOkClient(cookies string) *OkClient {
	return &OkClient{
		cookies:  strings.TrimSpace(cookies),
		ua:       okUA,
		deviceID: genUUID(),
	}
}

func (c *OkClient) httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *OkClient) doJSON(method, rawURL string, body []byte, extraHeaders map[string]string) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Origin", okOrigin)
	req.Header.Set("Referer", okOrigin+"/messages")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	if c.cookies != "" {
		req.Header.Set("Cookie", c.cookies)
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}


// parseCookieHeader splits a Cookie request header into name→value.
func parseCookieHeader(h string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(h, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			continue
		}
		out[strings.TrimSpace(part[:i])] = strings.TrimSpace(part[i+1:])
	}
	return out
}

func cookieHeaderFromMap(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// mergeSetCookies applies Set-Cookie pairs into an existing Cookie header map.
// Empty noise cookies from ok.ru (vk_access_token, cookieChoice, landref) are skipped when empty.
func mergeSetCookies(cookieHeader string, setCookies []string) string {
	m := parseCookieHeader(cookieHeader)
	for _, sc := range setCookies {
		pair := strings.Split(sc, ";")[0]
		i := strings.IndexByte(pair, '=')
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(pair[:i])
		val := strings.TrimSpace(pair[i+1:])
		if name == "" {
			continue
		}
		if (name == "vk_access_token" || name == "cookieChoice" || name == "landref") && val == "" {
			continue
		}
		m[name] = val
	}
	return cookieHeaderFromMap(m)
}

// RefreshSessionCookies GETs ok.ru/ (and /messages) with the current jar and
// merges Set-Cookie into c.cookies. OK rotates JSESSIONID + AUTHCODE this way;
// call before GetCallToken so long-lived exit nodes keep a working token.
func (c *OkClient) RefreshSessionCookies() error {
	if strings.TrimSpace(c.cookies) == "" {
		return fmt.Errorf("ok: no cookies to refresh")
	}
	client := c.httpClient()
	refreshOne := func(rawURL string) error {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", c.ua)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		req.Header.Set("Cookie", c.cookies)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		sets := resp.Header.Values("Set-Cookie")
		if len(sets) == 0 {
			return nil
		}
		c.cookies = mergeSetCookies(c.cookies, sets)
		logInfo("cookie refresh %s: %d Set-Cookie", rawURL, len(sets))
		return nil
	}
	if err := refreshOne("https://ok.ru/"); err != nil {
		return err
	}
	_ = refreshOne("https://ok.ru/messages")
	return nil
}

// GetCallToken — POST /web-api/v2/videochat/getCallToken
//
// Real capture:
//   body: {"id": 30}   // NOT peer uid (also seen: 38)
//   response.result: { "apiUrl": "https://calls.okcdn.ru", "token": "$..." }
// Token is for calls.okcdn.ru (next hop), not videowebrtc ws2 yet.
func (c *OkClient) GetCallToken(peerID int64) (CallParams, error) {
	var out CallParams
	_ = peerID

	// Soft keep-alive: rotate JSESSIONID/AUTHCODE via GET ok.ru Set-Cookie.
	if err := c.RefreshSessionCookies(); err != nil {
		logInfo("cookie refresh skipped: %v", err)
	}

	payload, _ := json.Marshal(map[string]interface{}{"id": callTokenAPIID})
	body, code, err := c.doJSON(http.MethodPost, getCallTokenPath, payload, nil)
	if err != nil {
		return out, err
	}
	if code != 200 {
		return out, fmt.Errorf("getCallToken HTTP %d: %s", code, truncate(body, 300))
	}

	var wrap struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Result  struct {
			APIURL string `json:"apiUrl"`
			Token  string `json:"token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return out, fmt.Errorf("getCallToken JSON: %w body=%s", err, truncate(body, 300))
	}
	if !wrap.Success || wrap.Result.Token == "" {
		// One more soft refresh + retry (session may have been mid-rotate).
		_ = c.RefreshSessionCookies()
		body, code, err = c.doJSON(http.MethodPost, getCallTokenPath, payload, nil)
		if err != nil {
			return out, err
		}
		if code != 200 {
			return out, fmt.Errorf("getCallToken HTTP %d: %s", code, truncate(body, 300))
		}
		if err := json.Unmarshal(body, &wrap); err != nil {
			return out, fmt.Errorf("getCallToken JSON: %w body=%s", err, truncate(body, 300))
		}
		if !wrap.Success || wrap.Result.Token == "" {
			return out, fmt.Errorf("getCallToken failed: %s", truncate(body, 500))
		}
	}

	out.Token = wrap.Result.Token
	out.APIURL = wrap.Result.APIURL
	out.Endpoint = wrap.Result.APIURL
	logInfo("getCallToken ok: apiUrl=%s tokenLen=%d", out.APIURL, len(out.Token))
	return out, nil
}

// WaitIncomingCall is implemented in ok_push.go (PeriodicManager SSE + VCP).

// AcceptCall confirms the incoming call if API requires a separate step before ws2.
// TODO: from HAR (may be part of videowebrtc accept-call only).
func (c *OkClient) AcceptCall(convID string) error {
	logInfo("AcceptCall(%s): no HTTP step stubbed — relying on videowebrtc accept-call", convID)
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func stringField(m map[string]interface{}, names ...string) string {
	for _, n := range names {
		if v, ok := m[n]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}
