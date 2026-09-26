# Future feature: multiple clients over one document

> Status: **not started, design only**. Written 2026-09-21 after live-testing the current one-document-one-client limitation and confirming
> the root cause in code and in production traffic. Nothing in this file has been implemented — `openflux-ctl` today still requires one
> document per client (see `cli/README.md`), which is the correct and only safe option until (if ever) this plan is executed.

## Why this file exists

`openflux-ctl` currently provisions one isolated exit-node container per client, each bound to its own document/transport link (see
`docs/superpowers/specs/2026-09-21-openflux-ctl-design.md`). That design was chosen specifically because the transport/tunnel stack has no
concept of multiple simultaneous clients sharing one document. This file records what it would actually take to lift that restriction, so
the idea doesn't have to be re-investigated from scratch later.

## The problem, confirmed twice

**1. Controlled experiment.** Two `--role=client` processes were pointed at the same `mailru` document, talking to the same exit node. Both
authenticated successfully as distinct mail.ru users, so the document medium itself tolerates multiple simultaneous participants. But at the
TCP layer, client A's debug log recorded the exact packet trace of client B's `httpbin.org` connection (identical `seq`, identical source
port `42016`, identical destination) — client A's own network stack received a SYN-ACK for a flow it never opened and correctly (per TCP)
sent an RST, killing client B's connection.

**2. Live incident.** While a PC client was left running (from the above test) against a document, a phone was separately connected as a
client on the same document for normal browsing. The PC client's debug log started showing the phone's real traffic (Apple APNs endpoints
`17.57.146.x:5223`, Cloudflare, Google) and issuing RSTs against it — i.e. the interference isn't a lab artifact, it reproduces immediately
in normal use whenever two devices share one document link.

## Root cause (confirmed in code)

- `tunnel/l3/l3.go:12`: `const clientIP = "10.10.10.2"` — the L3 exit backend hardcodes a single client identity. Every client that ever
  connects is "10.10.10.2" from the exit node's point of view.
- The same constant is hardcoded on the client side too: `tun_darwin.go`, `export_ios_packet.go` (`const tunClientIP = "10.10.10.2"`).
- `tunnel/l3/flow.go`: `flowKey{srcIP, dstIP, srcPort, dstPort, proto}` — the exit node's conntrack table has no session/client dimension at
  all. Two clients whose local gVisor stacks independently pick the same ephemeral source port produce byte-identical flow keys and collide.
- The document transport broadcasts every frame to every connected participant (confirmed: both test clients' processes received traffic for
  both sides' flows). Nothing in `transport/framing.go` or the client/exit receive paths filters incoming frames by sender — every connected
  party currently processes everything it receives as if it's addressed to it.

In short: the document medium supports N participants; the OpenFlux protocol built on top of it does not.

## What would need to change

### 1. Framing: add a session ID

`transport/framing.go`'s wire format needs a session/client ID field on every frame. This is a breaking wire-format change (version it).
Both ends stamp every frame they send with their own session ID.

### 2. Client-side: filter and self-identify

Each client generates a session ID at startup (random is fine — no coordination needed if the exit node builds per-session state lazily,
like conntrack "first packet creates state"). Every frame the client receives that isn't tagged with its own session ID must be dropped
before it reaches the local gVisor/l3 stack. This is the fix for the cross-talk observed above — it does not exist today in any form.

### 3. Exit node: become a session-aware router

This is the largest piece, and differs by backend:

- **L3 (`tunnel/l3/`)**: `clientIP`/`clientIPBytes` stop being a package constant. Each session gets its own virtual IP (from a small pool)
  or its own conntrack partition — either way, `flowKey` (`flow.go`) needs a `sessionID` component, and `l3.go`'s single conntrack table
  becomes a per-session table (or a single table keyed by `(sessionID, srcIP, srcPort, dstIP, dstPort, proto)`). Outbound-to-transport
  frames get wrapped with the destination session ID.
- **L4 (`tunnel/proxy_exit.go`)**: likely less invasive — gVisor already isolates per-connection state, so this is closer to "run N virtual
  NICs against one Transport, tagged by session" than "rebuild the flow table." Worth prototyping first since it may be the cheaper path to
  a working proof of concept.

### 4. Session lifecycle

Decide: does a session ever explicitly close (client sends a "goodbye" frame, exit node frees its virtual IP / conntrack partition), or does
it just time out like conntrack entries already do? Explicit close is cleaner but adds protocol surface; timeout-only is simpler but leaks
state until it expires.

## The open question that matters more than the code: trust model

Right now, isolation between clients is provided by the fact that **each client has its own document** — the link itself is the shared
secret between exactly two parties (one client, one exit node). Multiplexing onto one document removes that boundary. A plaintext session ID
is a _label_, not a _security boundary_ — any participant on the same document can, in principle, spoof another session's ID and read or
inject into that session's traffic, unless sessions are also cryptographically separated (distinct per-session keys, not the current single
`--encryption-key-file` for the whole pipe).

