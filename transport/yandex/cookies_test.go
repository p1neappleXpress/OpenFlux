package yandex

import (
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeCookieFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cookieNamesFor(t *testing.T, jar *cookiejar.Jar, rawURL string) map[string]string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string)
	for _, c := range jar.Cookies(u) {
		got[c.Name] = c.Value
	}
	return got
}

func TestLoadYandexCookiesScopesAndExpiry(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()
	path := writeCookieFile(t, strings.Join([]string{
		"# Netscape HTTP Cookie File",
		".yandex.ru\tTRUE\t/\tTRUE\t" + strconv.FormatInt(future, 10) + "\tSession_id\taccount-secret",
		"#HttpOnly_.yandex.ru\tTRUE\t/\tTRUE\t0\tyandexuid\thttp-only-secret",
		".yandex.ru\tTRUE\t/\tTRUE\t" + strconv.FormatInt(past, 10) + "\texpired\told-secret",
		".example.com\tTRUE\t/\tTRUE\t" + strconv.FormatInt(future, 10) + "\tother\tother-secret",
		".evil-yandex.ru\tTRUE\t/\tTRUE\t" + strconv.FormatInt(future, 10) + "\tlookalike\tevil-secret",
	}, "\n"))
	jar, _ := cookiejar.New(nil)
	if err := loadYandexCookies(path, jar); err != nil {
		t.Fatal(err)
	}
	got := cookieNamesFor(t, jar, "https://docs.yandex.ru/edit/d/test")
	if got["Session_id"] != "account-secret" || got["yandexuid"] != "http-only-secret" {
		t.Fatalf("Yandex cookies not loaded: %v", got)
	}
	if _, ok := got["expired"]; ok {
		t.Fatal("expired cookie was loaded")
	}
	if len(cookieNamesFor(t, jar, "https://example.com/")) != 0 || len(cookieNamesFor(t, jar, "https://evil-yandex.ru/")) != 0 {
		t.Fatal("cookies leaked to unrelated domain")
	}
}

func TestLoadYandexCookiesRejectsEmptyAndMalformed(t *testing.T) {
	for _, contents := range []string{"# no cookies\n", ".yandex.ru\tTRUE\t/\tTRUE\t0\tSession_id\n"} {
		jar, _ := cookiejar.New(nil)
		err := loadYandexCookies(writeCookieFile(t, contents), jar)
		if err == nil {
			t.Fatalf("accepted invalid cookie file %q", contents)
		}
		if strings.Contains(err.Error(), "Session_id") {
			t.Fatalf("error exposed cookie name: %v", err)
		}
	}
}

func TestLoadYandexCookiesAcceptsEmptyValue(t *testing.T) {
	path := writeCookieFile(t, ".yandex.ru\tTRUE\t/\tTRUE\t0\tempty\t\n")
	jar, _ := cookiejar.New(nil)
	if err := loadYandexCookies(path, jar); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeInvalidCookieFileFailsBeforeRequest(t *testing.T) {
	_, err := authorize("https://docs.yandex.ru/edit/d/test", writeCookieFile(t, "invalid"))
	if err == nil || !strings.Contains(err.Error(), "cookie") {
		t.Fatalf("expected cookie file error, got %v", err)
	}
}
