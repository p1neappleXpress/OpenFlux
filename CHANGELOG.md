# Changelog

All notable changes to the OpenFlux core. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.4.3] - 2026-10-11

Hotfix for 0.4.2: the Yandex and Mail.ru carriers, both reported by node operators.

### Fixed

- **Yandex: the carrier relays tunnel packets again.** The outgoing cursor message declared a fixed
  length of `18` before the base64 payload, but Yandex Disk does not relay a cursor whose declared
  length does not match the packet behind it, so every tunnel packet was silently dropped and the
  Session handshake timed out; a working Direct fallback could hide the failure. The prefix is now the
  real binary packet length (`len(pending)`) on every send. Found with a decisive A/B wire experiment
  (length `18` → 0/3 delivered, correct length → 3/3) by
  [@Gamazzz](https://github.com/p1neappleXpress/OpenFlux/issues/165) — thank you.
  [#165](https://github.com/p1neappleXpress/OpenFlux/issues/165)
- **Mail.ru Docs: the carrier comes up on public links again.** Mail.ru's nginx answered `409 Conflict`
  to the WebSocket upgrade, so the transport looped on reconnects and never connected. Two causes:
  `gorilla/websocket` only sends `Sec-WebSocket-Extensions: permessage-deflate` when compression is
  enabled, and the old Chrome/137 `User-Agent` is rejected on some edge nodes. Compression is now
  enabled and a current Firefox `User-Agent` is used for both the API and the WebSocket. Measured with
  the real service: before, 8× `409` in 75 s and 0 connections; after, the WebSocket connects, Engine.IO
  and Socket.IO come up, auth succeeds, and a packet passes between two peers. Reported by
  [@pirsasha](https://github.com/p1neappleXpress/OpenFlux/issues/163) ([#163](https://github.com/p1neappleXpress/OpenFlux/issues/163))
  and [@Shiller70](https://github.com/p1neappleXpress/OpenFlux/issues/164) ([#164](https://github.com/p1neappleXpress/OpenFlux/issues/164)) — thank you.

Exit nodes that carry Yandex or Mail.ru need `node-v1.2.3`.

## [0.4.2] - 2026-10-07

Hotfix for 0.4.1: Mail.ru Docs.

### Fixed

- **Mail.ru Docs: a link that only lets the document be read no longer drops the connection every few
  seconds.** The transport sends the editor's `saveChanges` now and then; for a link without edit rights
  (`"edit": false`, what most public links are) the server answered with a close without a close frame,
  every 0.5 to 5 seconds, so the channel was up for moments between reconnects. `saveChanges` is not sent
  where the document's permissions forbid editing. Found with the real service: before, 4 drops in 15
  seconds; now none in 150 seconds, and a packet passes between two peers on the same document.
- **Mail.ru Docs: the Engine.IO / Socket.IO handshake is completed before the editor auth is sent**
  (the server's `0{...}` open, our `40{token}`, its `40{sid}`, then the auth; an Engine.IO ping `2` during it
  is answered). [#147](https://github.com/p1neappleXpress/OpenFlux/pull/147) by Novarg93, rebased onto 0.4.1.

Exit nodes that carry Mail.ru need `node-v1.2.2`.

## [0.4.1] - 2026-10-07

Hotfix for 0.4.0, found testing the release between a Windows client and a Linux L3 exit.

### Fixed

- **Yandex: the check no longer repeats forever for a node with an IPv6 route.** The tunnel is
  IPv4 (L3 raw sockets, the client's gVisor stack, L4 re-dials), so a Yandex check the node asks
  the client to pass is shown to the client's browser from the node's IPv4 address, and the
  `spravka` Yandex issues is bound to that address. The node's own requests (document fetch,
  captcha PoW, WebSocket, Volga relay, boards) went out dual-stack, over IPv6 first wherever the
  host has an AAAA route, and Yandex rejected the browser's `spravka` for the node's IPv6 address:
  SmartCaptcha again after every cookie handoff. All Yandex carrier connections now dial
  IPv4 first and fall back to IPv6 only when no IPv4 address answers. Measured with the fix
  through a Linux L3 exit: the node's document opens (200), data runs over Yandex at ~2 MB/s.
- **L3 exit (Linux): `-d` no longer logs the host's own traffic.** The raw sockets hand over a copy
  of everything the host receives (its ssh session, DNS answers, the carriers' connections,
  internet scans) and each packet was logged before the flow lookup; over ssh the log fed on
  itself (8 500 lines in 3 minutes while idle). Only packets of tunnel flows are logged now;
  the ignored traffic stays visible as `noct=` in `[L3-STATS]`. Nothing but tunnel flows ever
  reached the transport.

## [0.4.0] - 2026-10-07

### Added

- **Script (JS) transports**, experimental: a carrier can be a signed JS package (`.flux`, or a
  bare `.js` with a detached signature) run by an in-process engine (goja), next to the native
  ones: `Type = script` in a `.conf`, `--transports`, Session only (the carrier sees ciphertext).
  A package carries its `id`, `wire` and `api` generation, an update address, and a typed `params`
  schema; the core is the one verifier (`--inspect-script`, `mobile.InspectTransport`, one trust
  path for the CLI, gomobile and the desktop app) and pins the author's key. Updates are the core's
  decision too: check, apply and roll back (`--check-script-update`, `--apply-script-update`,
  `--rollback-script`, `mobile.CheckScriptUpdate/ApplyScriptUpdate/RollbackScript`,
  `mobile.CompareScriptVersions`); a different wire generation or another key never installs
  silently. `scriptsign pack` lints and `scriptsign index` writes an author's `update.json`.
  OpenFlux's own key is a **list** (`officialKeys`): a rotation is two releases. The seven bundled
  transports (1.1.0) are signed with it. The engine is off in the apps unless the user turns on
  "Экспериментальные функции". Docs: `docs/scripted-transports.md`.
- Script transports: **settings the script declares**. `info().params` grows
  the fields a form needs (`type` number/boolean/select/textarea, `default`,
  `description`, `placeholder`, `options`, `min`, `max`, `pattern`, `group`,
  `advanced`, `scope`); an old script means what it always did. The apps open a
  wizard page generated from the declaration (`--script-settings`,
  `mobile.ScriptSettings`; a script may bring its own with
  `Transport.settings(values)`), keep what it submits with the script, and the
  script gets it as `cfg.params` (declared defaults filled in; the `.conf` carries
  it as one `Params = <base64url JSON>` line). `--inspect-script` reports each
  param's resolved scope, `settingsPage` and `paramProblems`.
- The wizard page (and `cfg.params`) now cover **every** declared param, the
  profile one included: a script with only one param still gets a wizard, and
  an app can let the profile editor's field and the wizard edit the same saved
  value (`cfg.url` falls back to `cfg.params[profileKey]` if a caller sends
  only the latter).
- Script transports: **setup pages**. `raise("needsSetup" | "captchaRequired")`
  is checked in the core and throws a `TypeError` into the script for anything
  but an `https` site, an inline page, or the loopback address of the script's
  own `httpserver.listen()`; the IPC request and the mobile bridge say which
  pages are the script's own (`Own`, `PendingCaptchaOwn`,
  `PendingCaptchaTransport`) instead of the apps guessing from the URL. A check
  an exit reports at a loopback address is no longer forwarded to the client.
  `SubmitCaptchaData` reads the payload with the core's rules
  (`script.FlattenSubmission`).
- `scripttest`: `-settings`, `-open`, `-submit`, `-lang`; a raised setup page is
  served on `127.0.0.1` with `window.openfluxSubmit` (`transport/script/devhost`).
  The templates and `docs/scripted-transports.md` describe it; the SDK has the
  author's guide.
- One registry for "type -> transport" (`transport/registry`) replaces four copies that had
  drifted apart (`main.go` twice, `transport_factory.go`, `mobile`).
- Script transports can now ask for a setup/login page the same way the
  native Yandex/mailru transports ask for a captcha: `raise("needsSetup"/
  "captchaRequired", {url|html, reason})` reaches `ErrorNotifier` with an
  `html` field alongside `url` (`transport/error_notifier.go`,
  `transport/manager`, `transport/ipc`, `transport/control`) for a page
  the script built itself, not just a real site.
- `httpserver.listen(handler, port?)` - a loopback-only HTTP server a
  script can run for its own setup mini-app (`transport/script/
  host_httpserver.go`); handler may be sync or async.
- `--inspect-script` CLI subcommand and `transport/script.InspectTrust` -
  one shared signature-verification path for the CLI, gomobile, and the
  desktop app (`JvmPlatformServices`), replacing duplicated logic.
- Nightly CLI prerelease channel (`.github/workflows/nightly.yml`),
  matching the Android/Desktop apps' existing nightly channels.
- The README is split into focused documents under `docs/`.

### Fixed

- A Yandex check (SmartCaptcha, login) on a profile without a Session (no key) now reaches the
  app over `--ipc-socket`, as it does with a Session. Before, that path never connected the
  carrier's report to the IPC bridge, so the transport only logged "external solver required"
  every 30 seconds and the app never opened its browser; the cookies the app offered back were
  already taken.
- JS transports brought in step with the native ones. The seven bundled transports are now 1.1.0 and follow the natives: the
  saveChanges "editor activity" stream (yandex, mailru); every cursor entry of a
  batched mailru message; boards' modify-objects wire (a JS boards client's
  packets were ignored by a current exit) without the client ping that dropped
  the socket every 20 s; a login wall reaches the app as `captchaRequired` with
  reason `login`; cupsonline sends one batch at a time (packets over ~1 KB from
  a JS side never reached a native peer), keeps a receive buffer per sender and
  can run as an exit that creates its rooms (`raise("roomList", {rooms})`,
  `ScriptTransport.RoomList/OnRoomList`); a oneme receiver no longer places calls
  after its call socket closes. Details and what still differs:
  `docs/plans/2026-10-04-native-js-parity.md`. Signed with the official key
  (`transport/script/js/build.sh <key>` rebuilds and signs; the signature test
  verifies every shipped signature).
- `ScriptTransport.Stop()` calls the script's `close()`, closes every socket
  the host opened for it, and interrupts JS that does not return (it used to
  leave sockets open, so a participant stayed in the document, and could hang).
- `ws.send` has a 20 s write deadline: a half-open socket no longer blocks the
  script's event loop.
- The core passes `params.exit` (its role) to every script transport.
- Manifest reading (`Inspect`) runs the script with a restricted host API and a
  3 s budget, and `.flux` archives are read with size limits.
- yandex and mailru make one reconnect at a time.
- oneme: a failed or error-answered start-call request (opcode 78) no longer crashes the
  process with a nil dereference or loops on `Dial error: malformed ws or wss URL`; the error
  (or a response without call params) is logged and the call is retried after a second
  (#145, thanks @choksi2212).
- A new node install downloads `node-install.sh` from the commit `node-v1.2.0` is tagged on
  (`provision.PinnedCommit`; the script and its SHA-256 are unchanged).
- The setup-page dev host (`scripttest`) no longer races between starting and closing its
  server (found by `go test -race`).

### Security

- Dependencies: Go 1.26.6, x/crypto 0.56, x/text 0.41 (govulncheck: 15 reachable findings down
  to 1), then pion/webrtc v4 (clean).
- Manifest reading no longer runs an unverified script with the full host API; `.flux` archives
  are read with size limits.
- Every GitHub Action is pinned to a commit SHA.

## [0.3.0] - 2026-10-01

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

- Yandex Boards moves packets as `modify-objects` text objects (the payload as
  base64 in the object's value) and deletes what it has received with
  `drop-objects`, instead of `notify-position`, which the board no longer
  relays; the handshake and the heartbeat are unchanged (`transport/yandex/boards.go`).
- Mail.ru Docs and Yandex Docs send the web client's `saveChanges` message at a
  random 0.5-5 s interval, built from the live session (the participant's own
  user id and, for Yandex, the document type and a changing cursor sequence),
  so the stream looks like a person editing (`transport/mailru/mailru.go`,
  `transport/yandex/yandex.go`).

- The hosting install no longer fails when the host cuts a transfer short.
  InfinityFree's Pure-FTPd aborted the 2 MB link parser part way through over a
  TLS data channel (`451 Transfer aborted`), which failed the whole install;
  the same files went up whole in plain FTP. Each file is now sent again (up to
  four tries, reconnecting between them); with TLS `auto`, two failures over a
  TLS data channel go on in plain FTP, as a host with no TLS would have, and the
  reported security becomes `none`; the link parser is optional, so a host that
  will not take it still gets a working node. `provision/phphost` also gained a
  `page` call (the node's control-panel address, `auto=0` so opening it only
  looks) and checks that a chosen token is 8–64 of `A–Z a–z 0–9 - _`.

- The mode-without-a-server node now survives any host's limits and keeps the
  tunnel up across generations, found on a local emulation of a free host
  (Apache + PHP-FPM, a 60 s CPU cap, the host's disabled functions, a hidden
  wall-clock kill):
  - a disabled `set_time_limit` / `ignore_user_abort` / `getenv` / `getmypid`
    ended the node at once under PHP 8 (calling a disabled function is a fatal
    error); every such call is now guarded, and the installer's `ping` lists
    what the host has taken away.
  - a generation hands over to its successor early and consistently (about two
    thirds of the known limit, never later than 45 s) instead of reaching for a
    longer run: on a host whose real limit we have not seen, aiming high got a
    generation killed before it had started a successor, and the chain broke.
    A CPU or wall limit the node has actually hit lowers the handover further.
  - a successor takes new streams only once its carrier link has stayed up a few
    seconds (a Mail.ru document drops the first connections right after they
    join, and streams handed over in that moment were lost); the client re-asks
    an unanswered OPEN and the exit answers a repeated one without dialing twice.
  - over cups.online the node no longer slept between messages inside its loop
    (which stalled new connections under a download) and sends only as fast as
    the server confirms, so the server does not drop the link; two generations
    sharing one room no longer corrupt each other's data.
  - the status page restarts a node that ended on its own while the page is open.
- `--mode=stream` without `--inbound` is SOCKS5 again on every OS (0.3.0 took
  the macOS client to utun, which needs root); the Desktop app names
  `--inbound=socks5` for a proxy profile.


- phpbox chain mode did not renew on real hosts: the successor was started by a
  request its predecessor closed at once, the node wrote its first lines (`joined
  ...`) into that closed connection, and on hosts where `ignore_user_abort` does not
  hold PHP ends the script at such a write, so every successor died right after
  joining and the tunnel needed a manual restart at each cap. A successor now writes
  nothing to its response (a first run only its opening lines); the request that
  starts it also reads a quick answer (a redirect, the host's browser check, an
  error page), logs it, and a retry tries the other of http/https.

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
