# OpenFlux

OpenFlux is an experimental TCP tunnel. The current MVP exposes a local SOCKS5 proxy, carries each SOCKS5 `CONNECT` over authenticated WSS/TLS to a controlled exit node, and opens an ordinary outbound TCP connection from that exit.

The goal of this milestone is a small, testable transport—not a polished application.

## Supported MVP path

```text
Application
  -> SOCKS5 at 127.0.0.1:1080
  -> one authenticated WSS connection per SOCKS CONNECT
  -> OpenFlux exit on a controlled VPS
  -> ordinary outbound TCP connection
```

Implemented and tested:

- SOCKS5 `CONNECT` with no authentication;
- TCP streams only;
- IPv4 destinations and domain names;
- DNS resolution on the exit node;
- TLS certificate verification, with an optional private CA;
- bearer-token authentication and WebSocket subprotocol `openflux.v1`;
- optional HTTP or SOCKS5 bootstrap proxy for the WSS connection;
- clean shutdown of listeners and active tunnels;
- destination-policy enforcement before the exit dials.

Current limits:

- no UDP or SOCKS5 `BIND`;
- no IPv6 destinations;
- no multiplexing: every SOCKS connection creates one WSS connection;
- no TUN support in the supported WSS route;
- no inbound port forwarding;
- only the first enabled route is selected;
- no systemd deployment instructions yet.

> A working WSS tunnel does **not** prove that it works through a Russian carrier whitelist. Operator, tariff, region, and endpoint behavior must be tested separately, using an officially permitted carrier. V2rayN may be used as a controlled bootstrap path, but success through it is not proof of independent reachability.

## Build

Use the Go version declared in [`go.mod`](go.mod):

```bash
go build -o openflux .
```

The root module uses the local nested module in [`fluxcore`](fluxcore/), so build from the repository root.

## Configuration

OpenFlux loads JSON with `--config`, expands environment variables, validates the complete configuration, and starts the first enabled route.

Two safe templates are included:

- [`flux.example.json`](flux.example.json) — client, with SOCKS5 on localhost;
- [`flux.exit.example.json`](flux.exit.example.json) — exit node, listening only on VPS localhost for the initial SSH-forwarded smoke test.

Copy them to local, untracked files before editing:

```bash
cp flux.example.json flux.client.json
```

```bash
cp flux.exit.example.json flux.exit.json
```

The example token is an environment reference:

```json
"authToken": "${OPENFLUX_AUTH_TOKEN}"
```

Provision the **same** random token on the client and exit. It must contain at least 32 non-whitespace characters; 32 random bytes encoded as 64 hexadecimal characters is a suitable choice. Keep it out of source control, chat, screenshots, process arguments, and logs. If `OPENFLUX_AUTH_TOKEN` is unset, validation fails instead of starting without authentication.

### TLS files

The exit requires:

- `certFile`: server certificate, optionally followed by its chain;
- `keyFile`: matching private key, readable only by the exit service account.

The certificate must contain the verification name in its Subject Alternative Name. The client either uses normal system trust or supplies the issuing CA through `caFile`. `serverName` is an optional TLS verification-name override; it does not disable certificate verification.

Relative `caFile`, `certFile`, and `keyFile` paths are resolved relative to the JSON configuration file.

There is deliberately no insecure certificate-skip option.

## Initial foreground smoke test

This procedure keeps the WSS port private and does not require firewall, routing, NAT, DNS, raw-socket, or systemd changes.

### 1. Prepare the exit

Place the exit configuration and TLS certificate/key on the VPS. Keep the example:

```json
"listenAddr": "127.0.0.1:8443"
```

Replace the documentation address in `denyCIDRs` with the VPS public IPv4 address as a `/32`. The exit also blocks addresses assigned to its local interfaces, but the explicit `/32` documents the intended boundary.

Set `OPENFLUX_AUTH_TOKEN` through a protected environment mechanism, then run in the foreground:

```bash
./openflux --config flux.exit.json
```

