# Architecture

## The path a packet takes

Any client works with either exit backend. `--mode` is chosen on the
**exit node**, not on the client.

```
Client (any):  macOS (utun) / Linux / Windows / iOS (packet tunnel) / Android
                    |
                    v
               Transport (Yandex.Docs / Volga / Board / MAX / Cups / Mail.ru / Direct / script)
                    |
                    v
               Exit node  -->  Internet
                 --mode l3   (raw SNAT/DNAT, Linux + root)
                 --mode l4   (gVisor proxy, any platform)
```

The client terminates TCP locally (gVisor, utun, or NEPacketTunnelProvider),
then sends raw IP packets into the transport. In a multi-transport session
several transports run at once and traffic fails over between them - see
[sessions.md](sessions.md).

## Exit-node backends

The exit node has exactly **two** backends, selected with `--mode` on the
**exit node**. The client does not choose a backend - the same client works
against either.

| `--mode` | Backend | Forwarding | Requires | Platforms |
|----------|---------|-----------|----------|-----------|
| `l3` | Raw L3 | SNAT/DNAT on raw IPv4 via SOCK_RAW + conntrack. No userspace TCP stack. | root / CAP_NET_RAW | Linux only |
| `l4` (alias `proxy`) | gVisor proxy | Terminates TCP/UDP in a userspace gVisor stack, then dials the real server. | nothing | Linux, macOS, Windows |

- `proxy` is a deprecated alias for `l4`; both select the same backend.
  `l4` is the canonical name going forward.
- **l3 is faster** (single end-to-end TCP connection, no double termination)
  but Linux-only and needs root.
- **l4 works everywhere** without root, at the cost of terminating TCP twice
  (client -> gVisor on exit -> real server).
