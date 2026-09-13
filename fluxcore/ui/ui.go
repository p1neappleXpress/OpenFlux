// Package ui serves the Flux local web interface. The page is embedded in the
// binary so the client works fully offline — no external fonts, scripts or
// styles — which matters for a privacy tool on a restricted network (spec §92).
package ui

import (
	"embed"
	"net/http"
)

//go:embed index.html
var files embed.FS

// Handler serves the Flux UI. Mount it at "/" on the same mux as the Control
// API so the page and its /api/* endpoints share one origin.
func Handler() http.Handler {
	index, _ := files.ReadFile("index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}
