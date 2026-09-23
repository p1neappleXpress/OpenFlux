# Android Yandex session import

The existing Android tunnel reaches `showcaptchafast` because its native `vyandex` client has no access to the Yandex browser session used by the working Windows client and VPS. The Android app must accept a Netscape `cookies.txt` export and pass it to a rebuilt OpenFlux native client.

The app will use Android's document picker. It copies the selected file into app-private `noBackupFilesDir`, with no cookie values in QR codes, logs, preferences, or the APK. When starting a `vyandex` tunnel without a saved file, the app asks for one. A tunnel's context menu can replace the saved file after expiry. Selection cancellation leaves the tunnel stopped; an invalid file is rejected by the existing Go parser without exposing values.

The native command builder owns `--yandex-cookies-file` and adds its private path only for `vyandex`. Rebuild arm64, armv7, and x86_64 binaries from the current OpenFlux source with cookie support, package them in an Android 1.1-based debug APK, and keep the existing AES key and QR configuration. Verify Kotlin unit tests, APK build, packaged binaries, QR decode, and the Windows/VPS tunnel; a live phone test remains necessary to confirm Android's network accepts the session.
