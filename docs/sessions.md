# Sessions: multi-transport, encryption, negotiation

## Multi-transport sessions

Run several transports in one negotiated session, for example a direct TCP
connection to the exit plus a Yandex document as the fallback:

```bash
# Exit: direct listener on :8445 plus the document
./openflux --role=exit --mode=l3 --negotiate \
    --transports=direct:100,yandex:50 --direct-listen=0.0.0.0:8445 \
    --encryption-key-file=secret.txt --url="YOUR_YANDEX_DOC_URL"

# Client
./openflux --role=client --inbound=socks5 --negotiate \
    --transports=direct:100,yandex:50 --direct-dial=EXIT_IP:8445 \
    --encryption-key-file=secret.txt --url="YOUR_YANDEX_DOC_URL"
```

- Every transport starts at once. One that fails to start (for example on a
  captcha) is retried in the background with backoff.
- Priority is the failover order: traffic uses the highest-priority transport
  that reaches the peer. Transports with equal priority share flows.
- A transport counts as working only while the peer is heard on it (quiet ones
  are pinged), not merely while it is attached to its document. Peers that
  predate this keep the old behavior.
- Transports are named after their type. Per-type document URLs:
  `--yandex-url`, `--vyandex-url`, `--boards-url`, `--mailru-url`,
  `--cupsonline-url`; MAX takes `--oneme-token` / `--oneme-uid`. A script
  transport takes `--script-path` / `--script-pubkey` / `--script-name` -
  see [scripted-transports.md](scripted-transports.md). `--url` is used for
  `yandex` when `--yandex-url` is empty.
- `--url` is also the encryption context: give both peers the same `--url`.
- `direct` needs the exit's port reachable from the client (open it in the
  firewall); it is only available in a session.

### As a `.conf` file

```ini
[Interface]
Role = client
Inbound = socks5
EncryptionKeyFile = secret.txt
URL = YOUR_YANDEX_DOC_URL

[Transport "direct"]
Priority = 100
Dial = EXIT_IP:8445

[Transport "yandex"]
Priority = 50
URL = YOUR_YANDEX_DOC_URL
```

`./openflux --config=client.conf`; flags on the command line override the
file. `[Interface]` keys: `Role`, `Inbound`, `Transport`, `Mode`, `Codec`,
`Socks5`, `EncryptionKeyFile`, `CookieStore`, `IPCSocket`, `URL`, `Debug`,
`UpstreamProxy` (exit, l4 only - see [architecture.md](architecture.md)).
Transport sections take `Type` (defaults to the section name), `Priority`
(default 50), `URL`, `Dial` / `Listen` (direct) and `Token` / `UID` (MAX). A
`.conf` with transport sections always runs as a negotiated session.

## Authenticated capability negotiation (opt-in)

Add these options on **both** updated peers, using the same secret and codec:

```bash
--codec=batched --encryption-key-file=/path/to/secret.txt --negotiate
```

The handshake runs inside AES-GCM and confirms fresh random challenges, peer
roles, IPv4/TCP/UDP support, ICMP-error support and maximum IPv4 packet size.
L4 does not advertise raw ICMP forwarding. Only the intersection of
capabilities is enabled. Data carries both session IDs and a sequence
number; a 4096-packet sliding replay window tolerates bounded reordering.
This is not forward secrecy or a replacement for a future key-exchange/rekey
design.

Negotiated mode never falls back to unencrypted or legacy peers. The client
gives up after 20 seconds if negotiation cannot complete (wrong key,
incompatible codec, missing option, or unavailable peer); the exit waits for
a client indefinitely. `--max-packet-size=1280..65000` caps the complete
IPv4 packet; the default is 65000, leaving room for authenticated envelopes.
The agreed limit is used by the gVisor link; the macOS TUN remains 1280.
Raw-exit replies exceeding the agreed limit are fragmented without DF, or
produce ICMP feedback to the Internet sender with DF.

Carrier reconnects keep the session. A restarted peer is accepted again
without restarting the other one: a hello from an unknown sender gets a
challenge minted for it alone, and the session is replaced only once that
challenge is echoed, so old traffic replayed from a carrier (anyone with
access to a document sees the ciphertext) cannot displace it. An exit
therefore serves one active client at a time. A client whose exit went
silent on every transport starts a new handshake on its own, within about a
minute. Existing iOS builds have no negotiation setting and must use an
exit without `--negotiate`. Their UDP switch remains manual. No claim of
device-level QUIC validation is made.

Classic (pre-Session) peers still interoperate - a classic setup with a key
(`--transport=X`, the apps' classic profiles) runs a Session and speaks
classic to an exit that does not answer the handshake, switching once it
does; a classic-configured exit serves both classic and Session clients.
`--negotiate` stays strict: exits configured as a Session (wizard, `.conf`,
`--transports`) serve Session clients only.

Full wire-level framing (envelope layout, state machine, codec framing,
encryption-context derivation, classic-vs-Session byte-for-byte layering):
[../PROTOCOL_NEGOTIATION.md](../PROTOCOL_NEGOTIATION.md).

## Encryption (optional, single-transport mode)

```bash
./openflux ... --encryption-key-file=/path/to/secret.txt
```

Both peers must use the same secret file. AES-256-GCM, directional keys.
Unset means unencrypted, unchanged behavior. `--session-context=<str>` sets
an explicit KDF context when neither peer's `--url` should be used as one;
both peers must agree on it.

## Captchas inside a session

See [transports.md](transports.md#captchas) - captcha/login signals ride
the session's control channel exactly like a single-transport setup, just
relayed over whichever transport still works.

## Sharing a session as a link

See [links.md](links.md).
