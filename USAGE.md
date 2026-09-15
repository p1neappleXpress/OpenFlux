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

In the iOS app, use the **+ (Add second document)** button to enter the second
document (VOLGA is single-document only, so it has no + button).

## 5. Import config (`OFLUX1:`)

Instead of entering the transport and document URL(s) by hand, the app can
import them from a single `OFLUX1:` string — the **Import** button reads it from
the clipboard and fills in the fields.

Format:

```
OFLUX1:<base64url(JSON)>
```

where the JSON is:

```json
{"t":"yandex","u":"https://disk.yandex.ru/i/XXXXXXXXXXXX"}
```

- `t` — transport: `yandex` (classic) or `volga`. Optional (defaults to volga);
  `vyandex` is accepted as an alias for `volga`.
- `u` — document URL, or a comma-separated list for a Yandex multi-document
  setup. Required.
- The payload is standard base64 with `+`→`-`, `/`→`_`, and `=` padding removed
  (base64url); the app re-adds the padding on import.

Generate one from the shell:

```bash
echo -n '{"t":"yandex","u":"https://disk.yandex.ru/i/XXXXXXXXXXXX"}' | \
  { printf 'OFLUX1:'; base64 | tr '+/' '-_' | tr -d '='; }
```

Examples (placeholder document IDs):

```
# single Yandex document
OFLUX1:eyJ0IjoieWFuZGV4IiwidSI6Imh0dHBzOi8vZGlzay55YW5kZXgucnUvaS9YWFhYWFhYWFhYWFgifQ
# two Yandex documents
OFLUX1:eyJ0IjoieWFuZGV4IiwidSI6Imh0dHBzOi8vZGlzay55YW5kZXgucnUvaS9BQUFBLGh0dHBzOi8vZGlzay55YW5kZXgucnUvaS9CQkJCIn0
# single VOLGA document
OFLUX1:eyJ0Ijoidm9sZ2EiLCJ1IjoiaHR0cHM6Ly9kaXNrLnlhbmRleC5ydS9pL1hYWFhYWFhYWFhYWCJ9
```

## Rules that must hold (client ↔ exit node)

These are the common causes of a stuck **`connecting`** state:

| Rule | Why |
|------|-----|
| **Same document list** on both sides — exact same URLs, same order. | With a multi-document exit node, reply traffic is spread across all documents; a client listening on fewer documents never receives the replies routed to the ones it is missing. |
| **Same transport** on both sides. | `yandex` and `volga` use different document channels and are not wire-compatible. |
| **Mixed build versions are OK.** | The codec self-negotiates: a newer client and an older exit node (or vice versa) fall back to the legacy per-packet format and keep working; they upgrade to batching only when both support it. Updating one side no longer breaks the other. |

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
