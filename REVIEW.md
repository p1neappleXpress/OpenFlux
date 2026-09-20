# Local review — 2026-09-17

Scope: local `codex/wire-v3-udp`, starting with `aeeac32`, `aa00cb3`, `09ab7d6`
on base `9148c63`. No GitHub PR was published or modified.

## Verdict

Keep the combined change **Draft**. The initial claim that all three priorities
were complete was too broad. CI exists and UDP has local coverage, but secure
negotiation and production Linux raw L3 UDP remain incomplete. Tests cannot
prove the absence of bugs.

## Corrected issues

- UDP payload bits interpreted as TCP FIN/RST; empty input could panic.
- Invalid UDP lengths/TCP data offsets accepted; UDP checksums included IP padding.
- Expired conntrack entries accepted until sweep; no table cap; endless stats loop.
- SOCKS5 failed/idle flows leaked; flows could be inserted after shutdown cleanup.
  Server Close left control connections open. Handshake deadlines, source-port
  hints, reserved bytes and IPv6 reply encoding were missing or incorrect.
- Mandatory DialUDP broke existing SOCKS dialers; UDP is now optional.
- Independent UDP read deadlines expired active one-way flows.
- gVisor packet/view reference leaks and unguarded detached-endpoint delivery.
- Batched Send accepted data outside Start/Stop; repeated Start spawned workers;
  received frames had no wire-byte/record limits. Queue memory is now bounded.
- Linux close relied on interrupting recvfrom; bounded I/O and descriptor locking
  prevent indefinite reads and fd-reuse I/O during shutdown.
- iOS blindly forwarded UDP to TCP-only exits; opt-in now preserves the fallback.
  Packet lengths and DNS response IPv4 headers were also corrected.

## Remaining blockers

1. Wire-v3 handshake is unauthenticated and not session-bound; replay/reconnect,
   downgrade resistance and MTU negotiation are unfinished. Disabled by default.
2. Raw L3 UDP source-port reservation/translation was implemented on September 18
   (see below). Its isolated Linux raw-socket/ICMP test passes in GitHub Actions,
   but only over loopback in a disposable network namespace. A real-network PMTU
   canary has not run. TCP port ownership and RST suppression are unchanged.
3. L3 fragmentation/ICMP/PMTU, process shutdown, legacy parser hardening, bounded
   legacy PacketTunnel associations and dial cancellation need follow-up.
4. No physical mobile-device or real document-carrier DNS/QUIC test was run.

## Reproducible checks

From the repository root (cache overrides are for this local sandbox):

```sh
export GOCACHE="$PWD/.cache/go-build"
export GOMODCACHE="$PWD/.cache/go-mod"
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...
./build_ios.sh
git diff --check
```

L4 echo requires a non-loopback IPv4 interface and explicitly skips without one.
The test does not change routes/VPN. Four codec compositions and 0/12/1200-byte
datagrams must pass. This is not a public-network/QUIC validation.

Final post-review verification: all commands above completed successfully
(exit 0) on macOS arm64 with Go 1.26.5. This includes the full test suite,
race detector, vet, all five cross-builds, the iOS arm64 static library and
whitespace checks. GitHub CI had not run at the time of this first review.

## Follow-up — 2026-09-18

Added endpoint-dependent UDP NAT with real kernel port reservations (port 0,
no address-reuse options), reverse translation, 256-flow cap, expiry and Close
cleanup. Reserves a free host port even when the original client port is occupied.
Replies must match the exact remote address and port. Invalid tunnel-input UDP
checksums and source addresses other than the configured tunnel client are dropped.
Linux raw-receive checksum/offload normalization remains outside this change;
naively checking raw bytes would reject partial-checksum loopback traffic.

Why reservations are required: Linux delivers packets to both raw sockets and
the kernel protocol handler, not exclusively to the raw socket
([raw(7)](https://man7.org/linux/man-pages/man7/raw.7.html)). The UDP handler's
no-socket path generates ICMP port-unreachable
([Linux UDP implementation](https://github.com/torvalds/linux/blob/master/net/ipv4/udp.c)).
The reservation keeps a matching UDP socket alive; its duplicate receive queue
is bounded to avoid an unbounded queue or a reader goroutine per flow.

Regression checks cover round-trip checksums, endpoint isolation, mapping reuse,
expiry, resource limits, send/bind failures, actual OS port ownership/release,
concurrent shutdown and the L3 handler integration. Linux-only `TestLinuxRawUDPNAT`
adds a real raw/socket echo, an occupied host source port, 32 replies (including
empty and 1200-byte datagrams), ICMP capture and shutdown. CI runs it as root
inside a disposable network namespace, without Internet or firewall changes.

Manual Linux invocation after compiling `go test -race -c -o /tmp/openflux-l3.test ./tunnel/l3`:

```sh
sudo unshare --net sh -ec 'ip link set lo up; OPENFLUX_L3_INTEGRATION=1 /tmp/openflux-l3.test -test.run "^TestLinuxRawUDPNAT$" -test.v -test.timeout=30s'
```

Follow-up verification: full `go test ./... -count=1`, race suite, vet, all five
cross-builds, iOS arm64 library and `git diff --check` passed (exit 0). The Linux
integration-test binary also cross-compiled successfully, but was not executed locally.
Fork GitHub Actions run 35278672089 later passed all six jobs at commit
`0a25fad`: tests, race detector, vet, five cross-build targets, and the isolated
Linux raw UDP/ICMP network-namespace test. This closes the namespace-test gate,
not the real-network, PMTU, fragmentation or physical-device gates.
