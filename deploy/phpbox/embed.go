// Package phpbox holds the PHP exit as files embedded in the core, so any
// client (the CLI, Android, iOS, Desktop) can put it on a web host without
// shipping the sources separately. The files are what build-bundle.sh copies
// and deploy/phpbox/README.md describes; provision/phphost uploads them.
package phpbox

import (
	"embed"
	"io/fs"
	"sort"
)

// files: the exits, lib/, the page and the core's link parser compiled to
// WebAssembly (assets/, built by build-wasm.sh and committed: it changes only
// when the openflux:// link format does). Tests and selftests stay out.
//
//go:embed cupsexit.php mailruexit.php phpbox.php lib/*.php lib/page.html assets/share.wasm.gz assets/wasm_exec.js
var files embed.FS

// File is one file of the bundle and where it goes, relative to the web root.
type File struct {
	Path string
	Data []byte
}

// Files lists the bundle, directories first in path order so uploads create
// lib/ and assets/ before their contents.
func Files() []File {
	var out []File
	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := files.ReadFile(p)
		if err == nil {
			out = append(out, File{Path: p, Data: b})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
