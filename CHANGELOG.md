# Changelog

All notable changes to the OpenFlux core. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- phpbox page (`deploy/phpbox`): opening an exit's URL in a browser shows a
  status page (OpenFlux look, light and dark) instead of silently running:
  live state, a debug log, Start/Stop, and a check that a node is already
  running on that target (a heartbeat plus a lock), so a second open or a
  pinger attaches to the first instead of starting another. Anything that is
  not a browser (a pinger, curl) runs the node exactly as before, and the
  old `?url=` / `?room=` addresses are unchanged. The page reads
  `openflux://` links and draws their QR in the browser with the core's own
  `share` package compiled to WebAssembly (`cmd/sharewasm`,
  `deploy/phpbox/build-wasm.sh`), so there is one link parser for every
  client; the secret in a link never leaves the browser.
- `deploy/phpbox/build-bundle.sh` builds the upload set from the sources
  (token in `config.php`, which also works where `putenv` is disabled).
- Link compatibility tests (`share/compat_test.go`): links written by
  earlier builds are frozen as literals and must keep reading, including how
  a link is mangled by chats and terminals, unknown JSON fields, and the
  stability of the error codes.

- phpbox self-renewing tunnel (`&chain=1`, what the page asks for): before a
  generation ends it starts the next one with a request to its own host; the
  new one joins, takes every new stream, and the old one drains the streams
  it has. A stream goes to exactly one generation (an atomic `mkdir` marker
  decides while both are up). `a=stop` ends the whole chain.
- `--mode=stream` logs like the packet modes: `-d` one line per mux frame
  (`[STREAM] -> 526 bytes - stream 7 DATA`), `-dd` operational logs (streams
  opening and closing, a busy carrier), `-ddd` hexdumps of DATA payloads.
  Until now the level was set after the stream branch had returned, so none
  of it showed.

- Stream mode is now a library (`streamproxy`: carrier -> mux -> SOCKS5 and an
  optional HTTP proxy, with counters) used by `--mode=stream` and, next, the
  mobile bridges; `--mode=stream` also takes `--http-proxy` and `--ipc-socket`.
- `openflux://` links and QR codes can name the stream mode (`share.Config.Mode`,
  `"stream"`): one carrier (cups.online or Mail.ru: the two the PHP exit has ports for), no session, no secret.
  Links without a mode are the classic tunnel as before; codes `unknown_mode`,
  `stream_transport`, `stream_one_transport`, `stream_plain_only`.
- phpbox flow control: windows per stream (256 KB) and over all streams (512 KB)
  with ACK frames, agreed in OPEN (`host:port\0fc` / OPEN_OK `fc`), so old clients
  and exits are unaffected. Without it a saturated carrier queue buried small
  frames: over Mail.ru, four parallel downloads starved new TLS handshakes and
  uploads for minutes. With it (same test): 60 of 60 handshakes complete while four
  8 MB downloads run, and a 4 MB upload takes 33 s instead of timing out.

- Own node without a server: `provision/phphost` puts the PHP exit on any web
  host over FTP and checks that it runs. `probe` finds the web folder (also
  a level or two down: `domains/<site>/public_html`, `www/<site>`) and
  whether it is writable, `deploy` uploads the bundle embedded in the core
  (`deploy/phpbox`, with the link parser as WebAssembly), keeps the token of an
  earlier install and checks file sizes, `check` asks the site (passing the
  iFastNet-style AES browser check in plain Go, so no browser is needed),
  `start` runs the node and waits for it, plus `stop`, `node`, `newRoom`, `link`,
  `remove`. Answers are codes (`ftp_login`, `ftp_no_webroot`, `site_antibot`,
  `php_missing`, ...), never text. One dispatcher, `phphost.Call`, serves the
  desktop wizard (`--node-wizard`, methods `php.*`, with progress lines), the
  Android bridge (`PhpCall`, `PhpProgress`, `PhpCancel`) and the iOS C API
  (`OpenFluxPhpCall`). The node answers `a=ping` for it.
- Android and iOS start the stream mode: `StartStreamProxy` /
  `OpenFluxStartStreamClient` (SOCKS5 with the usual auth and bypass list).
