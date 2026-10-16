# Changelog

All notable changes to the OpenFlux core. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

- node-install.sh follows the node-v* releases of p1neappleXpress/OpenFlux, and the node wizard installs it for both sources (`fork` and `official`).

## [0.14.0] / node-v1.7.0 - 2026-10-16 (OpenFluxTeam/OpenFlux)

node-install.sh installs **node-v1.7.0**. The team's stripe groups (p1neappleXpress, `integration/team-0.12`, the CLI release v0.12.1) join the speed work.

### Added

- **Stripe groups** (p1neappleXpress): several carriers of one type (cups.online, Avito) or of mixed types are joined into one
  stripe, one lane each, with per-stripe RTO and ack settings; `--lanes N` on the command line, `Lanes = N` in a `[Transport]`
  section, a seamless single-lane Mail.ru carrier (make-before-break rotation), tuned cups and Avito defaults (8 rooms).
- **Reno on direct.** While the traffic goes over plain TCP streams (direct) the tunnel stack opens new connections with Reno
  instead of CUBIC: nothing is lost inside such a tunnel, and CUBIC's HyStart took the carrier's delay jitter for congestion and
  left slow start at half the window the path takes (a single download over a real path: 6-7 MB/s with Reno against 3.5-4.5 with
  CUBIC; on the rig +20 %). Carriers that drop packets keep CUBIC. The stack follows the carrier that is used, within a second;
  `OPENFLUX_TCP_CC=reno|cubic` forces one.

### Removed

- **Session bonding** (`--bonding`, `Bonding = yes`, control subtypes 0x30/0x31), replaced by stripe groups. A `.conf` that still
  says `Bonding = yes` logs that it is ignored; the carriers fail over by priority. `Network =` stays.

## [0.13.1] / node-v1.6.1 - 2026-10-16 (OpenFluxTeam/OpenFlux)

node-install.sh installs **node-v1.6.1**.

### Fixed

- **A restarted exit is taken over by its client at once.** The restarted exit had no client and dropped everything the old
  one sent; the client noticed only after 30 s of silence («exit silent on every carrier for 30s: handshaking again»), and
  never when it had not yet learned that its exit answers keepalives. The exit now answers an authenticated packet that
  arrives without a handshake with a hello (at most one a second) and the client takes it over through the candidate
  exchange: about 4 s instead of 30 s or never.

## [0.13.0] / node-v1.6.0 - 2026-10-16 (OpenFluxTeam/OpenFlux)

node-install.sh installs **node-v1.6.0**. The speed work of the `speedTest` branch (log: TRANSPORT_PERFORMANCE.md).
Nothing on the wire changed: old clients and nodes work with new ones, but the gain in the node-to-client direction
needs the node updated (each side picks the carrier it sends on).

### Added

- **A live direct carrier is used first**, whatever priority the profile gives it (`transport.PreferredCarrier`). Profiles had
  direct at 50, below every document carrier, so the traffic went through a third party while a straight path worked. When
  direct stops reaching the peer the next carrier takes over, and takes the traffic back when it returns. `Prefer = no` in a
  `[Transport]` section or `OPENFLUX_DIRECT_FIRST=0` turn it off. New profiles and nodes give direct priority 110.
- **Large MTU on direct** (16000, `OPENFLUX_DIRECT_MTU`): the tunnel follows the carriers that are live; a packet that no live
  carrier moves is refused and the stack gets an ICMP fragmentation-needed, so a connection survives the fall back to a
  carrier with a smaller limit.
- `OPENFLUX_PPROF` (pprof endpoint), `cmd/speedbench` and `scripts/speedtest` (the test rig), extended gVisor counters in the debug STATS line.

### Changed

- Much less CPU per byte on the path of every carrier: zstd is tried only where it pays (never for the repeated envelope of TLS
  traffic on a direct carrier), packets are copied and allocated far less (pooled send buffers, the stack writes straight into the
  session buffer), the direct carrier writes with one `writev` and reads through a buffer, a lone packet after a pause is not
  delayed 5 ms, scrypt runs once per key. On a 1-vCPU VPS the exit needs 1.9 s of CPU for 96 MB instead of 12.9 s.
