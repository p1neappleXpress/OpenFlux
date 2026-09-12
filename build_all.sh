#!/usr/bin/env bash
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

OUTPUT_DIR="github_output"
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
COMMIT_HASH=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")

DEFAULT_PLATFORMS=(linux windows darwin)

print_info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
print_success() { echo -e "${GREEN}[OK]${NC} $1"; }
print_warn()    { echo -e "${YELLOW}[WARN]${NC} $1"; }
print_error()   { echo -e "${RED}[ERROR]${NC} $1"; }
print_header()  { echo -e "\n${BOLD}=== $1 ===${NC}\n"; }

check_env() {
    if ! command -v go &> /dev/null; then
        print_error "Go is not installed"
        exit 1
    fi
    print_info "Go version: $(go version | awk '{print $3}')"
}

prepare() {
    rm -rf "$OUTPUT_DIR"
    mkdir -p "$OUTPUT_DIR"
    print_info "Output directory: $OUTPUT_DIR"

    print_info "Downloading dependencies..."
    go mod tidy
}

build_one() {
    local os=$1
    local arch=$2
    local output_name=$3

    print_info "Building $os/$arch -> $output_name"

    export GOOS=$os
    export GOARCH=$arch
    export CGO_ENABLED=0

    if [[ "$os" == "windows" ]]; then
        output_name="${output_name}.exe"
    fi

    local ldflags="-s -w \
        -X main.Version=$VERSION \
        -X main.BuildDate=$BUILD_DATE \
        -X main.CommitHash=$COMMIT_HASH"

    if go build -ldflags "$ldflags" -o "$OUTPUT_DIR/$output_name" .; then
        chmod +x "$OUTPUT_DIR/$output_name" 2>/dev/null || true
        local size=$(du -h "$OUTPUT_DIR/$output_name" | cut -f1)
        print_success "$output_name ($size)"

        local sha256
        if command -v sha256sum &> /dev/null; then
            sha256=$(sha256sum "$OUTPUT_DIR/$output_name" | cut -d' ' -f1)
        elif command -v shasum &> /dev/null; then
            sha256=$(shasum -a 256 "$OUTPUT_DIR/$output_name" | cut -d' ' -f1)
        fi
        if [[ -n "$sha256" ]]; then
            echo "$sha256  $output_name" >> "$OUTPUT_DIR/SHA256SUMS"
        fi
    else
        print_error "Build failed for $os/$arch"
        return 1
    fi
}

build_all() {
    local platforms=("$@")
    if [[ ${#platforms[@]} -eq 0 ]]; then
        platforms=("${DEFAULT_PLATFORMS[@]}")
    fi

    for p in "${platforms[@]}"; do
        print_header "Platform: $p"

        case "$p" in
            linux)
                build_one linux amd64 openflux-linux-amd64
                build_one linux arm64 openflux-linux-arm64
                build_one linux arm   openflux-linux-arm
                ;;
            windows)
                build_one windows amd64 openflux-windows-amd64
                build_one windows 386   openflux-windows-386
                build_one windows arm64 openflux-windows-arm64
                ;;
            darwin)
                build_one darwin amd64 openflux-darwin-amd64
                build_one darwin arm64 openflux-darwin-arm64
                ;;
            *)
                print_warn "Unknown platform: $p"
                ;;
        esac
    done
}

generate_readme() {
    print_header "Generating README"

    cat > "$OUTPUT_DIR/README.md" << EOF
# OpenFlux Binaries

**Version:** \`$VERSION\`
**Build date:** $BUILD_DATE
**Commit:** \`$COMMIT_HASH\`

## Binaries

| Platform | File | Purpose |
|----------|------|---------|
| Linux amd64 | \`openflux-linux-amd64\` | Client / Exit node |
| Linux arm64 | \`openflux-linux-arm64\` | Client / Exit node |
| Linux arm   | \`openflux-linux-arm\`   | Client |
| macOS amd64 | \`openflux-darwin-amd64\` | Client |
| macOS arm64 | \`openflux-darwin-arm64\` | Client |
| Windows amd64 | \`openflux-windows-amd64.exe\` | Client / Exit node |
| Windows 386 | \`openflux-windows-386.exe\` | Client |
| Windows arm64 | \`openflux-windows-arm64.exe\` | Client |

## Usage

### Exit node (Linux, root required)

\`\`\`bash
sudo ./openflux-linux-amd64 \\
    --exit-node \\
    --url "https://disk.yandex.ru/i/YOUR_DOC" \\
    --debug
\`\`\`

### Client

\`\`\`bash
./openflux-linux-amd64 \\
    --client \\
    --url "https://disk.yandex.ru/i/YOUR_DOC" \\
    --socks5 :1080 \\
    --debug
\`\`\`

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| \`--client\` | | Run as client |
| \`--exit-node\` | | Run as exit node |
| \`--url\` | | Yandex Docs URL or static JSON path |
| \`--maxToken\` | | MAX token |
| \`--maxUid\` | | MAX user ID |
| \`--socks5\` | \`:1080\` | SOCKS5 listen address |
| \`--transport\` | \`yandex\` | \`yandex\`, \`yandex-volga\`, or \`oneme\` |
| \`--debug\` | \`false\` | Verbose logging |

## SHA256

See \`SHA256SUMS\`.

## License

GNU General Public License v3.0 or later. See \`LICENSE\` in the repository.
EOF

    print_success "README.md generated"
}

print_summary() {
    print_header "Done"

    local total=0
    for f in "$OUTPUT_DIR"/*; do
        [[ -f "$f" ]] || continue
        total=$((total + 1))
    done

    print_info "Files: $total"
    echo ""
    ls -lh "$OUTPUT_DIR/" | tail -n +2
    echo ""
    print_success "Output: $OUTPUT_DIR/"
}

main() {
    echo -e "${BOLD}OpenFlux build_all.sh${NC}"
    echo -e "Version: ${YELLOW}$VERSION${NC}"
    echo -e "Date:    $BUILD_DATE"
    echo -e "Commit:  $COMMIT_HASH"
    echo ""

    check_env
    prepare
    build_all "$@"
    generate_readme
    print_summary
}

main "$@"
