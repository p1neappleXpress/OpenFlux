# Android Yandex session import

The existing Android tunnel reaches `showcaptchafast` because its native `vyandex` client has no access to the Yandex browser session used by the working Windows client and VPS. The Android app must accept a Netscape `cookies.txt` export and pass it to a rebuilt OpenFlux native client.

The app will use Android's document picker. It copies the selected file into app-private `noBackupFilesDir`, with no cookie values in QR codes, logs, preferences, or the APK. When starting a `vyandex` tunnel without a saved file, the app asks for one. A tunnel's context menu can replace the saved file after expiry. Selection cancellation leaves the tunnel stopped; an invalid file is rejected by the existing Go parser without exposing values.

The native command builder owns `--yandex-cookies-file` and adds its private path only for `vyandex`. Rebuild the arm64 binary from the current OpenFlux source with cookie support and package it in an Android 1.1-based debug APK, keeping the existing AES key and QR configuration. The 32-bit ARM and x86_64 builds require external Android linking and are outside this artifact. Verify Kotlin unit tests, APK build, packaged binary, QR decode, and the Windows/VPS tunnel; a live phone test remains necessary to confirm Android's network accepts the session.
