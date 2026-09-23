# Android Yandex Session Import Implementation Plan

**Goal:** Make the Android `vyandex` client use the same user-provided Yandex cookies as the working desktop and VPS tunnel.

**Architecture:** The Android document picker copies a Netscape cookie export into app-private storage. The native process supervisor passes its path to a freshly built OpenFlux Android binary for `vyandex` only.

**Tech Stack:** Kotlin Android app, Android Storage Access Framework, Go 1.26 cross-build, Gradle.

---

1. Add a failing `NativeArgsTest` for app-owned cookie-file argument handling, run it, implement the argument, then rerun it.
2. Add a cookie-file store with bounded, atomic copy into `noBackupFilesDir` and a unit test for validation; run red then green.
3. Wire the document picker into tunnel start and add a context-menu action to replace the saved session. Keep cancelled selections from starting VPN.
4. Cross-build current OpenFlux for Android arm64 with `-checklinkname=0`, replace the bundled arm64 client binary, limit the debug APK to arm64, and build it. The other Android architectures require NDK external linking and are not included.
5. Run Android unit tests and APK build, inspect APK content, decode the QR locally, and verify the desktop/VPS tunnel remains operational. Give the user APK, cookie file, and exact import steps; state that phone behavior is still unverified.
