# OpenFlux WSS MVP architecture

## Scope

The supported MVP is a controlled TCP tunnel:

```text
application
  -> SOCKS5 listener on client loopback
  -> authenticated WSS/TLS
  -> OpenFlux exit on a controlled VPS
  -> ordinary outbound TCP
```

It is deliberately narrower than the repository's earlier packet-tunnel experiments. The WSS route does not instantiate the gVisor tunnel, create raw sockets, modify host routes, configure NAT, change DNS, or install firewall rules.

## Components

### Configuration and executable

[`fluxcore/config.go`](../fluxcore/config.go) defines and validates WSS route fields. [`main.go`](../main.go) loads JSON, expands environment variables, chooses the first enabled route, and starts one of two roles:

- `mode: "client"`: a localhost SOCKS5 listener backed directly by `wss.Client`;
- `mode: "exit-node"`: a TLS-only `wss.Server`.

The executable resolves relative TLS file paths against the configuration file directory. Signal cancellation closes listeners and active streams.

### SOCKS5 front end

[`socks5/socks5.go`](../socks5/socks5.go) supports only the no-authentication method and the `CONNECT` command. It accepts IPv4 and domain targets, rejects IPv6 explicitly, and uses staged `io.ReadFull` parsing so valid fragmented requests do not depend on TCP packet boundaries.

The boundary between SOCKS5 and WSS is intentionally small:

```go
type Dialer interface {
    DialTCP(address string) (net.Conn, error)
}
```

The WSS client implements this interface. The older packet-oriented `transport.Transport` interface is not involved in the supported route.

### WSS client

[`transport/wss/client.go`](../transport/wss/client.go) creates one WSS connection per `DialTCP` call. It:

- accepts only an absolute `wss://` URL at `/openflux/v1/tunnel`;
- verifies the server certificate with system roots plus an optional CA file;
- permits an optional verification-name override through `serverName`;
- offers only WebSocket subprotocol `openflux.v1`;
- sends the shared secret only as an `Authorization: Bearer` header;
- disables WebSocket compression;
- applies finite dial, handshake, and request timeouts;
- optionally bootstraps through an HTTP or SOCKS5 proxy.

There is no `InsecureSkipVerify` path.

### WSS exit server

[`transport/wss/server.go`](../transport/wss/server.go) serves HTTP/1.1 over TLS with TLS 1.2 as the minimum. It accepts only:

- `GET /openflux/v1/tunnel` with no query;
- the exact OpenFlux WebSocket subprotocol;
- no browser `Origin` header;
- a matching bearer token.

The token is reduced to a SHA-256 digest before constant-time comparison. Authorization values are not logged. The HTTP server's internal error logger is discarded to avoid accidental request detail leakage.

After a request passes destination policy, the exit dials a validated literal address with `network = "tcp4"` and relays bytes in both directions.

### Destination policy

[`transport/wss/policy.go`](../transport/wss/policy.go) is the outbound security boundary. It parses `host:port` strictly and supports only non-zero TCP ports.

For a hostname, the exit:

1. calls `LookupNetIP(ctx, "ip4", host)` once;
2. validates every returned address;
3. removes duplicates;
4. returns literal `IPv4:port` dial targets;
5. never resolves the original hostname again while dialing.

This avoids a validation/dial DNS-rebinding gap. If any answer is not allowed, the whole request is denied.

Always-denied destinations include:

- unspecified and loopback space;
- RFC1918 private space;
- link-local and multicast space;
- carrier-grade NAT `100.64.0.0/10`;
- benchmark, documentation, and reserved ranges;
- addresses assigned to local VPS interfaces;
- administrator-supplied IPv4 `denyCIDRs`.

IPv6 is denied until a separate IPv6 policy is implemented and tested.

## Connection sequence

