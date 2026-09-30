#!/usr/bin/env bash
# Builds the browser parser for the phpbox page: the core's own share package
# (openflux:// links + QR) compiled to WebAssembly, gzipped, plus Go's loader.
# Output: deploy/phpbox/assets/{share.wasm.gz,wasm_exec.js}  (git-ignored: build artifacts)
set -euo pipefail
cd "$(dirname "$0")/../.."
out=deploy/phpbox/assets
mkdir -p "$out"
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$out/share.wasm" ./cmd/sharewasm
gzip -9 -f -c "$out/share.wasm" > "$out/share.wasm.gz"
rm "$out/share.wasm"
cp -f "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/wasm_exec.js" && chmod u+w "$out/wasm_exec.js"
ls -la "$out"
