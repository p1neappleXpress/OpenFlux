# OpenFlux iOS app

SwiftUI client that links the OpenFlux Go core (`liboflux.a`). System VPN is
the default mode; a local SOCKS5 proxy is available under Advanced settings.

## Using the app

1. Choose the transport and enter the document link or MAX credentials.
2. Use the same encryption key and codec as the exit node, then tap Connect.
3. For an exit node with UDP support, enable Forward UDP under Advanced settings.
   The setting is remembered, but defaults off for compatibility with old exits.
   DNS continues to use DNS-over-TLS. Only IPv4 is tunneled; this is not an IPv6
   leak-protection feature.
4. Diagnostics → Check internet access makes an HTTPS request to ipify's IPv4
   endpoint and displays the public IP. It is user-initiated, checks the selected
   connection mode, and is canceled when the connection state changes. Success
   does not prove UDP support or leak protection.

Advanced settings also contains the codec and local proxy mode (default port
10808). Proxy mode does not route other apps automatically. Its log appears in
Diagnostics; the app does not present this process-local log as a VPN-extension log.

## Layout
- `project.yml` — XcodeGen project definition (run `xcodegen generate` to produce `OpenFlux.xcodeproj`).
- `OpenFlux/` — Swift sources, bridging header, Info.plist, assets.
- `Lib/liboflux.a`, `Lib/liboflux.h` — Go static library + generated header (copied from `../output/ios`).
- `ExportOptions.plist` — App Store export options (team 8GQH8GQ252, automatic signing).

## Go bridge API (liboflux.h)
- `OpenFluxStartClient(transportType, url, socksAddr, maxToken, maxUid)` — start the client (returns 0 on success).
- `OpenFluxStop()` — stop transport + SOCKS5 listener.
- `OpenFluxIsRunning()` / `OpenFluxIsConnected()` — state.
- `OpenFluxStatsJSON()` / `OpenFluxReadLog()` — stats + log tail (free with `OpenFluxFreeString`).

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
- System VPN uses the included `NEPacketTunnelProvider` target and requires
  Network Extension and shared Keychain signing entitlements. Test actual
  routing, reconnects and UDP on a signed build on a physical iPhone.
- Deployment target: iOS 15.0 (SwiftUI App lifecycle). The Go lib is built with
  `-miphoneos-version-min=13.0`, so it is compatible.
