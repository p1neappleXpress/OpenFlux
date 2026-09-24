package yandex

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"universal-bypass-tool/utils"
)

// solveCaptcha проходит Яндекс-капчу (blink-check) для заданного URL.
//
// Возвращает retpath (пустая строка = капча не требовалась).
// Cookies в jar обновляются на месте.
func solveCaptcha(docURL string, jar http.CookieJar, userAgent string) (string, error) {
	if jar == nil {
		return "", fmt.Errorf("captcha: nil cookiejar")
	}
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0"
	}

	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	utils.Debugf("[CAPTCHA] solve start: url=%s", docURL)

	captchaURL := ""
	currentURL := docURL
	for i := 0; i < 10; i++ {
		utils.Debugf("[CAPTCHA] GET %s", shortStr(currentURL, 120))
		req, _ := http.NewRequest("GET", currentURL, nil)
		setBrowserHeaders(req, userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("captcha GET: %w", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		utils.Debugf("[CAPTCHA]   status=%d location=%s",
			resp.StatusCode, shortStr(resp.Header.Get("Location"), 100))

		if resp.StatusCode == 200 {
			utils.Debugf("[CAPTCHA] 200 OK — капча не требуется")
			return "", nil
		}

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return "", fmt.Errorf("captcha unexpected status %d", resp.StatusCode)
		}

		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", fmt.Errorf("captcha: redirect without Location")
		}

		if strings.Contains(loc, "showcaptchafast") {
			captchaURL = loc
			break
		}
		currentURL = loc
	}

	if captchaURL == "" {
		return "", fmt.Errorf("captcha: showcaptchafast not found in redirect chain")
	}

	utils.Debugf("[CAPTCHA] GET showcaptchafast")
	req, _ := http.NewRequest("GET", captchaURL, nil)
	setBrowserHeaders(req, userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("captcha showcaptcha GET: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("captcha showcaptcha status %d", resp.StatusCode)
	}
	utils.Debugf("[CAPTCHA] showcaptcha: %d bytes", len(body))

	ssr, formAction, err := parseCaptchaHTML(string(body))
	if err != nil {
		return "", err
	}
	utils.Debugf("[CAPTCHA] uniqueKey=%s timestamp=%d complexity=%d prefix=%s",
		ssr.UniqueKey, ssr.Timestamp, ssr.Pow.Complexity, shortStr(ssr.Pow.Prefix, 32))

	t0 := time.Now()
	nonceHex, attempts := solveCaptchaPoW(ssr.Pow.Prefix, ssr.Pow.Complexity)
	utils.Debugf("[CAPTCHA] PoW solved: nonce=%s attempts=%d time=%v",
		nonceHex, attempts, time.Since(t0))

	fp := buildCaptchaFingerprint(nonceHex, userAgent)
	fpEncoded := encodeCaptchaFingerprint(fp)
	utils.Debugf("[CAPTCHA] fingerprint: json=%d encoded=%d bytes",
		len(mustMarshal(fp)), len(fpEncoded))

	form := url.Values{}
	form.Set("version", "1.5.0")
	form.Set("uniquekey", ssr.UniqueKey)
	form.Set("chstate", "ok")
	form.Set("fingerprint", fpEncoded)

	utils.Debugf("[CAPTCHA] POST %s", shortStr(formAction, 100))
	req2, _ := http.NewRequest("POST", formAction, strings.NewReader(form.Encode()))
	setBrowserHeaders(req2, userAgent)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Origin", "https://docs.yandex.ru")
	req2.Header.Set("Referer", captchaURL)

	resp2, err := client.Do(req2)
	if err != nil {
		return "", fmt.Errorf("captcha POST: %w", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	utils.Debugf("[CAPTCHA] POST result: status=%d location=%s",
		resp2.StatusCode, shortStr(resp2.Header.Get("Location"), 120))

	if resp2.StatusCode < 300 || resp2.StatusCode >= 400 {
		return "", fmt.Errorf("captcha POST unexpected status %d", resp2.StatusCode)
	}

	retpath := resp2.Header.Get("Location")
	if retpath == "" {
		retpath = docURL
	}

	utils.Debugf("[CAPTCHA] solve OK, retpath=%s", shortStr(retpath, 120))
	return retpath, nil
}

// ---- парсинг showcaptchafast ----

type captchaSSRData struct {
	UniqueKey string `json:"uniqueKey"`
	Action    string `json:"action"`
	Pow       struct {
		Complexity int    `json:"complexity"`
		Prefix     string `json:"prefix"`
	} `json:"pow"`
	Timestamp int64 `json:"timestamp"`
}

var (
	reSSRData    = regexp.MustCompile(`window\.__SSR_DATA__\s*=\s*JSON\.parse\(atob\("([^"]+)"\)\)`)
	reFormAction = regexp.MustCompile(`<form[^>]*id="tmgrdfrend-form"[^>]*action="([^"]+)"`)
)

func parseCaptchaHTML(html string) (*captchaSSRData, string, error) {
	m := reSSRData.FindStringSubmatch(html)
	if len(m) < 2 {
		return nil, "", fmt.Errorf("captcha: __SSR_DATA__ not found")
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return nil, "", fmt.Errorf("captcha: SSR_DATA base64: %w", err)
	}
	var ssr captchaSSRData
	if err := json.Unmarshal(raw, &ssr); err != nil {
		return nil, "", fmt.Errorf("captcha: SSR_DATA json: %w", err)
	}

	m2 := reFormAction.FindStringSubmatch(html)
	if len(m2) < 2 {
		return nil, "", fmt.Errorf("captcha: form action not found")
	}
	formAction := strings.ReplaceAll(m2[1], "&amp;", "&")
	if strings.HasPrefix(formAction, "/") {
		formAction = "https://docs.yandex.ru" + formAction
	}

	return &ssr, formAction, nil
}

// ---- PoW ----

func solveCaptchaPoW(prefixHex string, complexity int) (string, int) {
	prefix, err := hexDecode(prefixHex)
	if err != nil || len(prefix) == 0 {
		prefix = []byte(prefixHex)
	}

	var nonce [16]byte
	for attempts := 1; attempts < 10_000_000; attempts++ {
		ts := uint64(time.Now().UnixMilli())
		putU64LE(nonce[0:8], ts)
		putU64LE(nonce[8:16], uint64(rand.Int63()))

		h := sha256.New()
		h.Write(nonce[:])
		h.Write(prefix)
		sum := h.Sum(nil)

		if captchaCheckComplexity(sum, complexity) {
			return hexEncode(nonce[:]), attempts
		}
	}
	return "", 0
}

// captchaCheckComplexity — точная копия checkComplexity из fp.js.
func captchaCheckComplexity(h []byte, complexity int) bool {
	if complexity < 0 || complexity > 8*len(h) {
		return false
	}
	e, o := 0, 0
	for e <= complexity-8 {
		if h[o] != 0 {
			return false
		}
		e += 8
		o++
	}
	mask := byte(255) << uint(8+e-complexity)
	return h[o]&mask == 0
}

// ---- fingerprint ----

func buildCaptchaFingerprint(nonceHex, userAgent string) map[string]interface{} {
	return map[string]interface{}{
		"b6": 8, "b7": 8, "b9": []string{"en-US", "en"},
		"c2": "", "c4": "MacIntel", "c5": []interface{}{}, "c9": userAgent,
		"f4": 1080, "f5": 1920, "f6": 24, "f7": 1080, "f8": true,
		"f9": []int{1920, 1080}, "g1": 1920,
		"g2": "Europe/Moscow", "g3": -180,
		"j5": true,
		"m2": map[string]interface{}{"mTP": 0, "tE": false, "tS": false},
		"n6": false,
		"o2": 0, "o3": "srgb", "o4": 0, "o5": "en-US",
		"o8": nil, "o9": nil,
		"p1": nil, "p2": 0, "p3": nil, "p4": nil,
		"p5": nil, "p6": nil, "p8": []interface{}{}, "p9": "111111111",
		"j6": 48000,
		"a1": "",
		"a2": map[string]interface{}{"w": false, "d": ""},
		"a3": map[string]interface{}{
			"acos": 1.4444399284962483, "asin": 0.12349655394506357,
			"atan": 0.4636476090008061, "cos": -0.8390715290095377,
			"exp": 2.718281828459045, "log1p": 2.3978952727983707,
			"sin": -0.9917788534431158, "tan": -0.23206847684369653,
		},
		"a4": map[string]interface{}{"minDelta": 0.1, "maxDelta": 1.2},
		"a5": nil,
		"k4": []interface{}{},
		"j1": map[string]interface{}{
			"vn": "WebKit", "vr": "WebKit WebGL", "vU": "",
			"r": "Mozilla", "rU": "", "sLV": "WebGL GLSL ES 1.0 (1.0)",
		},
		"j2": map[string]interface{}{
			"cA": []interface{}{}, "p": []interface{}{}, "sP": []interface{}{},
			"e": []interface{}{}, "eP": []interface{}{},
		},
		"m10":     nonceHex,
		"version": "1.5.0",
	}
}

func encodeCaptchaFingerprint(fp map[string]interface{}) string {
	raw, _ := json.Marshal(fp)
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(raw)
	w.Close()
	return "~" + base64.StdEncoding.EncodeToString(buf.Bytes()) + "~"
}

// ---- helpers ----

func setBrowserHeaders(req *http.Request, userAgent string) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Sec-GPC", "1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
}

func hexEncode(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = h[v>>4]
		out[i*2+1] = h[v&0x0f]
	}
	return string(out)
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd hex length")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, lo := hexVal(s[i*2]), hexVal(s[i*2+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("invalid hex")
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func putU64LE(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
