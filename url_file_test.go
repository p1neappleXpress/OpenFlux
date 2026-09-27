package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadURLFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "url")
	content := append([]byte{0xEF, 0xBB, 0xBF}, "# exit document\r\n\r\n  https://disk.yandex.ru/i/abc  \r\n"...)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readURLFile(p)
	if err != nil || got != "https://disk.yandex.ru/i/abc" {
		t.Fatalf("got %q, %v", got, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# nothing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readURLFile(empty); err == nil {
		t.Fatal("a file with no URL was accepted")
	}
}