- The node's page shows the link and QR code apps scan for that node (made by
  the core as a stream-mode `openflux://` link).

- Stream mode as a full tunnel (`tunnel.StreamNet`): the device's IP packets
  (utun/Wintun, Android VpnService, an iOS packet tunnel) go into a local
  stack that opens one mux stream per TCP connection. DNS is answered on the
  device with fake addresses (198.18.0.0/16) and the name is opened at the exit,
  so nothing is resolved locally; TCP on ports 80/443 only, QUIC, other UDP
  and IPv6 are dropped and apps fall back to TCP. It is a `transport.Transport`,
  so every packet client runs on it unchanged: `--mode=stream --inbound=tun`,
  `mobile.StartStreamPacket`, `OpenFluxStartStreamPacketTunnel`.

### Fixed

- Mail.ru transport dropped data under load: the server batches several
  cursor entries into one message, and only the first was read; a message
  that merely mentioned a peer's keep-alive was dropped whole. Every entry
  is delivered now, in order (`cursorPayloads`).
- Stream client: `Mux.send` ignored the carrier's "write queue full" and
  dropped the frame, which corrupts the stream (a lost byte inside a TLS
  record fails the handshake). Sends wait with backoff, for up to 15 s, and
  report an error instead of losing data; `conn.Write` passes it on.
- phpbox WebSocket client: a frame arriving in pieces (a 22 KB message on a
  slow link) was cut short and desynchronised the stream; frames already in
  PHP's TLS buffer were not seen by `stream_select`; fragmented messages
  were not reassembled; a closed link was indistinguishable from a timeout,
  so a node stayed deaf after the server dropped it. Reading is buffered,
  fragments are reassembled, and the mux reconnects (with backoff).
- phpbox mux: when a run ends, the client is told (CLOSE) about the streams
  that end with it; streams idle for 300 s are closed (a lost CLOSE no
  longer leaks a socket); a destination with several addresses is retried on
  the next one when the first does not answer.

### Changed

- phpbox mux: destinations are dialed asynchronously (a slow one no longer
  stalls the others), writes to a full destination are queued instead of
  dropped, and the frame buffer is consumed by offset.

- The node wizard (`--node-wizard`, `mobile.Node*`) lets a new channel use
  any mix of a Yandex document, a Mail.ru public document and cups.online
  rooms besides direct (`provision.ChannelTransport`); the rooms are created
  by the app (`cupsonline.CreateRoomList`) so the node keeps them, and its
  link, across restarts. `provision.ShareLink` builds the link from the same
  priorities and encryption context `node-install.sh` writes to node.conf.
- `node-install.sh update` and the optional `openflux-node-update.timer`:
  the node moves itself to the newest `node-v*` release, verified against
  that release's `node-install.sh` and `SHA256SUMS`, and rolls back if a
  channel does not stay up. An app with an older pinned script no longer
  downgrades a server the updater has moved on.

## [0.2.0] - 2026-09-28

Every client now behaves alike: peers of different builds and modes find
each other instead of dropping every packet in silence. The node wizard
(desktop and Android) installs this core as `node-v1.1.0`.

### Added

