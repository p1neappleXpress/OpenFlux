# iOS transports and packet-tunnel lifecycle

The app and System VPN share transport (`yandex`, `vyandex`, `oneme`), codec
(`batched` by default, `legacy` opt-in), URL and optional encryption settings.
An empty encryption field disables encryption. A nonempty secret needs at least
16 Unicode scalar values; leading/trailing whitespace is removed as in the CLI.
Use an independently generated high-entropy secret and exactly the same document
URL string on both peers. Never put production URLs or credentials in bug reports.

## Wire compatibility

`internal/transportstack` is the only production wrapper builder used by CLI,
SOCKS5 iOS and packet-tunnel iOS. It deliberately preserves the **actual** main
CLI format (including the order previously contradicted by its comment):

```
send:    IP packet -> directional AES-256-GCM (optional) -> codec -> raw carrier
receive: raw carrier -> codec -> AES authentication/decryption -> IP packet
```

Moving compression ahead of encryption would improve compression, but would
break existing encrypted peers. Such a change requires a separately versioned
protocol. This PR does not make that change. Mobile batching/zstd resource
settings change compression choices, not decoder compatibility. The KDF remains
scrypt N=32768, r=8, p=1, the same context salt, directional keys, OFX v1 envelope,
random nonces and replay window. Wrong secrets or contexts fail authentication;
codec mismatch drops frames rather than forwarding undecodable bytes.

V1 `OpenFluxStartClient` and `OpenFluxStartPacketTunnel` keep legacy/no-encryption
behavior. Their V2 counterparts add codec/secret arguments. Packet-tunnel status
has its own `OpenFluxPacketTunnelIsConnected`, independent of standalone SOCKS5.

## Keychain and the encryption startup peak

The existing scrypt KDF needs approximately **32 MiB of scratch memory**. Running
it in an extension defeats a small relay preset. The containing app therefore
calls `OpenFluxDeriveEncryptionKey`, and writes an immutable credential snapshot
(derived v1 master key and MAX token) to shared Keychain. A non-secret random
record ID, transport, URL, codec and an encryption-enabled flag go in
`providerConfiguration`. The extension checks that snapshot's transport/URL and
encryption flag, then calls `OpenFluxStartPacketTunnelWithKeyV2`. Both paths use
the same `EncryptedTransport` implementation; no new KDF or wire format exists.

The original editable secret and MAX token are also held only in Keychain.
The old MAX UserDefaults value is migrated and removed after successful storage.
There is **no plaintext or UserDefaults fallback** if shared Keychain fails. A
missing/locked/mismatched record fails startup, never silently disables encryption.
The explicit secret-based packet V2 API remains available to embedders, but incurs
the original KDF peak; it is not the production Swift extension path.

