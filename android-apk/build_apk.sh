#!/usr/bin/env bash
# Build a locally signed OpenFlux APK on Linux x86_64, using Android SDK tools.
set -Eeuo pipefail
trap 'printf "\nОшибка на строке %s. Сборка остановлена.\n" "$LINENO" >&2' ERR

if [[ "${1:-}" == "--help" ]]; then
    cat <<'HELP'
Использование: ANDROID_HOME=/root/Android/Sdk bash build_apk.sh

Результат: dist/OpenFlux-local-arm64.apk
Android: ARM64, Android 8.0+ (minSdk 26, targetSdk 35).
Приложение: VPN всего устройства, IPv4/TCP и DNS через OpenFlux.
Остальной UDP и IPv6 блокируются (ограничение существующего ядра).

Необязательные переменные:
  ANDROID_NDK_HOME  Путь к Linux NDK r27 или новее.
  OPENFLUX_SRC      Использовать свой каталог исходников OpenFlux.
  OPENFLUX_GO       Путь к исполняемому файлу Go 1.26.4.

Без OPENFLUX_SRC скачивается закреплённый коммит OpenFlux.
Недостающие пакеты SDK устанавливаются через существующий sdkmanager.
Если Go 1.26.4 не найден, он загружается с go.dev в каталог проекта.
Ключ подписи сохраняется в signing/openflux-local.jks; сохраните его для обновлений.
HELP
    exit 0
fi

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || {
    echo 'Требуется Linux / Ubuntu / WSL на x86_64.' >&2
    exit 1
}
if [[ -x /usr/lib/jvm/java-17-openjdk-amd64/bin/java ]]; then
    export JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64
    export PATH="$JAVA_HOME/bin:$PATH"
fi
for command_name in git curl python3 unzip java keytool; do
    command -v "$command_name" >/dev/null || {
        echo "Не найдена команда $command_name. Установите зависимости из README.md." >&2
        exit 1
    }
done

export ANDROID_HOME="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/Android/Sdk}}"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-$ANDROID_HOME/ndk/27.0.12077973}"
ANDROID_JAR="$ANDROID_HOME/platforms/android-35/android.jar"
BUILD_TOOLS="$ANDROID_HOME/build-tools/35.0.0"
CLANG="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android26-clang"
WORK="$ROOT/.work"
mkdir -p "$WORK" "$ROOT/dist"

# Install only missing SDK components. Never replace the user's SDK directory.
missing_packages=()
[[ -f "$ANDROID_JAR" ]] || missing_packages+=("platforms;android-35")
[[ -x "$BUILD_TOOLS/aapt2" && -x "$BUILD_TOOLS/d8" && -x "$BUILD_TOOLS/apksigner" && -x "$BUILD_TOOLS/zipalign" ]] \
    || missing_packages+=("build-tools;35.0.0")
if [[ ! -x "$CLANG" ]]; then
    if [[ "$ANDROID_NDK_HOME" != "$ANDROID_HOME/ndk/27.0.12077973" ]]; then
        echo "В указанном ANDROID_NDK_HOME не найден Linux-компилятор: $CLANG" >&2
        exit 1
    fi
    missing_packages+=("ndk;27.0.12077973")
