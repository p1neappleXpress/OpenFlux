package yandex

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// loadYandexCookies imports a Netscape cookies.txt export into an HTTP cookie
// jar. Only yandex.ru and its subdomains are accepted; cookie values are never
// included in error messages.
func loadYandexCookies(path string, jar http.CookieJar) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open Yandex cookie file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	loaded := 0
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" || (strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_")) {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return fmt.Errorf("Yandex cookie file line %d: expected 7 tab-separated fields", lineNo)
		}
		rawDomain := strings.TrimPrefix(fields[0], "#HttpOnly_")
		domain := strings.ToLower(strings.TrimPrefix(rawDomain, "."))
		if domain != "yandex.ru" && !strings.HasSuffix(domain, ".yandex.ru") {
			continue
		}
		if fields[1] != "TRUE" && fields[1] != "FALSE" {
			return fmt.Errorf("Yandex cookie file line %d: invalid subdomain flag", lineNo)
		}
		if fields[3] != "TRUE" && fields[3] != "FALSE" {
			return fmt.Errorf("Yandex cookie file line %d: invalid secure flag", lineNo)
		}
		expires, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return fmt.Errorf("Yandex cookie file line %d: invalid expiry", lineNo)
		}
		if expires != 0 && expires <= time.Now().Unix() {
			continue
		}
		if fields[5] == "" {
			return fmt.Errorf("Yandex cookie file line %d: empty cookie name", lineNo)
		}
		path := fields[2]
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("Yandex cookie file line %d: invalid cookie path", lineNo)
		}
		cookie := &http.Cookie{
			Name:   fields[5],
			Value:  fields[6],
			Path:   path,
			Secure: fields[3] == "TRUE",
		}
		if fields[1] == "TRUE" {
			cookie.Domain = domain
		}
		if expires != 0 {
			cookie.Expires = time.Unix(expires, 0)
		}
		jar.SetCookies(&url.URL{Scheme: "https", Host: domain, Path: path}, []*http.Cookie{cookie})
		loaded++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Yandex cookie file: %w", err)
	}
	if loaded == 0 {
		return fmt.Errorf("Yandex cookie file has no unexpired yandex.ru cookies")
	}
	return nil
}
