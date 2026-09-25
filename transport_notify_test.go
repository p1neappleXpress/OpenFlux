package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestParseCookieHeader(t *testing.T) {
	got := parseCookieHeader(" spravka=abc ; yandexuid=42; broken ; =nokey; k=v=w ")
	want := map[string]string{"spravka": "abc", "yandexuid": "42", "k": "v=w"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: got %q, want %q", k, got[k], v)
		}
	}
}

// Файл с куками — единственный путь пройти интерактивную капчу на exit-ноде,
// поэтому проверяем весь путь: файл появился после старта наблюдателя -> куки
// доехали до зарегистрированного транспорта.
func TestWatchCookiesFilePicksUpLateFile(t *testing.T) {
	resetYandexRegistry()
	tr := newYandexDocs("https://disk.yandex.ru/i/test", transport.DefaultConfig())
	defer tr.Stop()

	path := filepath.Join(t.TempDir(), "cookies.txt")
	go watchCookiesFile(path) // файла ещё нет — наблюдатель должен дождаться

	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(path, []byte("spravka=s1; yandexuid=u1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ydocsMu.Lock()
	target := ydocsActive[0]
	ydocsMu.Unlock()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := target.FetchCookies(); err == nil && got["spravka"] == "s1" {
			if got["yandexuid"] != "u1" {
				t.Fatalf("second cookie lost: %v", got)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("cookies from the watched file never reached the transport")
}

// Флаг существует только чтобы оператор мог разблокировать ноду; пустой файл не
// должен ничего затирать.
func TestWatchCookiesFileIgnoresEmpty(t *testing.T) {
	resetYandexRegistry()
	tr := newYandexDocs("https://disk.yandex.ru/i/test2", transport.DefaultConfig())
	defer tr.Stop()

	ydocsMu.Lock()
	target := ydocsActive[0]
	ydocsMu.Unlock()
	if err := target.ApplyCookies(map[string]string{"keep": "me"}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	go watchCookiesFile(path)
	time.Sleep(500 * time.Millisecond)

	got, err := target.FetchCookies()
	if err != nil {
		t.Fatal(err)
	}
	if got["keep"] != "me" {
		t.Fatalf("empty cookies file clobbered the jar: %v", got)
	}
}
