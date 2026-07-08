#!/bin/bash
set -e

OUTPUT_DIR="output/ios"
LIBRARY_NAME="liboflux"

# Paths configuration
XCODE_PATH="/path/to/Xcode.app"
DEVELOPER_DIR="$XCODE_PATH/Contents/Developer"
SDK_PATH="$DEVELOPER_DIR/Platforms/iPhoneOS.platform/Developer/SDKs/iPhoneOS.sdk"
CLANG="$DEVELOPER_DIR/Toolchains/XcodeDefault.xctoolchain/usr/bin/clang"

mkdir -p "$OUTPUT_DIR"

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
if go build \
    -buildmode=c-archive \
    -ldflags="-s -w" \
    -trimpath \
    -o "$OUTPUT_DIR/$LIBRARY_NAME.a" \
    . ; then
    
    # Create header if not auto-generated
    if [ ! -f "$OUTPUT_DIR/$LIBRARY_NAME.h" ]; then
        cat > "$OUTPUT_DIR/$LIBRARY_NAME.h" << 'HEADEREOF'
#ifndef LIBTUNNEL_H
#define LIBTUNNEL_H

#ifdef __cplusplus
extern "C" {
#endif

void RunMain(void);
void RunMainClient(char* url);
void RunMainExitNode(void);

#ifdef __cplusplus
}
#endif

#endif /* LIBTUNNEL_H */
HEADEREOF
    fi
    
    echo "Build complete: $OUTPUT_DIR/$LIBRARY_NAME.a"
    ls -lh "$OUTPUT_DIR/$LIBRARY_NAME.a"
    
else
    echo "Build failed"
    exit 1
fi