Expected startup output includes the selected `wss` route and `WSS exit listening on 127.0.0.1:8443`. The token and private-key contents must never appear in output.

### 2. Create an SSH local forward

Using the already-authorized SSH connection to the VPS, forward a client-side port to the exit listener:

```bash
ssh -N -L 18443:127.0.0.1:8443 user@your-vps
```

If SSH itself currently uses V2rayN, keep using that known-good SSH setup. In MobaXterm, the equivalent is a local port forward from `127.0.0.1:18443` to VPS-side `127.0.0.1:8443`.

### 3. Prepare and run the client

The client template connects to the local SSH forward:

```json
"endpoint": "wss://127.0.0.1:18443/openflux/v1/tunnel"
```

Set `serverName` to the exact DNS SAN in the exit certificate and set `caFile` when using a private CA. Provision the same `OPENFLUX_AUTH_TOKEN`, then run:

```bash
./openflux --config flux.client.json
```

Expected startup output includes:

```text
SOCKS5 listening on 127.0.0.1:1080
```

### 4. Send controlled traffic

Use remote DNS through SOCKS (`--socks5-hostname`, not `--socks5`) and a controlled HTTP endpoint:

```bash
curl --socks5-hostname 127.0.0.1:1080 https://your-controlled-test-host.example/
```

Verify:

- the controlled response arrives intact;
- the destination hostname is resolved from the VPS, not the client;
- the observed public source address is the VPS address;
- loopback, private, link-local, VPS-local, and configured denied destinations fail;
- concurrent requests work;
- Ctrl+C closes listeners and active streams cleanly;
- logs contain no token, authorization header, proxy credentials, cookie, private key, or payload.

Do not add systemd or expose a public WSS port until this foreground test is understood and repeatable.

## Optional WSS bootstrap proxy

For a controlled test against a WSS endpoint that is already reachable outside the VPS, add this to the client route:

```json
"proxyURL": "socks5://127.0.0.1:10808"
```

Supported schemes are `http` and `socks5`. A SOCKS5 URL requires an explicit port. Do not point `proxyURL` at OpenFlux's own SOCKS listener (`127.0.0.1:1080`), because that would create a loop. Do not embed proxy credentials in a checked-in configuration.

## Exit destination policy

Before dialing, the exit:

1. parses the requested `host:port` strictly;
2. resolves a hostname once using IPv4 DNS on the VPS;
3. validates every returned address;
4. dials only validated literal IPv4 addresses.

The policy rejects IPv6 and blocks unspecified, loopback, private, link-local, multicast, carrier-grade NAT, benchmark, documentation, reserved, local-interface, and administrator-configured CIDR ranges. If any DNS answer is denied, the request is denied. This prevents validation/dial DNS rebinding.

## Protocol and architecture

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the connection sequence, trust boundaries, framing, policy, and shutdown behavior.

## Development verification

Run the nested configuration-module tests separately, then the root suite:

```bash
go -C fluxcore test ./...
```

```bash
go test ./...
```

Static analysis and a local build:

```bash
go vet ./...
```

```bash
go build ./...
```

A Linux/amd64 static build can be produced with:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o build/openflux-linux-amd64 .
```

Automated tests use local listeners and injected resolvers/dialers; they do not need MAX, Yandex, the public Internet, or a VPS.

## Experimental legacy paths

Yandex Docs, Yandex Volga, OneMe/MAX, CupsOnline, the packet transport abstraction, gVisor, and raw exit mode remain in the repository for research compatibility. They are **not** the supported MVP path and may require credentials, elevated privileges, or network behavior that this WSS design intentionally avoids.

The current [`Dockerfile`](Dockerfile), [`docker-compose.yml`](docker-compose.yml), [`.env.example`](.env.example), and `docker/entrypoint.sh` still describe that legacy container path. Do not use them as WSS deployment instructions. The initial WSS validation path is the foreground binary procedure above.

## License

See [`LICENSE`](LICENSE).
