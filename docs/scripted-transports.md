# Scripted (JS) transports

`transport/script` hosts JS-defined transports inside a per-transport
[goja](https://github.com/dop251/goja) runtime. A script transport is one
signed `.js` file (or a signed `.flux` package) that implements an entire
transport - auth flow, wire framing, reconnect/keepalive policy - on top of
a small host API the Go side injects. The Go side only does real I/O (HTTP,
WebSocket, UDP, timers) and pumps callbacks into the script's own event
loop; everything else is JavaScript. The full contract lives in
[`transport/script/js/template.js`](../transport/script/js/template.js) -
read that before writing one.

Use `--transport=script` (session only, needs
`--encryption-key-file`) with `--script-path=<name>.flux|.js`,
`--script-pubkey=<hex>` and optionally `--script-name=<name>`. See
[sessions.md](sessions.md) for how a script transport joins a session like
any other.

## What ships today

`transport/script/js/` has the shipped scripts: `yandex.js`, `vyandex.js`,
`boards.js`, `mailru.js`, `cupsonline.js`, `oneme-webrtc.js` and
`oneme-iceinject.js` (each with a detached `.sig`) - scripted ports of the
native transports, used as the real test bed for the engine. `js/src/` holds
their shared, un-bundled sources (see `cmd/scriptbundle` below).

The apps link the engine in-process. On iOS the C API (`mobile/ios/script.go`)
carries the same calls as gomobile's (`OpenFluxInspectTransport`,
`OpenFluxScriptSettings`, `OpenFluxScriptEngineSelfTest`, the update calls
below); a profile runs an installed transport through a Session spec of type
`script` with `path`/`pubkey`/`name`, and since the Network Extension opens
that file, the app installs transports into its App Group container.

## The host API

Everything injected into a script's runtime is deliberately unrestricted -
no capability scoping, no allowlisted hosts. The only gate a script passes
through is the signature check (see "Trust model" below), done once before
any of this ever runs:

- `http.fetch(opts)` / `http.newSession()` - HTTP requests with redirects
  followed or manual, and an isolated cookie jar per logical session (for a
  transport that juggles several independent sessions on one domain, e.g.
  several cups.online rooms).
- `ws.open(url, headers?, opts?)` - a WebSocket `Socket` with
  `send`/`close`/`onmessage`/`onclose`, dispatched onto the script's own
  event loop so JS never blocks on I/O. `opts.readTimeoutMs` fires `onclose`
  on a silent connection instead of hanging forever.
- `udp.open(addr, opts?)` - a dialed (fixed-remote) UDP datagram socket,
  same shape as `ws.open`'s socket.
- `httpserver.listen(handler, port?)` - **a script's own setup/mini-app
  page**: when a static HTML blob (`raise()`, below) isn't enough - a
  script that needs its own routes, a form that posts back to itself, a
  real origin for its own `fetch()` calls - it serves one over a real
  HTTP server instead. Always binds `127.0.0.1` only (a listening socket is
  the one host primitive that can reach *other* local processes, so it
  never takes a host argument). The app opens a WebView/browser at the
  returned `addr`, exactly like it does for a real site's login page.
- `cookieJar.get()` / `.set(values, domain?)` - the transport's own cookie
  jar, scoped to `info().cookieDomain` (or its parent domain, if
  `scopeCookiesToParentDomain` is set - needed when a captcha/login solve
  on one subdomain must reach a sibling subdomain).
- `base64`, `text` (encode/decode), `url.parse`, `gzip`, `lz4.decompressBlock`,
  `crypto.sha256` - generic codecs so a script's own wire format doesn't
  need a hand-rolled JS implementation.
- `crypto.solvePow(prefixHex, complexity)` - the one hot loop that stays
  native: Yandex SmartCaptcha's proof-of-work needs millions of SHA-256
  attempts, and a Go<->JS call per attempt would dwarf the cost of the hash
  itself. Everything *around* the PoW (fetching the challenge, building the
  fingerprint, posting the answer) still lives in the script.
- `concurrency.pool(n)` - bounded fan-out (`pool.run(fn)` gates at most `n`
  concurrent in-flight calls through a Go semaphore) for a Volga-style
  transport that needs real worker-pool concurrency, which goja's
  single-threaded runtime can't provide on its own.
