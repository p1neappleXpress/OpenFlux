#!/bin/bash
set -e

OUTPUT_DIR="output/ios"
LIBRARY_NAME="liboflux"
# Paths are this checkout's, whatever the caller's directory: an app that
# links the core as a submodule runs core/build_ios.sh from its own root.
ROOT="$(cd "$(dirname "$0")" && pwd)"
OUT="$ROOT/$OUTPUT_DIR/$LIBRARY_NAME.a"

# Paths configuration
XCODE_PATH="${XCODE_PATH:-/Applications/Xcode.app}"
DEVELOPER_DIR="$XCODE_PATH/Contents/Developer"
SDK_PATH="$DEVELOPER_DIR/Platforms/iPhoneOS.platform/Developer/SDKs/iPhoneOS.sdk"
CLANG="$DEVELOPER_DIR/Toolchains/XcodeDefault.xctoolchain/usr/bin/clang"

mkdir -p "$ROOT/$OUTPUT_DIR"

# Verify paths
if [ ! -d "$SDK_PATH" ]; then
    echo "SDK not found: $SDK_PATH"
    exit 1
fi

if [ ! -f "$CLANG" ]; then
    echo "Compiler not found: $CLANG"
    exit 1
fi

# Build environment
export GOARCH=arm64
export GOOS=ios
export CGO_ENABLED=1
export SDK_PATH="$SDK_PATH"
export CC="$CLANG -isysroot $SDK_PATH -arch arm64 -miphoneos-version-min=13.0"
export CXX="${CLANG}++ -isysroot $SDK_PATH -arch arm64 -miphoneos-version-min=13.0"
export CGO_CFLAGS="-isysroot $SDK_PATH -arch arm64 -miphoneos-version-min=13.0"
export CGO_LDFLAGS="-isysroot $SDK_PATH -arch arm64 -miphoneos-version-min=13.0"

echo "Building for iOS (arm64)..."

# Build static library
# The iOS C API lives in mobile/ios, on package mobile: the same Session,
# context rule, codec and link handling as the Android library. An app
# that links this checkout as a submodule gets exactly this core.
if (cd "$ROOT/mobile" && go build \
    -buildmode=c-archive \
    -tags ios \
    -ldflags="-w" \
    -trimpath \
    -o "$OUT" \
    ./ios) ; then
    
    # Header liboflux.h is generated automatically by cgo from //export directives.
    
    echo "Build complete: $OUT"
    ls -lh "$OUT"
    
else
    echo "Build failed"
    exit 1
fi
