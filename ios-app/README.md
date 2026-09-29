# OpenFlux iOS app

SwiftUI client that links the OpenFlux Go core (`liboflux.a`) and runs the
SOCKS5 tunnel over the Yandex.Docs transport on `127.0.0.1:1080`.

## Layout
- `project.yml` — XcodeGen project definition (run `xcodegen generate` to produce `OpenFlux.xcodeproj`).
- `OpenFlux/` — Swift sources, bridging header, Info.plist, assets.
- `Lib/liboflux.a`, `Lib/liboflux.h` — Go static library + generated header (copied from `../output/ios`).
- `ExportOptions.plist` — App Store export options (team 8GQH8GQ252, automatic signing).

## Go bridge API (liboflux.h)
Built from `mobile/ios` (`./build_ios.sh`) on package `mobile`, the same code
the Android app runs: Session, KDF context, codec fallback and openflux://
links behave the same on every client. An app outside this repository links
this core as a submodule and builds the library with the same script.

Classic profiles (one carrier, as the app has always started them):
- `OpenFluxSetEncryption(secret)`, `OpenFluxSetCodec(codec)` — for the next start.
  With a secret the client runs the Session and speaks classic to a node that
  does not answer it (an older or classic node); without one, classic only.
- `OpenFluxStartClient(transportType, url, socksAddr, maxToken, maxUid)` — SOCKS5 client
  (a comma-separated `url` runs one carrier per document).
- `OpenFluxStartPacketTunnel(transportType, url, maxToken, maxUid)` — Network Extension
  (`OpenFluxTunWritePacket` / `OpenFluxTunReadPacket`, `OpenFluxPacketTunnelConnected`,
  `OpenFluxStopPacketTunnel`, `OpenFluxSetTunnelUDP`, `OpenFluxSetGeositeDirect`,
  `OpenFluxDrainDirectIPs`).

Session profiles (several carriers, what a node's openflux:// link describes):
- `OpenFluxShareDecode(link)` → `{"config","context","session"}`: `session` is the
  profile, ready for `OpenFluxStartSession(session, secret, socksAddr)` or
  `OpenFluxStartSessionPacketTunnel(session, secret)`. Do not interpret the link in
  Swift: the context and carrier names in it must reach the core unchanged.
- `OpenFluxShareEncode(configJSON)` → `{"link","config","context"}`: the core fills in
  the context and drops defaults, so the link is the one every client makes.
- On failure both return `{"error","code","param"}`: `code` (`share.Code*`, e.g.
  `damaged`, `unknown_transport`) is what the app words for the user, `param` the value
  it is about; `error` is English detail for the log.

State and checks:
- `OpenFluxStop()`, `OpenFluxIsRunning()`, `OpenFluxIsConnected()`, `OpenFluxStatsJSON()`,
  `OpenFluxMode()` (`session` / `classic`), `OpenFluxActiveTransport()`.
- `OpenFluxCaptchaPending()` (this phone's carrier), `OpenFluxRemoteCaptchaPending()` and
  `OpenFluxRemoteCaptchaProxy()` (the exit's), `OpenFluxCaptchaReason()`,
  `OpenFluxApplyCaptchaCookies(header)` / `OpenFluxOfferCaptchaCookies(header)`,
  `OpenFluxCancelCaptcha()`, `OpenFluxSetInitialCookies(header)`, `OpenFluxSetCookieStore(path)`.
- `OpenFluxReadLog()`, `OpenFluxSetDebug(on)`, `OpenFluxSetDebugLevel(0..3)`,
  `OpenFluxSetDoTResolver(spec)`; free returned strings with `OpenFluxFreeString`.

## Preparing encryption keys outside the VPN extension

For memory-constrained Network Extensions, opt in to prepared encryption:

1. In the containing app, call `OpenFluxPreparePacketTunnelKeys(type, url, secret)`
   for a classic profile, or `OpenFluxPrepareSessionPacketTunnelKeys(session, secret)`.
   Run it off the UI thread: it performs the existing scrypt KDF for the primary
   and all alternate contexts. The response is `{"keys":"..."}` or `{"error":"..."}`;
   free it with `OpenFluxFreeString`.
2. Store the **private** `keys` string in the shared Keychain, separated by profile
   and by document/direct key slot. Use after-first-unlock, this-device-only access
   for reconnects; never put this data in logs, UserDefaults, or providerConfiguration.
3. In the extension, read the current secret and bundle, call
   `OpenFluxSetEncryption(secret)`, then `OpenFluxSetPreparedEncryption(keys)`,
   then the existing `OpenFluxStartPacketTunnel` / `OpenFluxStartSessionPacketTunnel`.
   Order matters: setting the secret clears the prepared setting.
4. Recompute in the app after a key/profile change. Missing, corrupt or stale
   prepared material fails closed and asks for preparation from the app; it never
   falls back to scrypt. Old app versions that do not call the new setter retain
   their existing behavior. Apply the app and core changes together to gain the fix.

The private versioned bundle contains scrypt master keys and a MAC binding it to
its secret. Its context list must exactly match the core's current profile rules.
It is equivalent to decryption credentials and is not a public password verifier.
AES-256-GCM, scrypt parameters, wire frames, Session/classic interoperability and
context fallback remain unchanged. New contexts not present in the bundle are
rejected; reconnect from the app to prepare the updated profile.

## Build + archive + export (one command)
From the repo root:
```bash
./build_ios_app.sh
```
Produces `ios-app/build/export/OpenFlux.ipa`, distribution-signed for the App Store.

## Upload to TestFlight
1. Create the app record once: App Store Connect > My Apps > **+** > New App,
   bundle id `com.p1neapplexpress-saharev.openflux`, platform iOS.
2. Upload the IPA (either option):
   ```bash
   # A) app-specific password (appleid.apple.com)
   xcrun altool --upload-app -f ios-app/build/export/OpenFlux.ipa -t ios \
     -u YOUR_APPLE_ID -p xxxx-xxxx-xxxx-xxxx

   # B) App Store Connect API key (.p8 in ~/.appstoreconnect/private_keys/)
   xcrun altool --upload-app -f ios-app/build/export/OpenFlux.ipa -t ios \
     --apiKey KEY_ID --apiIssuer ISSUER_ID
   ```
   Or open `ios-app/build/OpenFlux.xcarchive` in Xcode Organizer and use **Distribute App**.
3. The build appears in TestFlight after Apple processing (a few minutes).

## Notes / follow-ups
- The local SOCKS5 mode supports in-app checks. Whole-device routing uses the
  included `OpenFluxTunnel` (`NEPacketTunnelProvider`) target and requires a
  provisioning profile authorizing Network Extensions. For encrypted packet
  startup with a smaller memory footprint, integrate the prepared-key flow above
  in the containing app and extension together.
- Deployment target: iOS 15.0 (SwiftUI App lifecycle). The Go lib is built with
  `-miphoneos-version-min=13.0`, so it is compatible.
