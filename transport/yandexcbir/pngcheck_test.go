package yandexcbir

import (
	"bytes"
	"image/png"
	"testing"
)

func TestWrapIsValidPNG(t *testing.T) {
	b := WrapPNG([]byte("hello"))
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, err := UnwrapPNG(b); err != nil || string(got) != "hello" {
		t.Fatalf("unwrap: %q %v", got, err)
	}
}
