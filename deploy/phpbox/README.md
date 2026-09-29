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
