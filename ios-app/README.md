# OpenFlux iOS app

SwiftUI client that links the OpenFlux Go core (`liboflux.a`) and runs the
SOCKS5 tunnel over the Yandex.Docs transport on `127.0.0.1:1080`.

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
- The app runs a **local** SOCKS5 proxy. The in-app **Test** button proves the
  tunnel carries traffic (fetches the exit IP through the proxy). Routing the
  whole device requires a Network Extension (`NEPacketTunnelProvider`) target
  with the Network Extensions capability — not included in this first build.
- Deployment target: iOS 15.0 (SwiftUI App lifecycle). The Go lib is built with
  `-miphoneos-version-min=13.0`, so it is compatible.
