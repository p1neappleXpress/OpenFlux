//go:build js && wasm

// Command sharewasm exposes the core's openflux:// link reading, making and
// QR bitmap to a browser page, so the phpbox page parses links with the very
// code every client uses (one parser, nothing to drift). Answers are the same
// JSON the CLI's --parse-link prints (share.Result): codes and params, never
// user text; the page words them.
package main

import (
	"encoding/json"
	"strings"
	"syscall/js"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// qr answers {"rows":["0110..",..]} (1 = dark module, quiet zone included)
// or {"error":..}.
func qr(link string) string {
	bm, err := share.Bitmap(link)
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(b)
	}
	rows := make([]string, len(bm))
	for i, r := range bm {
		var sb strings.Builder
		for _, dark := range r {
			if dark {
				sb.WriteByte('1')
			} else {
				sb.WriteByte('0')
			}
		}
		rows[i] = sb.String()
	}
	b, _ := json.Marshal(map[string]any{"rows": rows})
	return string(b)
}

func main() {
	js.Global().Set("openfluxShare", js.ValueOf(map[string]any{
		"read": js.FuncOf(func(_ js.Value, a []js.Value) any { return share.Read(a[0].String()).JSON() }),
		"make": js.FuncOf(func(_ js.Value, a []js.Value) any { return share.MakeJSON(a[0].String()).JSON() }),
		"qr":   js.FuncOf(func(_ js.Value, a []js.Value) any { return qr(a[0].String()) }),
	}))
	select {}
}