- `emit(bytes)` - deliver one received application packet up to the core.
- `setState(state, err?)` - `"connecting" | "connected" | "reconnecting" |
  "degraded" | "dead"`. This is the **only** way `IsConnected()` on the Go
  side ever flips true; the core has no other way to know a script's link
  health.
- `raise(kind, payload)` / `onEvent(kind, payload)` - out-of-band signaling,
  in both directions:
  - `raise(kind, payload)` (JS -> Go) carries an event upward - any `kind`
    reaches whatever the host registered via `SetEventHandler`;
    `"captchaRequired"` and `"needsSetup"` additionally reach
    `SetErrorNotifier`, the same path the native yandex/mailru transports'
    captcha signal takes, carrying `payload.url` (a real site, or the
    script's own loopback server), `payload.html` (the script's own page)
    and `payload.reason`. For those two kinds the payload is checked first
    (`setuppage.go`) and a bad one throws a `TypeError` into the script - see
    "Settings and setup pages" below.
  - `onEvent(kind, payload)` (Go -> JS) is the script's optional export for
    the reverse direction - an inject the host wants to push down (for
    example applying externally-supplied cookies after the app solved a
    captcha). A script that doesn't define `onEvent` silently ignores it.

## Transport.info()

`info()` is called once, synchronously, right after the script is
evaluated - before `open()`. All fields are optional except `name`. It
declares:

- `name`, `version`, `cookieDomain`, `scopeCookiesToParentDomain`.
- `mtu`, `reliable`, `ordered`, `halfDuplex`, `minIntervalMs` - advisory
  only; the core never fragments, reorders or rate-limits on a script's
  behalf unless it opts in. The default is plain passthrough.
- `httpMaxConnsPerHost` / `httpMaxIdleConns` / `httpIdleConnTimeoutMs` - HTTP
  connection-pool tuning, for a transport that fans out many concurrent
  requests via `concurrency.pool`.
- `params` - what the script asks the user for and lets them tune, see
  "Settings and setup pages" below. `open(cfg)` gets the first one as `cfg.url`
  and the rest as `cfg.params` (always strings, declared defaults filled in).

## Signing and trust (`trust.go`)

Every script transport must pass a signature check before its bytes are
ever handed to goja - there is no unsigned-but-trusted path
(`VerifyScript`, ed25519). Freedom inside the runtime (the unrestricted host
API above) is traded for a hard gate at the door.

- A bare `.js` ships alongside a detached `<name>.js.sig`.
- A `.flux` package (a zip: `manifest.json` + `main.js` + an optional icon +
  `package.sig`) carries its own signature over all of those bytes,
  length-prefixed in a fixed order (`PackagePayload`), so a legitimate
  transport's name/author/description can't be swapped without
  invalidating the signature. Built and signed with `cmd/scriptsign`'s
  `pack` command.
- `OfficialKeyHex` (in `trust.go`) is the project's own signing key; a
  script signed by it is shown as first-party, every other key is an
  unknown author pinned on trust (TOFU - the key arrives with the share
  link, not inside the package).
- `InspectTrust(data, sig, pubkeyHex, officialKeyHex)` reads a downloaded
  transport **without ever running `open()` or touching the network** and
  returns a `TrustReport` (name, version, params, signature
  `valid|invalid|unverified`, fingerprint, official/author) - what an app
  shows in its "trust this transport?" dialog, and what it stores alongside
  the file afterwards.
- `--inspect-script --data=<path> [--sig=<path>] [--pubkey=<hex>]` is the
  desktop app's way into `InspectTrust`: the desktop app only ever has the
  compiled core binary (unlike the mobile apps, which link the engine
  in-process via gomobile), so it shells out to this subcommand and gets a
  JSON `TrustReport` on stdout. Always exits 0 and prints a report even when
  the script fails verification - "untrusted" is a report field, not a
  process failure.

## Settings and setup pages