Both targets include `Shared/SecretStore.swift`, the same `keychain-access-groups`
entitlement and expanded `OpenFluxKeychainGroup` Info.plist value. Set
`DEVELOPMENT_TEAM` locally and provision both bundle IDs with Network Extension
and Keychain Sharing capabilities. If changing bundle IDs, also change the shared
group suffix consistently in `project.yml`. Do not substitute a guessed Team ID.
Items use `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`, without iCloud sync:
background access requires one unlock after reboot and secrets do not migrate to
a different device. See Apple's [Keychain accessibility documentation](https://developer.apple.com/documentation/security/ksecattraccessibleafterfirstunlockthisdeviceonly)
and [Keychain data protection](https://support.apple.com/guide/security/keychain-data-protection-secb0694df1a/web).

## Memory and reconnects

`MobileVolgaConfig` leaves desktop/server defaults intact:

| Resource | Mobile limit |
| --- | ---: |
| Relay workers | 8 |
| Relay queue | 512 entries / 512 KiB payload budget |
| Codec queue | 256 entries / 512 KiB payload budget |
| Packet output queue | 256 entries / 512 KiB payload budget |
| Relay active connections per host | 8 |
| Relay idle connections per host / total | 8 / 16 |
| WebSocket read/write buffers | 4 KiB each |
| Incoming WebSocket message limit | 2 MiB |
| Authorization response limit | 2 MiB |
| Concurrent device DNS queries | 4 |
| Go soft memory limit in extension | 24 MiB |

Mobile relay encoding avoids the server's 16 MiB base64 pool; its zstd encoder
uses a 64 KiB window. Empty queues reserve kilobytes, not tens of megabytes.
Benchmarks cover startup of a prepared-key stack/session and idle relay workers
without network access. They **do not measure total iOS phys_footprint**, TLS
handshake peaks, WebRTC/MAX memory, or sustained load. Go's soft memory limit is
not an RSS guarantee. Device profiling remains mandatory before release.

The WebSocket listener updates its real connected state and reconnects in place.
The provider polls that state and sets `reasserting`; it does not cancel the VPN,
spawn loops or create new transports for a relay outage. Explicit stop cancels
reads, DNS requests, relay workers and the socket. A generation guard and a joined
write loop prevent old provider callbacks/readers leaking into a later start.
This addresses the lifecycle aspects of [issue #36](https://github.com/p1neappleXpress/OpenFlux/issues/36),
not a claim that iOS can never terminate an extension for resource pressure.

`OpenFluxTunReadPacket` returns `>0` for a whole packet, `0` for transient/no data
(bounded blocking, no spin), and `-1` only after explicit stop. Swift uses a 65535
byte buffer and treats those values separately. An undersized buffer drops/counts
the packet without copying a truncated IP packet. The existing codec/relay has
16-bit record lengths; the configured MTU is 1500, safely below those framing
limits even with encryption overhead. This PR adds no jumbo-packet fragmentation.

## DNS and routing

Yandex DoT (`77.88.8.8`, `common.dot.dns.yandex.net`) remains first; Google and
Cloudflare are fallbacks. Before installing default/DNS routes, the bridge builds
an immutable bootstrap plan for the document/control endpoints. Each DoT lookup
has a bounded timeout; at most two lookups run concurrently. For the exact Yandex
bootstrap hosts (`disk`, `docs`, `docviewer`, `volga`, `push` under `yandex.ru`),
failed DoT may fall back to the native system resolver **before VPN activation**.
This is a carrier bootstrap operation, not a fallback for arbitrary user traffic.
MAX retains strict DoT-only startup and requires all endpoints to resolve.

Successful IPv4 answers become /32 exclusions and are cached for the carrier, so
Volga authorization/relay/WebSocket connections do not require another successful
DoT query after the TUN starts. A failed individual known Yandex host may rely on
the existing Yandex CIDRs. Unknown/uncovered hosts fail setup without exposing
their names or URLs. A hostname suffix alone never grants prefix fallback.

The CIDRs previously in Swift are preserved verbatim in Go. The V2 bridge returns
the full route snapshot used by both Swift network settings and a **Yandex-only
carrier dial guard**. It dials numeric addresses while HTTP Host/TLS SNI and
certificate verification retain the hostname. Every actual IP must fall inside
an installed /32 or CIDR; unresolved names are not assumed to be proof of prefix
coverage. Reconnects can reuse bootstrap answers or try DoT again, but never call
system DNS or dial an uncovered new address. A backend outside the snapshot
fails safely until a new pre-VPN bootstrap, rather than looping into packetFlow.
Device DNS and SOCKS destination resolution retain their original DoT-only policy.

No guessed IP or new provider prefix is introduced. The prefix list is inherited
from the previous provider, not a claim of provider-published or permanent
ownership. Apple's route controls are documented in
[NEPacketTunnelProvider](https://developer.apple.com/documentation/networkextension/nepackettunnelprovider).

The transport may discover additional balancer/signaling/ICE endpoints during
authorization. Uncached Yandex endpoints still need secure resolution and a
covered address; this change does not promise offline DNS for arbitrary future
backends. Check actual socket destinations and packetFlow on a device, particularly
after DNS changes and for MAX's dynamic ICE, which is not given Yandex's fallback.
With PR #80's UDP-capable exit, System VPN can forward non-DNS IPv4 UDP when
"Forward UDP" is enabled in the app. The setting defaults to off and is stored
in the VPN profile; profiles created before this change stay off. UDP port 53
continues to use local DNS-over-TLS. The app does not auto-detect exit support:
enabling this with an older TCP-only exit can stall UDP applications. Disable it
to get the ICMP error/TCP fallback path. This change does not add IPv6,
fragmented IPv4 UDP, ICMP/PMTU forwarding, or an all-protocol leak-proof VPN.

## Validation and device checklist

Local commands:

```
go test ./...
go vet ./...
go build ./...
go test -race ./internal/... ./transport/...
go test ./internal/transportstack ./internal/packettunnel ./transport/yandex -run '^$' -bench Startup -benchmem
```

The workflow also compiles the Go iOS archive and both Swift targets on macOS
without signing. Locally on a Mac: run `./build_ios.sh`, copy the generated `.a`
and `.h` to `ios-app/Lib`, run `xcodegen generate`, and use `xcodebuild` with
`CODE_SIGNING_ALLOWED=NO` for a device compile. No TestFlight/deployment action is
part of this workflow. An unsigned compile does not verify shared-Keychain
entitlements on a provisioned device.

Before TestFlight upload, CURRENT_PROJECT_VERSION must be incremented from the already-used build number.
The maintainer selects that next build number. This change performs no signing
or upload and does not alter `CURRENT_PROJECT_VERSION`.

Before release, use disposable test documents and keys with a matching upstream
exit binary (never a working production VPS):

1. Test all three transports and both codecs, with encryption on/off. Capture
   provider logs with verbose logging enabled and confirm URLs/keys/tokens absent.
2. Test wrong secret/codec, empty secret, Keychain failure, and profile migration.
3. Interrupt connectivity, restore it, change Wi-Fi/cellular, lock/unlock, sleep,
   and stop/start repeatedly. Confirm one read/write pair and `reasserting` recovery.
4. Confirm transport and DoT sockets bypass packetFlow before/after reconnect;
   inspect changing Volga/push/balancer addresses and MAX ICE destinations.
   Block TCP/853 while Yandex HTTPS remains available: verify native bootstrap
   DNS, cached carrier dials, partial endpoint failure and safe rejection of an
   uncovered backend. Verify ordinary destination DNS never uses system fallback.
5. Measure `phys_footprint` at cold start, TLS handshake and sustained transfer;
   inspect Jetsam/EXC_RESOURCE reports. Include IPv4 packets above 4096 bytes.
6. Verify actual signed app/extension Keychain sharing, first-unlock behavior,
   device and simulator builds. No Apple credentials belong in this repository.
7. With a PR #80 exit, enable Forward UDP and test a UDP echo and QUIC service
   over Wi-Fi and cellular. Turn it off and verify ICMP/TCP fallback; repeat
   against an older TCP-only exit. Confirm UDP/53 still uses local DoT.