This decides the scope of the whole project:

- **Scope A — one owner, multiple own devices.** Lower trust bar (you trust yourself). A plaintext session ID with no per-session crypto may
  be an acceptable, explicitly-documented limitation. This is the cheaper version of this feature.
- **Scope B — multiple distinct, mutually-untrusting clients (the original multi-accounting motivation) sharing one document.** Requires
  real per-session key distribution/negotiation — materially more work than the routing changes above, and arguably defeats part of the
  operational simplicity that made "one document per client" attractive in the first place (you'd still need to hand each client something
  unique — a session key — even though they'd share a URL).

**Do not start implementation before deciding which scope this is.** The routing work in sections 1-3 is required either way; Scope B adds a
whole additional subsystem on top.

## Rough phased plan (Scope A — no per-session crypto)

1. **Spike**: prototype L4-mode multi-session support only (skip L3 initially — it's the more invasive backend). Two clients, one document,
   one exit node, `--mode=l4`. Goal: prove frame filtering + per-session gVisor NIC actually stops the cross-talk observed in this file's
   experiments.
2. **Framing v2**: add the session ID field, keep it backward-incompatible-but-versioned (a v1 frame from an old binary should fail closed,
   not be silently misparsed).
3. **Client-side filtering**: session ID generation + drop-if-not-mine, for both L3 and L4 client paths.
4. **L4 exit-node multi-session routing**: land the spike's approach for real.
5. **L3 exit-node multi-session routing**: per-session conntrack partitioning, virtual IP pool allocation.
6. **Per-transport validation**: repeat the two confirmed-broadcast tests (isolated + "leave one client running, connect a second device")
   against all four URL-based transports (`yandex`, `vyandex`, `cupsonline`, `mailru`) — the broadcast/multi-participant behavior was only
   confirmed for `mailru` here; the others are assumed similar but unverified.
7. **`openflux-ctl` integration**: once the protocol actually supports it, decide whether/how to expose it — e.g. an
   `openflux-ctl add-session <existing-client> <new-name>` that reuses an existing client's document instead of provisioning a new
   container, vs. leaving the CLI's one-container-per-client model as the default and treating shared-document mode as an advanced,
   separately-documented option.

## Effort estimate

Medium-to-large. Not a patch — a genuine protocol and systems change touching the wire format, both client and exit sides, and (if L3 is
included) the conntrack/NAT layer specifically. Budget this as a multi-week effort for Scope A; add real time for Scope B's key-distribution
subsystem if that turns out to be the actual requirement.

## References

- Design/spec for the current (shipped) one-document-per-client model: `docs/superpowers/specs/2026-09-21-openflux-ctl-design.md`
- `cli/README.md` — current, correct guidance: one document per client, no exceptions.
- `tunnel/l3/l3.go`, `tunnel/l3/flow.go`, `tunnel/l3/conntrack.go` — current single-tenant L3 exit implementation.
- `tunnel/proxy_exit.go` — current single-tenant L4 exit implementation.
- `transport/framing.go` — current wire framing, no session field.