- On Linux with root, prefer `l3`. On Windows, the intended path is `l3`
  inside a lightweight QEMU VM (see [Known limitations](#known-limitations))
  - the WinDivert backend is not wired yet, and `l4` is the working
  fallback until then. On non-root hosts, use `l4`.

### l4 upstream HTTP CONNECT proxy

An `l4` exit dials outbound TCP through one injectable dialer
(`tunnel.DialFunc`, default `tunnel.DirectDial`). With
`--upstream-proxy=http://127.0.0.1:10808` (or `UpstreamProxy` in the
`.conf` `[Interface]` section) that dialer becomes an HTTP CONNECT client
(`tunnel/upstream.go`): every outbound TCP connection is established with
`CONNECT host:port` against the proxy, and application bytes flow over the
returned `net.Conn` once the proxy answers `200`.

```bash
# Exit without upstream (old behavior, unchanged default)
./openflux --role=exit --mode=l4 --transport=boards --url="YOUR_BOARD_URL"

# Exit with upstream: TCP leaves through a local Xray/3x-ui HTTP inbound,
# whose existing VLESS outbound and routing stay configured in Xray itself
./openflux --role=exit --mode=l4 --transport=boards --url="YOUR_BOARD_URL" \
    --upstream-proxy=http://127.0.0.1:10808
```

- Only `l4` TCP is proxied. UDP (including DNS carried over UDP) keeps
  dialing directly; HTTP CONNECT cannot carry it in this implementation.
  With no `--upstream-proxy` everything dials directly, as before.
- The proxy endpoint must be reachable from the exit host. If it is down,
  refuses the CONNECT, or stays silent past the handshake timeout, the flow
  fails and the client connection closes - the exit never falls back to a
  direct dial on its own.
- Format: `http://host:port` or bare `host:port`; Basic auth as
  `http://user:pass@host:port` (sent as `Proxy-Authorization`). Other
  schemes (`socks5://`, `https://`, ...) are rejected. Credentials never
  appear in logs (the startup line prints a redacted address).
- Health check through the proxy itself, without OpenFlux:
  `curl -x http://127.0.0.1:10808 https://ifconfig.me` should return the
  address the proxy egresses from.
- In `l3` mode the flag is accepted but ignored with a warning: L3
  forwards packets (SNAT/DNAT) and never dials TCP.

### l3 and kernel RSTs

In `l3` mode the kernel sees return packets for connections it never opened
and emits RSTs, tearing the tunnel connections down. Drop them:

```
# Scoped (recommended): assign a dedicated egress IP, run with --local-ip, then:
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <egress-ip> -j DROP

# Host-wide fallback (drops ALL outbound RST; makes closed ports look filtered):
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP
```

Client-originated RSTs are forwarded normally. The rule above is only for
RSTs generated locally by the exit-node kernel.

## Client inbounds

- **macOS utun** - `--inbound=tun` (default on macOS). Creates a utun
  interface, watches its own sockets to install bypass routes, then takes
  the default route. No SOCKS5, no gVisor on the client.
- **Windows tun** - `--inbound=tun` on Windows uses a Wintun adapter (needs
  administrator and `wintun.dll` next to the binary) for the same
  full-tunnel behavior as the macOS client: the core's own sockets are
  bound to the real interface (see `netbind/`) so carriers never loop into
  the tunnel, IPv6 is routed into the adapter and dropped so programs fall
  back to IPv4, and the routes only live as long as the adapter.
- **SOCKS5** - `--inbound=socks5` (default on non-macOS platforms). Point a
  browser or app at `127.0.0.1:1080`. UDP-capable applications may use the
  SOCKS5 `UDP ASSOCIATE` command. `--http-proxy=<addr>` additionally serves
  a plain HTTP proxy (CONNECT and plain requests) through the same tunnel.
- **iOS packet tunnel** - NEPacketTunnelProvider, pure L3 forwarding.

`netbind/` is what keeps the core's *own* connections (to Yandex, to a
direct exit, etc.) off its own tunnel on Windows: every outbound socket is
bound to the interface that currently carries the default route
(`IP_UNICAST_IF`), the same role Android's VPN-exclude and iOS's extension
sandboxing play on those platforms.

## Repository layout

```
OpenFlux/
  main.go                          # CLI entry (client / exit / benches)
  conf.go                          # .conf parser
  transport_spec.go                # --transports parsing, session bootstrap
  transport_factory.go             # Builds a transport from its type
  ipc_handler.go                   # IPC: cookies from the app
  auth_proxy.go                    # Local HTTP proxy for the exit's checks
  share_cli.go                     # --share, --parse-link, --make-link
  script_cli.go                    # --inspect-script
  node_wizard.go, node_wizard_*.go # --node-wizard JSON-over-stdio protocol
  share/                           # openflux:// links and QR codes
  bench.go                         # Benchmark helpers
  tun_darwin.go, tun_windows.go    # macOS utun / Windows Wintun clients
  tun_watch.go, tun_learn.go, tun_other.go
  netbind/                         # Binds the core's own sockets off its tunnel (Windows)
  signals_{unix,windows}.go        # Shutdown signals
  transport/
    transport.go                   # Transport interface
    batched.go                     # BatchedTransport (coalescing + zstd)
    framing.go                     # Wire framing for batched frames
    compressor.go                  # Legacy per-packet LZ4 codec
    encrypted.go                   # Optional AES-256-GCM wrapper
    session.go                     # Negotiated multi-transport session
    direct.go                      # Direct TCP transport
    cookies.go, cookiestore.go     # Cookie exchange and persistence
    error_notifier.go              # Out-of-band errors (captcha, login)
    control/                       # Envelope and control messages
    manager/                       # Transports, cookies and checks per session
    ipc/                           # App <-> core IPC over a Unix socket
    yandex/                        # Yandex.Docs, Volga, Board, captcha solver
    oneme/                         # MAX Messenger backend
    cupsonline/                    # Cups.online backend
    mailru/                        # Mail.ru Docs backend
    phpbox/                        # Client side of the no-VPS PHP exit
    script/                        # Scripted (JS/goja) transport engine - see scripted-transports.md
  tunnel/
    tunnel.go                      # Client tunnel (gVisor + TunnelLinkEndpoint)
    endpoint.go                    # Virtual NIC (client)
    packettunnel.go                # Packet tunnel (iOS)
    httpproxy.go                   # HTTP proxy over a tunnel stack
    exit.go                        # NewExitNode dispatcher (l3 / l4)
    proxy_exit.go                  # L4 exit (gVisor + net.Dial)
    l3/                            # L3Exit: SNAT/DNAT, conntrack, egress filter, fragmentation, ICMP
    windivert/                     # WinDivert backend (present, not wired to L3 yet)
  socks5/                          # SOCKS5 server (client fallback)
  provision/                       # SSH node-provisioning wizard backend
    phphost/                       # No-VPS PHP-hosting deployment over FTP
  deploy/
    node-install.sh                # VDS installer the SSH wizard runs
    phpbox/                        # The no-VPS PHP exit's PHP sources
  network/                         # Checksums, packet parsing
  utils/                           # Logging
  ios-app/                         # SwiftUI iOS client (XcodeGen)
  mobile/                          # App bridge: gomobile (Android) and the iOS C library
  build_all.sh, build_ios.sh, build_ios_app.sh, build_android.sh
  scripts/
    cleanup-utun.sh                # Remove leftover utun routes (macOS)
```

## UDP limitations

- UDP is IPv4-only for now.
- L3 reassembles IPv4 fragments with a 30-second fixed lifetime, 64 incomplete
  datagrams, 128 fragments per datagram and a 4 MiB byte budget per direction.
  Overlaps and malformed fragments are discarded; expiry is swept on input.
- L3 relays checksum-validated ICMP errors only for live TCP/UDP NAT flows,
  restoring the quoted client address/port and checksums. Redirects and echo
  traffic are not relayed. Egress EMSGSIZE produces ICMP fragmentation-needed
  with the kernel route MTU; non-DF packets can instead be fragmented. Outgoing
  fragmentation of IPv4 headers containing options is not supported.
- This is ICMP-based PMTU feedback, not active DPLPMTUD probing. Networks that
  filter ICMP can still black-hole large DF packets; real-network tests remain
  necessary. The negotiated packet ceiling is distinct from the Internet MTU.
- Linux raw L3 UDP reserves a kernel-selected source port per remote endpoint
  using a real UDP socket and restores the client's port on return. This avoids
  taking ports owned by host applications and is intended to prevent kernel
  ICMP port-unreachable without firewall changes. There are at most 256 mappings;
  idle expiry is 2 minutes (15 seconds for DNS). Source-port preservation and
  endpoint-independent NAT/hole-punching are not provided.
- The isolated Linux raw-socket/ICMP test passes in GitHub Actions (`ci.yml`).
  It covers loopback inside a disposable network namespace, including
  host-port conflicts and false ICMP port-unreachable responses; it is not an
  Internet/PMTU canary. TCP's existing raw-port ownership and
  RST-suppression requirements are unchanged.
- iOS keeps the old TCP fallback for non-DNS UDP unless the app explicitly
  calls `OpenFluxTunSetUDPEnabled(1)` for a known UDP-capable exit. Reset it to
  `0` when switching to an older exit. Physical-device QUIC is not validated.
- Most document/WebSocket transports are reliable and ordered. UDP works over
  them, but packet loss in the carrier can still cause head-of-line blocking;
  this is not equivalent to a native datagram transport.

## Benchmarks

Measure raw goodput through the transport, without touching the host network:

```bash
# Sender: push 100 MB
./openflux --role=bench-send --bench-bytes=100 --transport=yandex --url="..."

# Receiver: measure goodput
./openflux --role=bench-sink --transport=yandex --url="..."
```

## Known limitations

- **L3 exit on Windows and macOS.** The L3 exit currently works on Linux
  (SOCK_RAW) only; Windows and macOS use `--mode=l4`. The `tunnel/windivert/`
  package (Windows) exists but is not wired to the L3 forwarder yet. A native
  macOS L3 exit is not implemented.
- **Run the exit node under QEMU** (for a Windows `l3` path) is planned, not built yet.