```text
SOCKS client              OpenFlux client          OpenFlux exit             destination
     |                           |                        |                         |
     | SOCKS5 greeting           |                        |                         |
     |-------------------------->|                        |                         |
     | no-auth selected          |                        |                         |
     |<--------------------------|                        |                         |
     | CONNECT host:port         |                        |                         |
     |-------------------------->|                        |                         |
     |                           | TLS + WebSocket Upgrade|                         |
     |                           | Authorization: Bearer… |                         |
     |                           | openflux.v1            |                         |
     |                           |----------------------->|                         |
     |                           | CONNECT control JSON   |                         |
     |                           |----------------------->|                         |
     |                           |                        | resolve once + policy   |
     |                           |                        | TCP dial literal IPv4   |
     |                           |                        |------------------------>|
     |                           | result: ok             |                         |
     |                           |<-----------------------|                         |
     | SOCKS success             |                        |                         |
     |<--------------------------|                        |                         |
     |<========================== binary byte stream ==============================>|
```

A SOCKS success response is sent only after the exit has successfully connected to the destination.

## Protocol version 1

Handshake:

- TLS-protected WebSocket;
- path `/openflux/v1/tunnel`;
- subprotocol `openflux.v1`;
- bearer token in the HTTP Upgrade request.

Client control message:

```json
{"version":1,"command":"CONNECT","target":"example.org:443"}
```

Successful exit response:

```json
{"version":1,"ok":true,"code":"ok"}
```

Control messages are text JSON, reject unknown fields and trailing JSON values, and are limited to 8 KiB. After successful negotiation, only binary WebSocket messages carry the TCP stream. A logical write is split into frames no larger than 64 KiB. The adapter preserves stream semantics across WebSocket message boundaries and serializes concurrent reads and writes.

The exit exposes only coarse failure classes:

- `bad_request`;
- `unsupported_version`;
- `policy_denied`;
- `resolve_failed`;
- `connect_failed`.

The client maps these to coarse SOCKS5 reply codes. Resolver, policy, and dial internals are not returned to the untrusted client.

## Trust boundaries

The MVP assumes:

- one controlled client/user;
- controlled client devices;
- one controlled VPS;
- a high-entropy shared token provisioned outside source control;
- a private key that remains on the VPS;
- a valid TLS trust path on the client;
- a SOCKS5 listener bound only to loopback.

Bearer authentication does not replace TLS. TLS protects the token in transit and authenticates the exit; the token authenticates the client to the exit.

The exit is still an outbound network capability. Destination policy reduces access to local and special-purpose networks, while `denyCIDRs` lets an administrator add environment-specific exclusions. It is not a general multi-user authorization system.

## Lifecycle

The SOCKS5 server tracks its listener and accepted client connections. `Close` stops new accepts, closes active clients, and waits for handlers.

The WSS server separately tracks upgraded WebSocket connections because `http.Server.Shutdown` does not own hijacked connections. Shutdown:

1. rejects new registrations;
2. closes active upgraded connections;
3. shuts down the HTTP server;
4. waits for relay goroutines, bounded by context timeout.

No systemd unit is part of this milestone. First deployment remains a foreground smoke test with an existing binary/config rollback.

## Excluded from this milestone

- UDP;
- IPv6;
- multiplexing;
- TUN/device-wide routing;
- raw sockets and kernel RST manipulation;
- gVisor in the WSS path;
- inbound port forwarding;
- automatic route selection/failover;
- public control API;
- systemd installation;
- mobile UI, Flux World, FluxBrain, and AI agents;
- claims of carrier-whitelist compatibility.

The legacy Yandex, OneMe/MAX, CupsOnline, packet/gVisor, and raw paths remain experimental and are not evidence about the WSS MVP's correctness or deployment safety.

## Verification boundary

The automated suite covers fragmented SOCKS5 parsing, WSS framing, protocol validation, TLS/authentication, remote DNS policy, large streams, concurrent streams, error mapping, and shutdown through local test servers and injected resolvers/dialers.

A real VPS foreground test is still required to validate packaging, certificates, file permissions, SSH forwarding, external DNS, and the VPS's observed egress address. Carrier/whitelist testing is a later and independent experiment.
