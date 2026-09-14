# Usage

How to run an exit node and point a client at it. See [README](README.md) for
build instructions.

```
Client (iOS app / SOCKS5) ──▶ Transport (Yandex.Docs) ──▶ Exit node ──▶ Internet
```

The client and the exit node talk **through a shared Yandex document**: the
client writes tunnel data into the document's editor channel, the exit node
reads it, forwards it to the real Internet, and writes the replies back.

## 1. Prepare a document

1. Create a document on Yandex.Docs / Yandex.Disk (a collaborative editor
   document, not a plain file).
2. Make it **publicly accessible with edit rights** (anyone with the link can
   edit) and copy the share link, e.g. `https://disk.yandex.ru/i/XXXXXXXXXXXX`.
3. Match the document type to the transport:
   - `yandex` (classic) — a standard collaborative document.
   - `volga` / `vyandex` — a document served through the office-online editor.

The same link is used on **both** the exit node and the client.

## 2. Run the exit node

The exit node needs root (raw sockets) and a Linux host with a public IP.

```bash
sudo ./universal-bypass-tool \
  --exit-node \
  --transport yandex \
  --url "https://disk.yandex.ru/i/XXXXXXXXXXXX"
```

The exit node forwards raw traffic, so the kernel's own RST/ICMP replies would
tear down tunneled connections. Scope the drop rules to a dedicated egress IP:

```bash
# assign an alias IP, then run the node with --local-ip <ip> and:
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <ip> -j DROP
sudo iptables -A OUTPUT -p icmp --icmp-type port-unreachable -s <ip> -j DROP
```

A healthy start prints `OPENFLUX_READY transport=… mode=exit-node` and, per
document, `[YDOCS] WebSocket connected …`.

## 3. Configure the client

### iOS app

1. **Transport** — pick the same transport the exit node runs (`yandex` or
   `volga`). They are not interchangeable.
2. **Document URL** — paste the same link the exit node uses.
3. Connect.

Instead of typing the fields by hand you can paste a single **`OFLUX1:`**
string (a compact base64url bundle of the transport type and document URL) and
tap **Import** — it fills in the fields for you.

Optional: choose a **DNS-over-TLS** resolver, and toggle **UDP** (QUIC/HTTP3)
forwarding.

### Desktop (SOCKS5)

```bash
./universal-bypass-tool \
  --client \
  --transport yandex \
  --url "https://disk.yandex.ru/i/XXXXXXXXXXXX" \
  --socks5 127.0.0.1:1080
```

Then point your application at the SOCKS5 proxy on `127.0.0.1:1080`.

## 4. Multiple documents (throughput / failover)

`--url` accepts a **comma-separated list** of documents. The client stripes
flows across them (one TCP flow stays on one document, in order) and keeps
working if one document's channel drops.

```bash
--url "https://disk.yandex.ru/i/AAAA,https://disk.yandex.ru/i/BBBB"
```

In the iOS app, paste the same comma-separated list (no spaces) in the document
field.

## Rules that must hold (client ↔ exit node)

These are the common causes of a stuck **`connecting`** state:

| Rule | Why |
|------|-----|
| **Same document list** on both sides — exact same URLs, same order. | With a multi-document exit node, reply traffic is spread across all documents; a client listening on fewer documents never receives the replies routed to the ones it is missing. |
| **Same transport** on both sides. | `yandex` and `volga` use different document channels and are not wire-compatible. |
| **Same build version** on both sides. | The wire codec is symmetric; a client and exit node on different versions can produce incompatible frames. |

If the client is stuck on `connecting`, check those three first, then confirm
the exit node log shows `WebSocket connected` for every document.

## Advanced: batch tuning

Outgoing packets are coalesced into one compressed frame per channel message.
The batch size can be tuned at runtime via environment variables (defaults in
parentheses):

| Variable | Meaning | Default |
|----------|---------|---------|
| `OPENFLUX_BATCH_BYTES` | max bytes per batch | `8192` |
| `OPENFLUX_BATCH_COUNT` | max packets per batch | `64` |
| `OPENFLUX_BATCH_LINGER_MS` | how long to wait for stragglers | `5` |

Larger batches cut the message count further but add latency; match them to the
channel's per-message limits.