Two ways a script talks to the user besides its URL; both end as a page in the
app's built-in browser and hand data back with `window.openfluxSubmit`. The
author's guide is the SDK's
[07-settings-and-setup-pages.md](https://github.com/p1neappleXpress/OpenFluxSDK/blob/main/docs/07-settings-and-setup-pages.md);
this is the engine's side of it.

**Settings (declarative).** `info().params` entries carry `key, label, type
(text|url|secret|number|boolean|select|textarea), required, scope, default,
description, placeholder, options, min, max, pattern, group, advanced`
(`manifest.go`, `Param`). The first param is the profile's one input (`cfg.url`)
unless `scope` says otherwise; the rest are settings (`params.go`: `scopes`,
`SettingParams`, `ResolvedParams`). `SettingsPage` (`settingspage.go`) generates
one self-contained wizard page from **every** param `ResolvedParams` returns,
the profile one included - the field in the profile editor and the one in the
wizard edit the same saved value, so an app can offer either, or both, for it;
a script may bring its own page with `Transport.settings(values)`
(`InspectSettings`: run like `Inspect`, no I/O, 3 s, sync). `BuildSettings` is
the apps' entry point: it verifies the script against the pinned key first,
then returns the page, the resolved params and the values with defaults filled
in as a `SettingsReport` (CLI `--script-settings`, gomobile `ScriptSettings`).
The app keeps what the page submits (an app with a profile field for one of
these params usually also updates it there, from the same submission); at
start it reaches the core as `Params = <base64url JSON>` in the `.conf` (a
`.conf` value is cut at `#`/`;`; `EncodeSettings`/`DecodeSettings`) or as
`params.settings` in a mobile session spec; `registry` flattens it into
`cfg.params` (never over `path`/`pubkey`/`name`/`exit`/`settings`) and
`WithDefaults` fills what was never set - over every param now, so
`cfg.params[profileKey]` is there too, and `cfg.url` falls back to it if a
caller left `url` empty. `NormalizeSettings` is the one place that says what a
valid value is (the generated page mirrors it in JS); `Info.CheckParams` lists
a declaration's mistakes for its author (`--inspect-script` reports them as
`paramProblems`).

**Setup pages (run time).** `raise("needsSetup" | "captchaRequired", {html |
url, reason})`. `checkSetupPayload` (`setuppage.go`) lets through an `https`
site, an inline page (<= 512 KiB), or an `http` loopback address whose port is
an `httpserver.listen()` this transport started and has not closed; anything
else (another `http` host, a foreign loopback port, `file:`/`javascript:`/`data:`,
`html` with `url`) throws a `TypeError` into the script. `IsOwnPage` marks the
page the script's own (inline, or its loopback server): the IPC request and the
mobile bridge carry it (`CookiesRequestPayload.Own`, `PendingCaptchaOwn`), so no
app guesses from the URL, and the app gives such a page `window.openfluxSubmit`,
collects no cookies, and shows `reason` as the dialog title. A check an exit
node reports whose address is on loopback is dropped by the client's core
(`AcceptRemoteCheck`). What a page submits is read by `FlattenSubmission` (flat
or `{client: {...}}`; strings, numbers, booleans) and delivered through
`ApplyCookies` as `onEvent("cookiesApplied")`.

**`devhost`** (`transport/script/devhost`) plays the app for a script under
development; `scripttest` uses it (`-settings`, `-open`, `-submit`). It is the
reference for the contract above.

## Dev tools (`transport/script/cmd/`)

- **`scriptsign`** - `genkey`, `sign` (writes a detached `.sig`), `verify`
  (a bare script or a `.flux`), `pack` (build and sign a `.flux` from a
  manifest + script + optional icon).
- **`scripttest`** - a manual, live-network smoke test: loads a signed
  `.js` exactly as the real factory does, starts it against a real URL, and
  prints every state/event transition and received packet until the
  deadline. `-cookies-file` lets an out-of-band-solved captcha unblock a
  yandex-family script. `-burst`/`-burst-size` measure real submission
  throughput. `-settings` opens the script's settings wizard and prints what a
  Save would hand to the script (`-param` prefills, `-lang ru|en`); a setup page
  the script raises is served on `127.0.0.1` with `window.openfluxSubmit` in it
  (`-open` shows it in your browser, `-submit '<json>'` answers it unattended).
- **`scriptbundle`** - inlines a script's local `require("./...")` modules
  into one self-contained file (a plain regex scan, not a real JS parser;
  fine for this project's own sources, not meant for arbitrary third-party
  JS). Sign the *output* of this tool, not the pre-bundled sources - see
  `js/src/` for the un-bundled shared code (e.g. the SmartCaptcha solver
  shared by `yandex.js`/`vyandex.js`/`boards.js`).

