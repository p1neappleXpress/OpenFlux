# phpbox — a stream-mux exit on plain PHP hosting

`phpbox.php` turns a plain PHP host (including free shared hosting that
cannot run a binary, keep a long-lived process, or listen on a socket) into a
limited OpenFlux exit. The client side is the `transport/phpbox` package.

It is meant for reconnect-tolerant, mostly-`:443` traffic — Telegram (MTProto)
and short HTTPS requests — not as a general full-tunnel VPN exit.

## How it works

PHP hosts buffer the request body but stream the response, so one full-duplex
tunnel rides two half-duplex HTTP channels:

- **down** (`GET …?r=down`): a long-poll that *is* the session. It holds the
  destination sockets, `stream_select()`s them, and streams `dst→client`
  frames back in the response body. Each loop it also drains the upstream bus
  and applies `OPEN`/`DATA`/`CLOSE`.
- **up** (`POST …?r=up`): the client's `OPEN`/`DATA`/`CLOSE` frames, appended
  to a per-session bus file for the down process to pick up. Returns at once.

Frame: `type(1) | stream_id(4 BE) | len(4 BE) | payload`, where type is
`1 OPEN "host:port"`, `2 DATA`, `3 CLOSE`, `4 OPEN_OK`, `5 OPEN_ERR reason`.

A new session id is minted per down-connection. When the host caps the
request (`RUN_CAP`, ~140 s) the streams end and the client reconnects; MTProto
and short HTTPS tolerate it.

## Setup

1. Upload `phpbox.php` next to a domain on the host.
2. Set `PHPBOX_TOKEN` in the environment (or edit the constant) to a secret.
3. Point the client's phpbox transport at the file's URL with that token.

Guards: a shared token, only ports 80/443, and no private/loopback targets.

## Limits (measured on free hosting)

- No binary, no daemon, no listening socket: the exit exists only for the life
  of each request. `RUN_CAP` ≈ 140 s per down-poll, then a reconnect.
