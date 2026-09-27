#!/bin/bash
set -e

ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-$HOME/Android/Sdk/ndk/27.0.12077973}"
OUTPUT_DIR="output/android/arm64-v8a"
BINARY_NAME="openflux"

mkdir -p "$OUTPUT_DIR"

# The NDK's prebuilt toolchain directory is named after the build host.
case "$(uname -s)" in
    Darwin) HOST_TAG=darwin-x86_64 ;;
    Linux) HOST_TAG=linux-x86_64 ;;
    MINGW* | MSYS* | CYGWIN*) HOST_TAG=windows-x86_64 ;;
    *) echo "unsupported build host $(uname -s); set NDK_HOST_TAG" >&2; exit 1 ;;
esac
HOST_TAG="${NDK_HOST_TAG:-$HOST_TAG}"
# The binary runs on Android 8.0+ (API 26). Targeting a newer API links
# symbols such as android_get_device_api_level that older Androids lack.
ANDROID_API="${ANDROID_API:-26}"
TOOLCHAIN="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/$HOST_TAG/bin"
if [ ! -d "$TOOLCHAIN" ]; then
    echo "no NDK toolchain at $TOOLCHAIN; set ANDROID_NDK_HOME" >&2
    exit 1
fi
CLANG_SUFFIX=""
if [ "$HOST_TAG" = windows-x86_64 ]; then
    # Go on Windows needs a Windows path to the .cmd wrappers, and Git Bash
    # must not rewrite the device paths below into Windows paths.
    CLANG_SUFFIX=".cmd"
    TOOLCHAIN="$(cygpath -m "$TOOLCHAIN")"
    export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*" MSYS2_ENV_CONV_EXCL="*"
fi

export GOARCH=arm64
export GOOS=android
export CGO_ENABLED=1
export CC="$TOOLCHAIN/aarch64-linux-android${ANDROID_API}-clang${CLANG_SUFFIX}"
export CXX="$TOOLCHAIN/aarch64-linux-android${ANDROID_API}-clang++${CLANG_SUFFIX}"
export CGO_CFLAGS="-march=armv8-a -O2"
export CGO_CXXFLAGS="-march=armv8-a -O2"
export CGO_LDFLAGS="-Wl,-rpath,/system/lib64 -Wl,-rpath,/vendor/lib64"

go build \
    -v \
    -ldflags="-s -w -linkmode external -extldflags '-Wl,-rpath,/system/lib64 -Wl,-rpath,/vendor/lib64' -checklinkname=0" \
    -o "$OUTPUT_DIR/$BINARY_NAME" \
    .

if [ -f "$OUTPUT_DIR/$BINARY_NAME" ]; then
    echo "Build successful: $OUTPUT_DIR/$BINARY_NAME"
    file "$OUTPUT_DIR/$BINARY_NAME"
fi