These three tools are also cross-compiled and published by the nightly
channel - see [building-and-releases.md](building-and-releases.md).

## Versions and updates

The unit of an update is the signed `.flux` package. Its `manifest.json`
(covered by the signature) carries:

| field | meaning |
|---|---|
| `id` | stable identity across versions and mirrors; the package is filed as `<id>.flux` (defaults to the lower-cased `name`) |
| `version` | semantic version, `MAJOR.MINOR.PATCH[-pre]`; must equal the script's own `info().version` (`scriptsign pack` checks) |
| `wire` | wire-format generation (default 1). Same `wire` = client and node interoperate, an update is safe; another `wire` = breaking, both ends must update together |
| `api` | host-API generation the script was written for (default 1, `script.APIVersion`); a core that knows only an older one refuses the package |
| `update` | https URLs of the author's `update.json`; the first that answers wins, the rest are mirrors |

The author publishes an `update.json` next to the release, one entry per
channel (`stable`, `nightly`); `scriptsign index` writes it from the built
package so id, version, wire, api and the SHA-256 cannot disagree with what is
actually published:

```json
{ "format": 1, "id": "vyandex",
  "channels": { "stable": { "version": "1.4.0", "wire": 2, "api": 1,
                            "url": "vyandex-1.4.0.flux", "sha256": "…",
                            "mirrors": ["https://…"], "notes": "…" } } }
```

Nothing in `update.json` is trusted by itself. `script.CheckUpdate` /
`ApplyUpdate` accept a package only if it verifies under the key the user
pinned when they first trusted the transport (a changed key is never an
update: it is a new import with a new fingerprint), its manifest agrees with
the index (id, version, wire, api), its hash matches, it is strictly newer,
and every URL is https. A rejected update leaves the installed file
untouched; an accepted one keeps the replaced file as `<id>.flux.prev`, and
`RollbackPackage` swaps it back (re-verified, so it fails closed too).

The report (`UpdateReport`) carries machine codes only (`status`, `code`,
`wireBreak`, `official`, `autoOk`); the apps own the wording. `autoOk` is the
core's decision for "may install without asking": first-party key, same
wire, same author key. Everything else is asked about.

CLI (desktop): `--check-script-update`, `--apply-script-update
[--allow-wire-break]`, `--rollback-script`, each with `--id --pubkey
--update=<url,…> [--version --wire --channel --dir]`, printing the JSON
report. Mobile: `CheckScriptUpdate`, `ApplyScriptUpdate`, `RollbackScript`
(iOS C API: `OpenFluxCheckScriptUpdate`, `OpenFluxApplyScriptUpdate`,
`OpenFluxRollbackScript`).

## Official keys

A transport is "official" when its signature verifies under a key in
`officialKeys` (`transport/script/trust.go`; `OfficialKeyHex`, the first, is the
one packages are signed with today). Official transports are the only ones an app
updates without asking (`UpdateReport.AutoOK`), so these keys are the root of
that trust and are handled accordingly:

- **Where the private key lives.** On the key holder's machine
  (`~/oflx-keys/script-signing.key`), signed with `transport/script/js/build.sh
  <key>` (bundled scripts) or `OpenFluxTransports/scripts/build.sh <id> <key>`
  (released packages). It is not stored as a CI secret; the release workflow in
  OpenFluxTransports notices the missing `SIGNING_KEY` and a release is made by
  hand.
- **Rotating it** takes two core releases. First add the new public key to
  `officialKeys` and ship that (every installed app now trusts both). Once most
  apps have it, sign with the new key. An update of an official transport signed
  by another official key is accepted and reported as `UpdateReport.NewKey`; the
  app re-pins the install to it. The previous version kept for a rollback is also
  accepted when it predates the rotation.
- **A leaked key** is removed from `officialKeys` in a release: installs pinned to
  it stop being "official" (they ask before updating) and nothing it signed is
  trusted as first-party any more. Re-sign the transports with a new key and ship
  the list that has it first.
- **Third-party transports** never change key through an update, and an update
  index announcing a key other than the pinned one is blocked (`key_changed`): that
  is a new trust decision, made by importing the transport again.
