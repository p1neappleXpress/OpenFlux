# CLI flag reference

For the story behind a group of flags, see
[transports.md](transports.md), [sessions.md](sessions.md),
[links.md](links.md), [logging.md](logging.md),
[node-provisioning.md](node-provisioning.md) and
[scripted-transports.md](scripted-transports.md). This page is the flat
lookup table. `./openflux -h` prints the same information, grouped and
with inline examples.

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--role` | `-r` | `client` | `client` \| `exit` \| `bench-send` \| `bench-sink` |
| `--inbound` | `-i` | (platform) | `tun` (macOS/Windows via Wintun) \| `socks5` |
| `--transport` | `-t` | `yandex` | `yandex` \| `vyandex` \| `boards` \| `oneme` \| `cupsonline` \| `mailru` \| `direct` \| `script` |
| `--mode` | `-m` | `l3` | Exit-node mode: `l3` \| `l4`; client: `stream` (the mode without a server, with `--transport=cupsonline\|mailru`, `--inbound=tun` for the full tunnel - see [phpbox.md](phpbox.md)) |
| `--codec` | `-c` | `batched` | `batched` \| `legacy` |
| `--url` | `-u` | `http://#` | Document URL |
| `--socks5` | `-s` | `:1080` | SOCKS5 listen address |
| `--http-proxy` | | | Also serve an HTTP proxy (CONNECT + plain requests) on this address |
| `--local-ip` | `-l` | (auto) | Egress IP for l3 SNAT / RST filter |
| `--upstream-proxy` | | | Exit (l4 only): HTTP CONNECT proxy for outbound TCP, e.g. `http://127.0.0.1:10808` (or `http://user:pass@host:port`). Empty = dial directly. UDP always dials directly |
| `--debug` | `-d`, `-dd`, `-ddd` | `0` | `1`: one line per packet; `2`: plus operational logs; `3`: plus hexdumps |
| `--sensitive` | | `false` | Also log key material and, with `-ddd`, plaintext frames (cookie jars, tokens) |
| `--encryption-key-file` | | | AES-256-GCM shared secret file |
| `--session-context` | | (derived) | KDF context for the key; default `--url`, else the highest-priority transport URL, else `http://#` |
| `--maxToken` | | | MAX auth token (`--transport=oneme`) |
| `--maxUid` | | | MAX user id (`--transport=oneme`) |
| `--bench-bytes` | | `0` | MB to push (`--role=bench-send`) |
| `--bench-compressible` | | `false` | Use compressible payload (bench) |
| `--negotiate` | | `false` | Authenticated session (both peers) |
| `--max-packet-size` | | `65000` | Largest IPv4 packet in a session (1280..65000) |
| `--transports` | | | Session transports with priorities, e.g. `direct:100,yandex:50` |
| `--direct-dial` | | | Exit address for `direct` (client) |
| `--direct-listen` | | | Listen address for `direct` (exit) |
| `--yandex-url`, `--vyandex-url`, `--boards-url`, `--mailru-url`, `--cupsonline-url` | | | Per-type document URL in a session |
| `--yandex-cookies-file` | | | Netscape `cookies.txt` with a Yandex login, for `vyandex` |
| `--oneme-token`, `--oneme-uid` | | | MAX credentials in a session |
| `--script-path` | | | Script transport: path to `<name>.flux` or `<name>.js` (needs `<name>.js.sig` beside a bare `.js`). Also used with `--transport=script` |
| `--script-pubkey` | | | Script transport: author's ed25519 public key (hex) |
| `--script-name` | | | Script transport: carrier name (default: the name the script's own `info()` reports) |
| `--config` | | | `.conf` file; flags override it |
| `--cookie-store` | | `./cookies-<transport>.json` | Cookie jar file |
| `--ipc-socket` | | | Unix socket for the app (captcha requests, cookies) |
| `--share` | | `false` | Exit: print an `openflux://` link and QR code for clients |
| `--share-host` | | (first public IPv4) | Exit: address clients dial for `direct` in that link |
| `--node-wizard` | | | Sole argument: run the JSON-over-stdio provisioning protocol instead of normal CLI startup - see [node-provisioning.md](node-provisioning.md) |
| `--inspect-script` | | | `--inspect-script --data=<path> [--sig=<path>] [--pubkey=<hex>]`: verify and read a downloaded script transport, print a JSON trust report - see [scripted-transports.md](scripted-transports.md) |
| `--parse-link` | | | `--parse-link <link\|->`: read an openflux:// link (`-`: from stdin) and print `{"config","context"}` or `{"error","code","param"}` as JSON |
| `--make-link` | | | `--make-link <json\|->`: build the link for a share configuration (`-`: from stdin) and print `{"link","config","context"}` or the error |

Deprecated (kept for one release, mapped automatically to the new flags):
`--client`, `--exit-node`, `--tun`, `--socks5-mode`, `--legacy`,
`--bench-send`, `--bench-sink`.

`--node-wizard` and `--inspect-script` are standalone entry points, checked
before normal flag parsing: `--node-wizard` must be the *only* argument;
`--inspect-script` must be the *first* argument, followed by its own
`--data`/`--sig`/`--pubkey` flags. Either way, none of the flags above
apply in that process invocation.