fi
if (( ${#missing_packages[@]} )); then
    SDKMANAGER="$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager"
    if [[ ! -f "$SDKMANAGER" ]]; then
        SDKMANAGER="$(find "$ANDROID_HOME/cmdline-tools" -type f -path '*/bin/sdkmanager' | sort -V | tail -n 1)"
    fi
    [[ -n "$SDKMANAGER" && -f "$SDKMANAGER" ]] || {
        echo 'Не найден sdkmanager. Нужны Android SDK Command-line Tools.' >&2
        exit 1
    }
    echo 'SDK: при запросе ознакомьтесь с лицензией и введите y для принятия.'
    bash "$SDKMANAGER" --sdk_root="$ANDROID_HOME" --licenses
    bash "$SDKMANAGER" --sdk_root="$ANDROID_HOME" --install "${missing_packages[@]}"
fi
"$CLANG" --version

# Use a pinned Go toolchain to avoid incompatibilities with gVisor internals.
unset GOROOT GOFLAGS GOOS GOARCH GOARM GOARM64 GOAMD64 CC CXX
unset CGO_ENABLED CGO_CFLAGS CGO_CXXFLAGS CGO_LDFLAGS
export GOTOOLCHAIN=local
GO_BIN="${OPENFLUX_GO:-}"
if [[ -z "$GO_BIN" && -x /opt/openflux-go-1.26.4/bin/go ]]; then
    GO_BIN=/opt/openflux-go-1.26.4/bin/go
fi
if [[ -z "$GO_BIN" ]]; then
    GO_BIN="$ROOT/.tools/go1.26.4/bin/go"
    if [[ ! -x "$GO_BIN" ]]; then
        archive="$WORK/go1.26.4.linux-amd64.tar.gz"
        curl -fL --retry 3 https://go.dev/dl/go1.26.4.linux-amd64.tar.gz -o "$archive"
        printf '%s  %s\n' \
            '1153d3d50e0ac764b447adfe05c2bcf08e889d42a02e0fe0259bd47f6733ad7f' \
            "$archive" | sha256sum -c -
        mkdir -p "$ROOT/.tools/go1.26.4"
        tar -xzf "$archive" -C "$ROOT/.tools/go1.26.4" --strip-components=1
    fi
fi
[[ "$("$GO_BIN" version)" == 'go version go1.26.4 '* ]] || {
    echo 'Для этой сборки используйте Go 1.26.4; OPENFLUX_GO должен указывать на его bin/go.' >&2
    exit 1
}
"$GO_BIN" version

REF=a3be43bb514385c18f55f98e92dd7c02109d2118
if [[ -n "${OPENFLUX_SRC:-}" ]]; then
    SOURCE="$(cd -- "$OPENFLUX_SRC" && pwd)"
else
    SOURCE="$WORK/OpenFlux"
    if [[ ! -d "$SOURCE/.git" ]]; then
        git init -q "$SOURCE"
        git -C "$SOURCE" remote add origin https://github.com/p1neappleXpress/OpenFlux.git
    fi
    if ! git -C "$SOURCE" cat-file -e "$REF^{commit}" 2>/dev/null; then
        git -C "$SOURCE" fetch --depth 1 origin "$REF"
    fi
    git -C "$SOURCE" checkout --detach "$REF"
fi
[[ -f "$SOURCE/go.mod" && -f "$SOURCE/main.go" ]] || {
    echo "Не найдены исходники OpenFlux: $SOURCE" >&2
    exit 1
}
SOURCE_REV="$(git -C "$SOURCE" rev-parse HEAD 2>/dev/null || printf 'unknown')"

BUILD="$(mktemp -d "$WORK/build.XXXXXX")"
mkdir -p "$BUILD/lib/arm64-v8a" "$BUILD/generated" "$BUILD/classes" "$BUILD/dex"
# Patch only an isolated build copy. Never edit the user's OPENFLUX_SRC.
python3 - "$SOURCE" "$BUILD/core" "$ROOT/core-overlay" <<'PY'
import pathlib, shutil, sys
source, dest, overlay = map(pathlib.Path, sys.argv[1:])
shutil.copytree(source, dest, ignore=shutil.ignore_patterns('.git'))
import subprocess
subprocess.check_call([sys.executable, str(overlay/'apply.py'), str(dest)])
PY
echo 'Сборка Go-клиента для Android ARM64…'
(
    cd "$BUILD/core"
    "$GO_BIN" mod download
    GOOS=android GOARCH=arm64 CGO_ENABLED=1 \
        CC="$CLANG" CXX="${CLANG}++" \
        CGO_CFLAGS='-march=armv8-a -O2' \
        CGO_CXXFLAGS='-march=armv8-a -O2' \
        "$GO_BIN" build -v -trimpath -buildvcs=false -buildmode=pie \
        -ldflags='-s -w -checklinkname=0 -linkmode=external -extldflags=-Wl,-z,max-page-size=16384' \
        -o "$BUILD/lib/arm64-v8a/libopenflux.so" .
)

echo 'Сборка TUN → SOCKS5 / DNS TCP (JNI)…'
(
    cd "$ROOT/native"
    "$GO_BIN" mod download
    GOOS=android GOARCH=arm64 CGO_ENABLED=1 \
        CC="$CLANG" CXX="${CLANG}++" \
        CGO_CFLAGS='-march=armv8-a -O2' \
        CGO_CXXFLAGS='-march=armv8-a -O2' \
        "$GO_BIN" build -v -trimpath -buildvcs=false -buildmode=c-shared \
        -ldflags='-s -w -checklinkname=0 -linkmode=external -extldflags=-Wl,-z,max-page-size=16384' \
        -o "$BUILD/lib/arm64-v8a/libopenfluxtun.so" ./cmd/jnibridge
)

# An installed native-library path is executable on modern Android.
# The client is a PIE executable packaged as libopenflux.so, not a JNI library.
python3 - "$BUILD/lib/arm64-v8a/libopenflux.so" "$BUILD/lib/arm64-v8a/libopenfluxtun.so" <<'PY'
import struct, sys
for path in sys.argv[1:]:
    with open(path, 'rb') as f:
        header = f.read(64)
        assert header[:6] == b'\x7fELF\x02\x01', 'Expected little-endian ELF64'
        assert struct.unpack_from('<H', header, 16)[0] == 3, 'Expected ET_DYN'
        assert struct.unpack_from('<H', header, 18)[0] == 183, 'Expected ARM64'
        phoff = struct.unpack_from('<Q', header, 32)[0]
        phsize, phnum = struct.unpack_from('<HH', header, 54)
        for i in range(phnum):
            f.seek(phoff+i*phsize)
            entry = f.read(phsize)
            if struct.unpack_from('<I', entry)[0] == 1:
                assert struct.unpack_from('<Q', entry, 48)[0] >= 16384, 'Expected 16 KiB LOAD alignment'
PY

echo 'Компиляция ресурсов и Android-оболочки…'
"$BUILD_TOOLS/aapt2" compile --dir "$ROOT/res" -o "$BUILD/resources.zip"
"$BUILD_TOOLS/aapt2" link -I "$ANDROID_JAR" --manifest "$ROOT/AndroidManifest.xml" \
    --java "$BUILD/generated" -o "$BUILD/unsigned.apk" "$BUILD/resources.zip"

if command -v javac >/dev/null; then
    JAVAC=(javac)
else
    # Some JRE distributions retain the compiler module but omit the javac launcher.
    JAVAC=(java -m jdk.compiler/com.sun.tools.javac.Main)
fi
mapfile -d '' JAVA_SOURCES < <(find "$ROOT/src" "$BUILD/generated" -type f -name '*.java' -print0)
"${JAVAC[@]}" -encoding UTF-8 --release 8 -classpath "$ANDROID_JAR" \
    -d "$BUILD/classes" "${JAVA_SOURCES[@]}"
python3 - "$BUILD" <<'PY'
import pathlib, sys, zipfile
root = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(root/'classes.jar', 'w', zipfile.ZIP_DEFLATED) as z:
    for p in sorted((root/'classes').rglob('*.class')):
        z.write(p, p.relative_to(root/'classes').as_posix())
PY
"$BUILD_TOOLS/d8" --min-api 26 --lib "$ANDROID_JAR" \
    --output "$BUILD/dex" "$BUILD/classes.jar"
python3 - "$BUILD" "$ROOT/third-party" <<'PY'
import pathlib, sys, zipfile
root = pathlib.Path(sys.argv[1])
with zipfile.ZipFile(root/'unsigned.apk', 'a', zipfile.ZIP_DEFLATED) as z:
    for p in sorted((root/'dex').glob('*.dex')):
        z.write(p, p.name)
    for p in sorted((root/'lib/arm64-v8a').glob('*.so')):
        z.write(p, 'lib/arm64-v8a/'+p.name)
    notices = pathlib.Path(sys.argv[2])
    for p in sorted(notices.rglob('*')):
        if p.is_file():
            z.write(p, 'assets/third-party/'+p.relative_to(notices).as_posix())
PY

echo 'Подпись APK…'
mkdir -p "$ROOT/signing"
KEYSTORE="$ROOT/signing/openflux-local.jks"
if [[ ! -f "$KEYSTORE" ]]; then
    (umask 077
     keytool -genkeypair -keystore "$KEYSTORE" -storetype JKS \
         -alias openflux-local -storepass android -keypass android \
         -keyalg RSA -keysize 3072 -validity 10000 \
         -dname 'CN=OpenFlux Local Build,OU=Local Development,O=Local' -noprompt)
fi
"$BUILD_TOOLS/zipalign" -f -P 16 4 "$BUILD/unsigned.apk" "$BUILD/aligned.apk"
APK="$ROOT/dist/OpenFlux-local-arm64.apk"
"$BUILD_TOOLS/apksigner" sign --ks "$KEYSTORE" --ks-key-alias openflux-local \
    --ks-pass pass:android --key-pass pass:android \
    --v1-signing-enabled false --v2-signing-enabled true --v3-signing-enabled true \
    --v4-signing-enabled false --out "$BUILD/signed.apk" "$BUILD/aligned.apk"
"$BUILD_TOOLS/apksigner" verify --verbose "$BUILD/signed.apk"
"$BUILD_TOOLS/zipalign" -c -P 16 4 "$BUILD/signed.apk"
cp "$BUILD/signed.apk" "$APK"
"$BUILD_TOOLS/aapt2" dump badging "$APK" > "$ROOT/dist/apk-info.txt"
"$GO_BIN" version -m "$BUILD/lib/arm64-v8a/libopenflux.so" > "$ROOT/dist/core-build.txt"
"$GO_BIN" version -m "$BUILD/lib/arm64-v8a/libopenfluxtun.so" > "$ROOT/dist/bridge-build.txt"
printf 'pinned_upstream=%s\nsource_revision=%s\nclient_overlay=core-overlay/apply.py\n' "$REF" "$SOURCE_REV" > "$ROOT/dist/source-build.txt"
sha256sum "$APK" > "$ROOT/dist/OpenFlux-local-arm64.apk.sha256"
printf '\nГотово: %s\n' "$APK"
ls -lh "$APK"