- Queues are bounded in bytes (512 KB) and the carrier socket has `TCP_NOTSENT_LOWAT` on Linux/Android, so a download does not
  leave megabytes in front of the next page.

### Fixed

- Telemost's filler frames no longer make a Session peer log that the other side speaks classic mode.

## [0.12.0] / node-v1.5.1 - 2026-10-15 (meepo161/OpenFlux)

node-install.sh installs **node-v1.5.1**: node-v1.5.0 with the 0.7.3 Mail.ru batch.

### Changed

- Merged p1neappleXpress 0.7.3 (section below): the Mail.ru carrier packs up to
  500 KB into each chat message, because the server cuts a connection after about
  a dozen messages whatever their size. Only the sender changes, so old clients
  and nodes keep working with new ones.

## [0.11.0] / node-v1.5.0 - 2026-10-14 (meepo161/OpenFlux)

node-install.sh installs **node-v1.5.0**: the node core with Avito, the 0.7.2
fixes, ScriptAutoUpdate and everything below.

### Changed

- Merged p1neappleXpress 0.7.2 (section below): an Avito socket that dropped no
  longer crashes the process; a cups.online or Avito exit on the phone starts
  without a room list; steadier throughput tests. The «official» node script is
  pinned at 630a0e5 (node-v1.4.1, the 0.7.2 exit core).

### Added

- **The node wizard installs every transport**: Telemost (a conference link),
  Avito Code (rooms made at once, no sign-in) and signed `.flux` JS transports
  (the app sends the package; the node keeps it current, `ScriptAutoUpdate`).
  `node-install.sh` takes `telemost=`, `avito=` and `script=` lines;
  `deploy/test/config-test.sh` checks the installer's config and node.conf
  under the VDS's shell.
- **Trial channels get Avito too.** A pool without transports (opensub's
  personal channels) gets cups.online and Avito Code rooms, both made without
  any sign-in; tried live: a tunnel through rooms made this way carries
  traffic (about 100 KB/s cups.online, 80 KB/s Avito on one client). Telemost
  conferences need a Yandex sign-in to be made (401 without one), and the JS
  whiteboards (MTS-Link, Bitrix24, PRUFFME, Yandex Boards) join an existing
  board as a guest but cannot make one.

## [0.10.0] - 2026-10-13 (meepo161/OpenFlux)

### Added

- **opensub gives each app links with the transports it runs.** The app lists
  them in `X-OpenFlux-Transports`; an app without the header gets the set links
  carried before (yandex, vyandex, boards, mailru, cupsonline), since it drops a
  link with a type it does not know and the whole location with it. Telemost
  reaches subscribers this way (`share.Shareable` is the one list), and
  `provision` takes Telemost conference links.

### Changed

- Merged p1neappleXpress 0.7.1 (section below): the avito carrier, cups.online
  and avito as stripes with the ack-gated send window and 12 KB messages. The
  fork's cups.online carrier keeps choosing its network (`--network`), the
  stripe lanes too, and keeps giving up a half-received packet on the old wire
  (`StreamResyncAfter`). The «official» node script is pinned at 3652885
  (node-v1.4.0, the 0.7.1 exit core); the fork's own script still installs
  node-v1.4.2 until the next fork node release.

## [0.9.0] / node-v1.4.2 - 2026-10-13 (meepo161/OpenFlux)

### Changed

- Merged p1neappleXpress 0.6.0 and 0.6.1 (sections below): the Mail.ru carrier
  now carries the tunnel over the document's chat channel (the Mail.ru server
  closes the socket on synthetic cursor writes, so the old path is dead), the
  scripted Mail.ru carrier connects again, and a node can keep its `.flux`
  transports updated and self-healing (`--script-auto-update`). The fork's
  fix that unlocks the document when a second editor joins stays on the new
  path. Client and node must both run this version for Mail.ru.
- The «official» script the node wizard offers is pinned at
  p1neappleXpress c9076e7 (node-v1.3.1).