- Classic compatibility inside the Session (`PROTOCOL_NEGOTIATION.md`):
  a classic setup with a key (`--transport=X`, the apps' classic profiles)
  runs a Session and speaks classic to an exit that does not answer the
  handshake, switching once it does; a classic-configured exit serves
  classic and Session clients. `--negotiate` stays strict; exits configured
  as a Session (wizard, `.conf`, `--transports`) serve Session clients only.
- Codec fallback: the classic codec decodes batch-v2 and legacy frames,
  sends what the peer sends, and the client tries the other framing when
  the peer is silent. `--codec` is a preference now, not a requirement.
- KDF context fallback: one rule for the context (`transport.KDFContexts`)
  and alternates for what other builds derive; a record that fails under
  the current keys is tried under them, the exit answers under the
  client's context, a silent client cycles through them.
- `mobile/ios`: the iOS C library (`build_ios.sh`) on package `mobile`,
  replacing the root `export_ios*.go`: the calls the iOS app makes, plus
  Session profiles (`OpenFluxShareDecode` returns one ready to start),
  mode and captcha calls. An app that links this core as a submodule gets
  the same Session, links and fallbacks as Android.
- `mobile`: `ShareSessionSpecs`, `SetInitialCookies`, `SetLowMemory`
  (Volga's new `SlimVolgaConfig` for the iOS extension), `ReadTimeout`,
  `ConnectionMode`.
- Links are read and made by the core only. `share.Read` / `share.Make`
  answer every entry point with the same JSON (`--parse-link`, new
  `--make-link`, `mobile.ReadShareLink` / `MakeShareLink`, the iOS
  `OpenFluxShareDecode` / `OpenFluxShareEncode`): the configuration and
  its context, the link, or an error `code` (and `param`) that the apps
  put in their own words. `share.Make` normalizes what apps spell
  differently (the default codec is left out, an encrypted link always
  names its context, by the one rule when not given), so one
  configuration gives one link on every client; the node wizard's link,
  desktop and Android, comes from one `share.NodeConfig`.
- Logs a user can act on without `-dd`: key or context mismatch, the peer
  running the other layering, codec and context fallbacks, a second client
  taking over the exit, carriers failing to start, documents dropping, and
  a diagnosis when the handshake does not complete.

### Fixed

- A classic cupsonline client given the rooms as `--url` derived another
  key than the exit that created them: nothing got through.
- boards sent engine.io pings from the client, which an EIO=4 server
  answers by closing the socket: the board dropped every 20 seconds.
- A Session client's cupsonline carrier was built as an exit: with no or
  dead rooms it created rooms of its own and waited in them.
- openflux:// links: base64 padding, the standard alphabet, whitespace and
  line breaks are accepted; secrets are counted in characters as Kotlin
  counts them, not bytes.
- Carrier names that differ between the two sides no longer break cookie
  exchange and exit checks (messages carry the document URL).
- yandex / mailru: a socket whose keepalive failed is closed so the
  reconnect runs; writes are bounded.
- `utils.Infof` reaches the apps' log screens.
- A Session stopped each carrier twice.

## [0.1.0] - 2026-09-27

First release from the current `main` line (encrypted-logging + the
maintainer's multi-transport work folded together) and the first cut by
`release.yml` instead of a manual build.

### Added

- `mobile/`: the Android/iOS gomobile bridge now lives in this repository
  (moved from `meepo161/openfluxfork`, full history and authorship
  preserved), so building the mobile clients needs only this checkout.
- `.github/workflows/release.yml`: a `v*` tag cross-compiles the CLI for
  Linux (amd64/arm/arm64), Windows (386/amd64/arm64) and macOS
  (amd64/arm64) and publishes it with `SHA256SUMS.txt`. Replaces the
  ad-hoc manually-built `0.0.x` releases.
- `deploy/node-install.sh` now downloads the exit-node core from this
  repository's own `node-v*` releases instead of `meepo161/openfluxfork`;
  `provision/pin.go`'s pinned script commit/hash points here too.

### Fixed

- `main.go`: bench-send/bench-sink never resolved to the exit-side session
  role under `--negotiate`, so a negotiated session between two bench
  processes hung retrying the handshake and derived encryption keys in the
  same direction on both sides instead of swapped.
- `main.go`: the startup banner still printed the project's pre-rename
  name (`=== Universal Bypass Tool ===`) instead of `=== OpenFlux ===`.
- `.github/workflows/node-release.yml`: the pinned Go version (1.26.8) had
  drifted from `go.mod` (1.26.4), so its "reproducible" build didn't
  actually reproduce the hashes `deploy/node-install.sh` expects.

### Credits

`androidApp`/`desktopApp`/`shared` for [OpenFluxAndroid](https://github.com/p1neappleXpress/OpenFluxAndroid)
and [OpenFluxDesktop](https://github.com/p1neappleXpress/OpenFluxDesktop) now
run the Compose Multiplatform app built by [@meepo161](https://github.com/meepo161)
in [OpenFluxClient](https://github.com/meepo161/OpenFluxClient), moved into
those repositories with his agreement.

## [0.0.1] - [0.0.5]

Manually built and published cross-platform CLI binaries, before this
CHANGELOG and the automated release workflow existed.