- Outbound is filtered to `:80`/`:443`; other ports are refused.
- Throughput is modest (HTTP framing + the host's CPU/hit limits). Treat it as
  a free backup channel, not a fast path.

## v0 caveat — no encryption yet

v0 carries frames in the clear, so the host and anything on-path see the
destinations and traffic (the app's own TLS/MTProto still protects content).
Wrap the carrier with the core's encryption before any real use.

## cupsexit.php — the cups path (for the censored client)

When the client can only reach cups.online (not the PHP host directly), use
`cupsexit.php` instead of `phpbox.php`. It joins the **same cups room** as the
client and relays the stream mux there:

```
client (--mode=stream, --transport=cupsonline, --url <room>)
      ->  cups.online room  ->  cupsexit.php  ->  dst
```

`cupsexit.php` is a cups participant (same auth + Centrifuge WS + cursor
encoding as `transport/cupsonline`) plus the mux demux and `fsockopen(dst)`.
It carries no IP packets and runs no gVisor.

Run (open in a browser so the host's bot check passes; keep re-opening or
point a pinger at it — one request serves ~140 s):

```
https://<host>/cupsexit.php?k=PHPBOX_TOKEN&url=<full cups room URL>
```

Offline framing self-test (no network): `php cupsexit.php selftest`.

## mailruexit.php — the Mail.ru path

`mailruexit.php` is the sibling of `cupsexit.php` for Mail.ru's cloud document
editor (docs.datacloudmail.ru), matching `transport/mailru`: one WebSocket
message carries exactly one packet (one mux frame), base64 in the "cursor"
field, no extra length framing. Same guards and shape as cups.

```
client (--mode=stream, --transport=mailru, --url <public doc link>)
      ->  Mail.ru doc  ->  mailruexit.php  ->  dst
```

Run: `https://<host>/mailruexit.php?k=PHPBOX_TOKEN&url=<public doc link>`
Offline self-test: `php mailruexit.php selftest`.

## Shared code (`lib/`)

Both exits share `lib/`: `mux.php` (the carrier-agnostic stream mux — the
`Carrier` interface, OPEN/DATA/CLOSE demux, dst dialing with the port/private
guards, the select loop), `ws.php` (a minimal WebSocket client), and
`util.php` (HTTP with a cookie jar, HTML scraping, the private-target guard).
Each exit implements one `Carrier`: `CupsCarrier` (Centrifuge + cursor
integers + 2-byte chunk/packet framing) and `MailruCarrier` (Socket.IO +
base64 in the cursor field). Adding another editor = one more `Carrier`.

## The page (v0.3)

Open an exit's URL in a browser and you get a status page, not a wall of
text: the node's state (running / run number / time left / streams / bytes),
a live log with `info` and `debug` lines, Start and Stop, and a link box.

- **Already running?** The running node writes a heartbeat every second (and
  holds a lock). Opening the page again, or a pinger hitting the URL, attaches
  to it and says so; it never starts a second node on the same target.
- **Who gets what.** A browser navigation gets the page, which starts the node
  unless one already runs (`&auto=0` to only look). Anything else (a pinger,
  `curl`) runs the node directly, exactly as before; `&a=run` forces it,
  `&headless=1` forces it from a browser. Other actions: `a=status`, `a=log`,
  `a=stop` (JSON).
- **Links.** Paste an `openflux://` link into the box: it is parsed in the
  browser by the core's own parser (`share`, compiled to WebAssembly), shown
  field by field with the reason codes worded by the page, and drawn as a QR.
  The secret stays in the browser. The page picks the transport that fits
  this exit (`cupsonline` or `mailru`) and starts the node on it. Raw room and
  document addresses still work, and so does the old `?url=` / `?room=`.
- **Debug log.** Kept on the host in a ring file (trimmed when full).
  Destination hosts are hidden (`*:443`) like the core's `--sensitive`; tick
  the box on the page, or add `&sensitive=1`, to log them.

Build and upload:

```bash
deploy/phpbox/build-bundle.sh      # -> phpbox_bundle/ (token in config.php; needs Go for the parser)
./upload_bundle.sh                 # asks for the FTP password; nothing is stored
```

`NO_WASM=1` skips the parser build: addresses work, `openflux://` links then
show a note instead of a parse. Tests: `php test/mux_test.php`,
`test/node_test.sh`, `php cupsexit.php selftest`, `php mailruexit.php selftest`.

Free-host facts this code relies on (measured on InfinityFree): `sleep`,
`set_time_limit`, `putenv`, `proc_open` and `socket_*` are disabled, so the
code uses `usleep`, `stream_select` and `define()`; the 60 s limit counts CPU
time, not wall time.

## Keeping the tunnel up (chain mode, v0.4)

A request lives only a few minutes on these hosts (the 60 s limit counts CPU
time, so a waiting node is nearly free, but the host ends the request after a
while). With `&chain=1` (the page's "keep the tunnel up continuously" box, on
by default) a node starts its successor before it ends:

1. at `cap - 60 s` (cap 240 s) the running generation asks its own host, which
   lets the request through the bot check, to start generation N+1;
2. N+1 joins the room, reports "serving", and takes every new stream;
3. N stops taking new streams ("draining"), keeps serving the ones it has
   (they end by themselves, or at its cap), and exits when none are left.

While both are up a new stream goes to whichever generation creates its marker
directory first (`mkdir` is atomic), so no stream is ever served twice. A stop
(`Stop` on the page or `&a=stop`) leaves a marker that ends every generation
and keeps the dying one from spawning another. A pinger that hits the URL
while the chain is alive just attaches ("already running"); if the chain ever
breaks (the host refused the self-request) the next open or ping starts it again.

Streams that are still open when the last generation of their chain ends are
cut with a CLOSE, and the client reconnects; MTProto and short HTTPS tolerate it.

## Client logs

`--mode=stream` obeys the same levels as the packet modes: `-d` prints one line
per mux frame (`[STREAM] -> 526 bytes - stream 7 DATA`, `OPEN host:443`,
`<- ... OPEN_OK`), `-dd` adds the operational lines, `-ddd` hexdumps DATA
payloads. On the PHP side the page's log has `info` and `debug` lines;
destinations are hidden (`*:443`) unless `&sensitive=1`.

## Tuning (query parameters of a run)

`&cap=` seconds one generation lives (30-900), `&chunk=` bytes read from a
destination per frame (2048-262144, default 65536; bigger frames cost fewer carrier messages),
`&win=` flow-control window per stream in bytes (default 262144, 2x that over all streams; 0 = never honour a client's request for windows), `&idle=` seconds before an idle stream is closed (0 = never), `&sensitive=1`,
`&chain=1`.

## Flow control

A saturated carrier (a document server that takes a few hundred KB/s) queues
whatever it is given, without limit, and every frame waits behind it: four
parallel downloads made new TLS handshakes starve for minutes. So a client that
understands acks says so in OPEN (`host:port` + `\0fc`), the exit answers
OPEN_OK `fc`, and from then on each side keeps at most a window (256 KB per
stream, 512 KB over all) of DATA unacknowledged; the other side acks what it
consumed (ACK frame 6, payload = running total, uint32). Measured on a
simulated shared queue with four bulk downloads running: a handshake-sized
round trip takes 0.5 s with windows and never completes within 6 s without.
Old clients and old exits never say `fc`, and then nothing changes.