## [0.8.0] - 2026-10-11 (meepo161/OpenFlux)

### Added

- `node-install.sh show` returns the channel's encryption `key`, read as root
  over SSH, so the owner's app can make the link and profile of a node that was
  installed elsewhere (a server screen lists all nodes on it and shares or
  connects to any). `provision.ChannelInfo.Key`; its `String()` leaves the key out.

## [0.7.0] - 2026-10-11 (meepo161/OpenFlux)

### Added

- The node's mode, l4 or l3, is chosen when a node is made and can be changed on
  an installed channel. `node-install.sh` takes `mode=l3|l4` (plan, apply,
  set-transports) and reports `mode` (show, apply, set-transports). l4 stays the
  default and needs nothing. l3 passes IP packets through raw sockets: the
  channel's unit gets a drop-in with `AmbientCapabilities=CAP_NET_RAW`, and
  `l3-rst.sh` adds an iptables rule dropping the kernel's own TCP RSTs from the
  server's address while an l3 channel runs (and removes it when the last one
  stops). It needs iptables, iproute2 and systemd 231+, and says so before
  installing; the rule is host-wide for that address, which the plan states.
  `provision.Channel.Mode`, `provision.CheckMode`, `provision.Conn.SetChannel`,
  `ChannelInfo.Mode`; `mobile.NodePlan` / `NodeApply` / `NodeSetTransports`
  take the mode; the desktop node wizard takes `mode`.

## [0.6.0] - 2026-10-11 (meepo161/OpenFlux)

### Added

- Subscription service `opensub` (`cmd/opensub`, `subscription/`): the owner of
  nodes sells or gives access to them without handing out documents, keys or
  SSH. People make an account in the app (login and password, no email; a
  recovery key shown once), get a trial, pay (a manual provider: the owner
  marks the order paid) and press "Подключить"; the service answers with the
  locations built from the owner's node channels (`/etc/openflux/…` channel
  files), re-read when they change. Keys without an account
  (`openflux://sub?…`), devices per plan, password change and recovery,
  account deletion; bcrypt over a SHA-256 prehash, rate limits, a self-signed
  TLS certificate whose SHA-256 is in the key (pinned by the apps). The owner
  runs it with `opensub serve` and manages it with `opensub users | grant |
  ban | key-add | orders | paid …`. The whole design, every user step and edge
  case: docs/subscriptions.ru.md. Release assets: `opensub-linux-amd64`,
  `opensub-linux-arm64`.
- `subscription.Invite`, `subscription.Document`: the key and the subscription
  document the apps read (profiles are `openflux://v1/…` links, checked with
  the same reader as imported ones).

## [0.5.1] / node-v1.4.1 - 2026-10-10 (meepo161/OpenFlux)

### Added

- The apps change the documents of a channel already on the server: the node
  wizard lists the server's channels, "Изменить транспорты" reads one
  (`node-install.sh show`) and applies the new set in place
  (`node-install.sh set-transports`). The key, port and encryption context
  stay, so links handed out keep working; with the channel's profile on the
  device the wizard builds the new link with the same context, checks the
  channel and updates the profile. `provision.Conn.Show`,
  `provision.Conn.SetTransports`, `provision.ShareLinkContext`; the node
  wizard's `showChannel` / `setTransports`; `mobile.NodeShowChannel`,
  `mobile.NodeSetTransports`, `mobile.NodeShareLinkContext`.

### Fixed

- Mail.ru Docs: the document server now answers an editor WebSocket that
  claims to be Chrome with 409 Conflict when the TLS handshake is not
  Chrome's, so the carrier never joined a document on either side. It
  presents itself as Firefox, which the server accepts.
- cups.online: a lost message no longer stalls a member's stream. A part of a
  packet still waiting after `StreamResyncAfter` (2 s) is given up and the next
  message starts the stream afresh; before, every later packet of that member
  (resends too) was lost until a length happened to line the stream up again.
- `TestInstallOnVDS` (the Node install workflow) answers the fork's own release
  repository, so the updater steps run against the fake GitHub again.

## [0.5.0] / node-v1.4.0 - 2026-10-10 (meepo161/OpenFlux)

The fork on p1neappleXpress/OpenFlux 0.4.2: its script (JS) transports, Mail.ru and
Yandex fixes, and the phpbox carrier, with the fork's bonding, node wizard, and
per-carrier network binding.

### Added

- Adding and removing documents on a node that already runs: `node-install.sh
  set-transports CONFIG` (`provision` / the apps' node management call it over SSH).
  The key, the port, the cookies and the encryption context stay, so links already
  handed out keep working. The node is restarted; if it does not come back up on
  the new config, the previous one is restored.
- Script (JS) transports, phpbox carrier and the rest of p1neappleXpress 0.4.0-0.4.2
  (see the upstream sections below).

### Changed

- Merged p1neappleXpress/OpenFlux 0.4.2 into the fork. The Yandex carriers dial IPv4
  first, through the carrier's own network binding (`dialIPv4FirstOn`), so bonded
  carriers keep their interface.
- The Yandex captcha stays a client job: the check is passed in the app and the
  cookies reach the exit over the Session (`mobile.OfferExitCookies`), so one
  carrier's cookies serve the others when a session cookie expires.

## [0.4.0] / node-v1.3.0 - 2026-09-30 (meepo161/OpenFlux)

### Added

- Bonding: a client with `--bonding` (`Bonding = yes` in a .conf) uses all
  its carriers at once, so documents on several networks (mobile data and
  Wi-Fi) or several documents on one add their speeds up, and a carrier
  that drops leaves the rest carrying on. TCP connections spread over the
  carriers, one carrier each, and move when theirs is lost; UDP (SRT, QUIC)
  is split packet by packet. The exit agrees by itself; exits before
  node-v1.3.0 ignore the request and the client routes as before. Three
  Mail.ru documents, three connections: 0.84 MB/s against 0.59 on one.
- `Network = cellular|wifi|ethernet` per `[Transport]` binds a carrier to a
  network: Android through the app (mobile data kept up next to Wi-Fi),
  Windows by adapter type. The mobile library takes `"network"` per
  transport, `"bonding"` next to them, and the app's NetworkBinder.

### Fixed

- `.conf` booleans took only true/false: `Bonding = yes` read as off.

## [0.3.3] / node-v1.2.3 - 2026-09-30 (meepo161/OpenFlux)

### Fixed

- Mail.ru carrier: when the second peer joined the document, the editor
  server locked the document on the first one and, since it never unlocked
  it, dropped it 30 seconds later (disconnectReason 4007); rejoining, it
  locked the document on the other peer. The carrier went down every half
  minute while both peers were on it, and every rejoin stalled traffic for
  up to 30 seconds, so long uploads such as a stream broke. The carrier now
  unlocks the document for a joining peer (unLockDocument); a test pair held
  a two-minute upload at about 4.8 Mbit/s without a drop.

## [0.3.2] / node-v1.2.2 - 2026-09-30 (meepo161/OpenFlux)

### Fixed

- A node set up by the wizard (.conf carriers) or with --transports served
  Session clients only: a client that speaks classic, such as the iOS app,
  was dropped on every carrier ("serves Session peers only") and never got
  through. Such an exit now also serves classic clients on any of its
  carriers, as a --transport=X exit does; --negotiate stays Session-only.
  The exit serves one client at a time, and a classic client is answered
  only while no Session client is active on the channel.

## [0.3.1] / node-v1.2.1 - 2026-09-29 (meepo161/OpenFlux)

### Fixed

- A full packet batch queue now waits for capacity instead of dropping data.
  A batch rejected by a temporarily full or disconnected carrier is retried
  in order until it is accepted or the tunnel stops.

### Added

- Tunnel diagnostics report queue waits, carrier send retries and TCP
  retransmissions every 30 seconds when any of those counters increases.

## [0.3.0] / node-v1.2.0 - 2026-09-29 (meepo161/OpenFlux)

The fork on p1neappleXpress/OpenFlux 0.2.0 (one protocol for every client,
links made and read by the core) with the node wizard's transport choice
and self-updating nodes (p1neappleXpress/OpenFlux#125) and the Accounts
core side (`mobile.OfferExitCookies`). The node core is `node-v1.2.0`.

## [0.2.0] / node-v1.1.0 - 2026-09-28 (meepo161/OpenFlux)

Releases of the meepo161 fork: the core with the node wizard's transport
choice and self-updating nodes (p1neappleXpress/OpenFlux#125), wired to
this fork.

### Changed

- `deploy/node-install.sh` installs the core from this fork's `node-v*`
  releases and its updater follows them (`RELEASE_REPO=meepo161/OpenFlux`).
- The node wizard (`--node-wizard` `connect`, `mobile.NodeConnect`) takes a
  `source`: `fork` (default) installs this fork's core, `official` the same
  script following p1neappleXpress/OpenFlux (`provision.PinnedFor`).

## [0.7.3] - 2026-10-14 (p1neappleXpress/OpenFlux)

### Changed

- **Mail.ru is much faster.** The Mail.ru docs server cuts an anonymous editor after about a dozen
  chat messages no matter how big they are, so the tunnel now coalesces far more into each message
  before that cut — end-to-end throughput rises from tens of KB/s to roughly 800 KB/s per burst.
  Only the Mail.ru carrier changes its default batch size; every other transport is byte-for-byte
  unchanged, and `OPENFLUX_BATCH_BYTES` still overrides it.

## [0.7.1] - 2026-10-13 (p1neappleXpress/OpenFlux)
## [0.7.2] - 2026-10-13 (p1neappleXpress/OpenFlux)

### Fixed

- **A cups.online or Avito exit node could not be started on the phone**: the app asked for a document link
  ("Ссылка на документ не указана"), but an exit of these two given no room list is the one that creates its
  rooms. It starts now, and the link it shares carries the rooms it created (for Avito the list was left out).
- **An Avito socket that dropped could crash the whole process** (the exit node, or the app): its reader went on
  using the connection after it had been cleared, or read the next connection next to that one's own reader.
  Each reader now keeps to its own socket and is waited for.

## [0.7.1] - 2026-10-13

0.7.0 was cut from a branch that did not carry 0.6.0 and 0.6.1, so it lost the Mail.ru chat-channel fix
(Mail.ru answered "access deny" 4004 again) and the node's `.flux` auto-updater. 0.7.1 is 0.6.1 plus
everything in 0.7.0, plus the fixes below.

### Added

- **avito (code.avito.ru) carrier**, and `avito` in share links.
- **cups.online and avito run as stripes.** Each cups room (each avito room pair) is one lane of a reliable
  striped link (sequence numbers, reorder, retransmission), so a message the server loses no longer breaks
  the tunnel's byte stream, and speed adds up across rooms. A fresh exit prints a new room list format
  (`{"v":2,...}`); an old bare list keeps the old wire, so running nodes and older apps keep working, but
  an older app cannot use a new list. `CUPS_STRIPE=0` / `AVITO_STRIPE=0` make an exit print the old list.
  `CUPS_ROOMS` sets how many rooms a fresh cups exit creates (default 4).

### Fixed

- **cups.online crawled at 25-90 KB/s.** The transport fired a message every 12 ms with no feedback; the
  server relays a room only at ~130-200 KB/s and silently drops the rest from the relay (the send is still
  acknowledged), so about 70% of what was sent never arrived and the retransmissions stalled the tunnel.
  A lane now keeps at most 3 messages unanswered and sends the next one when the server answers, so it
  follows each room's real rate; messages carry 12 KB instead of 8 KB. Through the tunnel, 4 rooms: 20 MB
  at 424 KB/s (was 90 KB/s), retransmissions 2% instead of more than the data itself. `CUPS_WINDOW`
  tunes the window (0 turns it off).
- **avito sent one packet per round trip** (the chunk size never applied); an operation now carries up to
  256 KB of framed packets, and its send/read loops stop with their socket instead of outliving a reconnect.
- **A striped cups lane corrupted frames**: it kept slices of a receive buffer the socket reused.

## [0.4.2] - 2026-10-07 (p1neappleXpress/OpenFlux)
## [0.6.1] - 2026-10-13 (p1neappleXpress/OpenFlux)

### Fixed

- **The Mail.ru carrier dropped every connection with "access deny" (code 4004).** The current Mail.ru editor
  server (R7/OnlyOffice 2026.2.1.2268) validates cursor and `saveChanges` writes against the live document
  model and closes the socket on any synthetic one, so the transport's old trick of stuffing tunnel bytes into
  the cursor field stopped working entirely. The data path now rides the document's **chat channel**
  (`type:"message"`), which the server relays verbatim to the other participant without validating it against
  the document: tunnelled bytes are tagged and base64-encoded there, the server's echo of our own chat and a
  human's chat are skipped on receive, and the old cursor keep-alive and `saveChanges` "editor activity" are
  gone (the Socket.IO ping/pong keeps the socket up). Both the native transport and its scripted (`.flux`)
  port carry data this way now.
- **The scripted Mail.ru carrier could not connect at all (HTTP 409 on the WebSocket upgrade).** `ws.open`
  now offers permessage-deflate when a script asks for it (`{compress: true}`) — Mail.ru's nginx rejects the
  upgrade without it — and sends `Accept-Language`, the same two fixes the native transport already carried.

## [0.6.0] - 2026-10-12 (p1neappleXpress/OpenFlux)

### Added

- **A node can keep its scripted (`.flux`) transports current and self-heal, with no restart.** The new
  `--script-auto-update` flag turns on a headless updater — the counterpart of the app's updater for a CLI or
  exit node that has no GUI. It checks each `.flux` carrier's author feed on the usual schedule and installs
  an update when the core vouches for it (first-party key, same wire); and when a carrier keeps failing to
  connect it walks a recovery ladder: check for a newer package now, then roll back to the previous one, then
  fall back to the native transport of the same service. An applied or rolled-back package is **hot-swapped**
  into the live session, so only that one carrier blinks while it reconnects — the handshake, the other
  carriers and the packet flow stay up. `--script-update-channel` selects `stable` or `nightly`. Off by
  default, so node behaviour is unchanged unless asked for.

## [0.5.1] - 2026-10-11

### Fixed

- **A Telemost exit crashed with "negative shift amount"** when a client vanished mid-transfer: a piece
  resent 255 times wrapped its retry counter to zero and the timeout's shift went negative. The counter now
  saturates, and the sender drops a piece nobody acked for the hole deadline plus 3 s, so a gone receiver no
  longer keeps the lanes resending.
- **Downloads through an L4 exit sat at about 1.5 MB/s on many servers.** The exit asked for 16 MB socket
  buffers on every outgoing connection; Linux cuts that to `net.core.rmem_max` (416 KB on a stock kernel) and
  turns buffer autotuning off, so the exit fetched from the internet at 1.4 MB/s where the same socket left
  alone does 42 MB/s. The buffers are left to the kernel.
- **Script transports with `--transport=script` lost what their setup page was given** (a token, a pairing)
  at every start: the cookie store was never opened for them. Each script now gets its own jar,
  `./cookies-script-<name>.json` by default, under the same key the Session path uses.
- **Telemost lanes paired by guesswork.** The SFU drops a participant's description, so the lane tags never
  reached the other end; the tags now ride in the participant name as characters that take no space, and a
  node pairs with the newest run of the other end at once, ghosts of earlier runs aside.
- **Mail.ru Docs still answered 409 on some clients** after the User-Agent fix (reported on iOS, #164):
  the WebSocket upgrade and the API call now also send `Accept-Language`, the way a browser does.

### Changed

- **Telemost starts with three lanes and adds more under load** (up to six; `stripes` and `stripes_max`
  in a profile, `TELEMOST_STRIPES_MAX` in the environment). A lane is added only after 15 s of full
  saturation: a new pair of lanes joining stops the SFU forwarding every lane for 10-15 s.
- **Telemost lanes join as "Гость"** - no program name in the participant list or the RTCP CNAME.
- **Steadier speed on striped carriers.** Each lane's rate follows what the SFU lets through (lanes of one
  conference differ: 6% and 64% lost at one rate), the shared send queue lets TCP ride out the carrier's
  swings (CoDel 150 ms / 600 ms instead of 30/250), and a lane that stops delivering keeps its stream warm
  with copies instead of falling silent into the SFU's ramp. Between two phones on Wi-Fi, three lanes: 4.4-5
  MB/s steady after the first seconds, against 1.2-1.7 MB/s on average before.

## [0.5.0] - 2026-10-11

### Added

- **Telemost transport (type `telemost`, `-t telemost`).** The tunnel rides a Yandex Telemost conference as
  video relayed by its SFU; the conference link is all a profile needs. One Telemost stream carries at most
  about 16 Mbit/s (the SFU caps each subscriber), so the carrier runs **five lanes**: five participants of the
  conference on each end, each publishing a camera and watching one lane of the other end, with the tunnel
  striped over them. The profile parameter `stripes` sets another count, or `auto` (start at 3 and add lanes
  while they stay saturated). Share links (`--share`) carry it. Both ends need this version, and one
  conference serves one client and one exit. Clients run it through their proxies or the Android VPN; the
  desktop full tunnel (`--inbound=tun`) refuses it for now, since its media would loop into the tunnel.
  Experimental.
- **A reliable link under the tunnel for striped carriers.** The SFU loses 10-80% of a lane's packets, and
  the tunnel's TCP read every loss and every slow repair as congestion and lived in timeouts. The stripe now
  delivers in order and repairs what the SFU drops: lanes take packets on their own send clock (ack first, then
  resends, then new data), the receiver acks every 10 ms, a packet is resent - on another lane - as soon as
  a later packet of the same lane arrives, one send queue under CoDel gives TCP a clean congestion signal,
  idle room in a tick carries early copies of overdue packets, and a lane whose packets stop arriving only
  gets probes. Telemost publishes each tick's packets spread over the tick: back to back they lost 50-80%
  on the way from a server, spread 0-10%. Measured with an HTTPS download through the tunnel, exit on a
  server and client on a phone: **from 24 KB/s to about 1.5 MB/s** on average (3.4 MB/s at peak), with
  0.014% TCP retransmissions and no timeouts; the raw carrier delivers 1.7 MB/s on the same path. The first
  seconds of every transfer are still slow: the SFU raises a stream's allowance only gradually.
- **Script transports: WebRTC with an RTP line and typed data-channel messages.** `pc.addTransceiver(kind,
  {direction})` adds an audio or video m-line to the next offer (a conference SFU such as MTS-Link's wants
  a `recvonly` audio line next to the data channel), and a data channel's `onmessage(bytes, isString)` now
  tells text from binary, so a transport can keep the signaling strings apart from the tunnel's packets.

### Changed

- The tunnel's TCP (gVisor) uses CUBIC instead of Reno (`OPENFLUX_TCP_CC=reno` restores Reno), a 3-second
  floor for its retransmission timeout (`OPENFLUX_TCP_MIN_RTO_MS`) and duplicate-ACK loss detection instead
  of RACK and tail loss probes (`OPENFLUX_TCP_RACK=1` restores them). Carriers deliver in order but now and
  then late, and a timeout that fired on late data used to cost a connection its whole window.

Exit nodes that carry Telemost need `node-v1.3.0`.

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

## [0.4.1] - 2026-10-07 (p1neappleXpress/OpenFlux)

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

## [0.4.0] - 2026-10-07 (p1neappleXpress/OpenFlux)

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

## [0.3.0] - 2026-10-01 (p1neappleXpress/OpenFlux)

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

## [0.2.0] - 2026-09-28 (p1neappleXpress/OpenFlux)

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

## [0.1.0] - 2026-09-27 (p1neappleXpress/OpenFlux)

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
