# Authenticated negotiation v1

Not a new cipher suite or a production security certification: an
authenticated session (the Session) on top of the existing encrypted packet
stack. Both peers need the same encryption secret and context (see
"Encryption context"). A Session runs whenever a peer is configured with a
key: `--negotiate`, `--transports`, `.conf` transports, the apps' Session
profiles, and also classic setups (`--transport=X` with a key, the apps'
classic profiles), which keep classic compatibility (see "Classic
compatibility").

## Layering on a carrier

There are two layerings, and implementations must match the one they
speak byte for byte:

    Session  IPv4 -> envelope -> batch-v2 frame of envelopes -> AES-GCM record -> carrier
    classic  IPv4 -> AES-GCM record -> codec frame (batch-v2 or legacy) -> carrier

In the Session the batch frame (zstd when it helps) is encrypted as a whole,
so every carrier frame starts with the record header `OFX` (0x4F 0x46 0x58),
version 1 and the direction byte. In the classic layering each IPv4 packet is
encrypted on its own and the records are framed by the codec, so a carrier
frame starts with 0x02 (batch-v2) or 0x00 / 0x1F (legacy per-packet, raw /
LZ4). Receive reverses the order. Because the first byte differs, one
carrier can carry both, and a receiver can tell which layering its peer runs.

(An earlier version of this document gave the Session's order as
envelope -> AES-GCM -> batch-v2, which is the classic order; the code has
always encrypted the batch frame. A Session built from that description
does not interoperate.)

## Envelope

The envelope is inside AEAD, including its type, role, identities,
capabilities and sequence. Integers are unsigned big-endian; unknown
versions/types/reserved bits fail closed.

| Bytes | Meaning |
| --- | --- |
| 0..3 | `OFN` followed by version byte 1 |
| 4 | 1 = hello, 2 = IPv4 data, 3 = control |
| 5 | sender role: 0 client, 1 exit; peer must have opposite role |
| 6..37 | sender's random 256-bit process-session challenge |
| 38..69 | recipient's challenge; zero only for initial hello |
| 70..77 (hello) | capabilities uint32, max IPv4 packet uint16, ready byte (0/1), reserved zero byte |
| 70..77 (data) | sequence uint64, starting at 1 |
| 70..77 (control) | subtype byte, flags byte (zero), payload length uint16, reserved zero |
| 78.. (data) | one complete IPv4 packet (possibly an IP fragment) |
| 78.. (control) | payload (JSON for the subtypes below) |

Hello length is exactly 78 bytes. Capability bits: 0 IPv4, 1 TCP, 2 UDP,
4 ICMP errors. Bit 3 belonged to the retired wire-v3 prototype and is rejected.
IPv4+TCP are mandatory. Allowed packet limits: 1280..65000, leaving space for
this envelope and existing AES overhead within a 65535-byte batch record.

## State and acceptance

Each instance generates a cryptographically random challenge. An authenticated
hello without an echo can elicit a response but does not establish a session.
Readiness requires a valid peer hello echoing the current local challenge.
The effective policy is the capability intersection and smaller packet limit.
The established peer cannot change its policy through later hellos. Retries
with an unconfirmed ready bit receive a fresh confirmation, including after the
other side has completed Start. The client's handshake deadline is 20 seconds
(a client with classic compatibility starts sending classic after 3 and
keeps offering the handshake); the exit never initiates and waits for a
client indefinitely.

A hello from a different sender while established usually means the peer
restarted, but may be old traffic replayed from a carrier (anyone with access
to a document sees the ciphertext). The established session is left untouched:
an initial hello from the new sender is answered with a challenge minted for it
alone (one candidate at a time, at most one new candidate per second, valid for
20 seconds). Only a hello echoing that fresh challenge, which replayed traffic
cannot contain, replaces the peer. The replacement runs under the fresh
challenge with a new sequence and replay window, so the old session's data no
longer matches. The exit therefore serves one active client at a time; two
clients sharing a secret take the session from each other.

A client whose peer answers keepalives (below) and has been silent on every
carrier for the link timeout drops the session and handshakes again under a
new challenge, as a restarted process would: a restarted exit knows nothing of
the old session and never speaks first.

Data must name both current challenges, use a negotiated protocol, fit the
effective packet limit, and pass the 4096-entry sequence replay window. Duplicates,
zero sequence numbers and packets older than the window are discarded; bounded
reordering is accepted. There is no delivery acknowledgment or retransmission.
This avoids adding a reliability layer underneath UDP/QUIC.

## Carriers

Every configured carrier is started; one whose Start fails (e.g. a captcha
during authorization) is retried with exponential backoff (1s to 30s) and joins
when it comes up. Each carrier is started once, through its batching and
encryption wrappers.

A carrier being attached to its document says nothing about the peer's side of
it, so each side records when the peer was last heard on each carrier (any
authenticated envelope). Carriers quiet for 10 seconds get a LinkPing, answered
with a LinkPong on the carrier it arrived on. Once the peer has answered a ping,
a carrier is live only if the peer was heard on it within 30 seconds; a peer
that never answers predates keepalive, and liveness stays the carrier's own
connection state. Data and control use the highest-priority live carrier; flows
are hashed across carriers only when they share that priority.

## Control messages

| Subtype | Direction | Payload |
| --- | --- | --- |
| 0x01 CookiesRequest | client -> exit | `{"transport"}`; the exit answers with its jar |
| 0x02 CookiesResponse | exit -> client | `{"transport","jar"}` |
| 0x03 CookiesOffer | both | `{"transport","jar"}`, applied and persisted by the receiver |
| 0x04 AuthRequired | exit -> client | `{"transport","url","reason"}` (`smartcaptcha` or `login`) |
| 0x10..0x13 | client <-> exit | transport start, stop, status, list |
| 0x20 LinkPing, 0x21 LinkPong | both | none |

An empty `transport` (older peers) means the highest-priority transport that
carries cookies. Cookie messages and AuthRequired may carry `"doc"`, the
document URL of that transport on the sender's side: the two sides do not
always name carriers alike, so a receiver that has no carrier of that name
matches by `doc`, then by the type the name starts with when it has one
carrier of that type. Unknown subtypes are passed to the application and otherwise
ignored.

## Checks on the exit

When a transport on the exit hits SmartCaptcha or a login wall, the exit sends
AuthRequired (at most every 20 seconds per transport) over any live carrier,
typically a direct one while the document carrier is the one stuck. The check
has to be passed from the exit's address. The client relays it to the app over
IPC as a CookiesRequest with `remote: true` and `proxy`: a loopback HTTP proxy
(CONNECT and plain requests) whose connections leave through the tunnel and
the exit. The app points its browser at that proxy, passes the check and
answers with a CookiesOffer carrying `remote: true`, which the client forwards
to the exit as CookiesOffer for that transport.

The proxy runs its own small TCP stack on the tunnel. The exit answers only one
client address, so this stack shares it and uses local TCP ports 12000..12999,
below gVisor's ephemeral range and common OS ones; replies to those ports go to
it, everything else to the regular client path. The proxy listens on loopback
without authentication while it runs, like the SOCKS5 inbound.

## Classic compatibility

A peer configured classic with a key runs a Session with the classic
layering next to it on its one carrier:

- A client falls back: it offers the handshake and, until the exit answers
  (it waits 3 seconds at start), sends IPv4 in the classic layering, so an
  exit that predates Session or runs without it still works. It keeps
  offering the handshake (every 2 seconds after the first 20, every 10 once
  the exit answered classic) and switches to the Session when answered. A
  single-carrier Session client (a Session profile or `--transports` with
  one carrier) falls back the same way; `--negotiate` does not.
- An exit configured classic serves classic clients as well as Session
  ones, as long as no Session client was heard within the link timeout:
  classic frames that arrive while a Session client is active are dropped
  (a replayed capture cannot take the replies away from it).
- An exit configured as a Session (`--negotiate`, `--transports`, `.conf`
  transports, the node wizard, the apps' Session exits) serves Session
  clients only; classic frames are dropped and logged with the fix.

## Codec (classic layering)

Both framings are decoded whichever the peer uses. A peer sends batch-v2
once it has received a batch frame (an empty one, `02 00`, is a capability
probe), legacy while it has received only legacy frames, and its preferred
framing (`--codec`) before it has heard anything. The side that speaks
first (the client) switches to the other framing after 2 seconds without
an answer, then every second, so a peer that decodes one framing only is
still reached. A lone 0x00 is a carrier keepalive (Volga), not a frame.

## Encryption context

The scrypt salt is `SHA-256("OpenFlux encrypted transport v1\0" + context)`.
Every peer picks the context with the same rule (`transport.KDFContexts`):

1. an explicit context (`--session-context`, the context an openflux://
   link carries);
2. `--url`, unless it is a cupsonline room list;
3. the URL of the highest-priority carrier that names the channel
   (not cupsonline: its exit creates the room list at start; not direct:
   host:port differs between the sides; not oneme);
4. `http://#`.

Builds have derived it differently (the classic cupsonline client used the
room list; panels and an older fork used the transport name), so each peer
also knows the alternates other builds derive for its setup. A record that
does not open under the current keys is tried under them; the exit answers
under the context the client used, and a client that hears nothing moves to
the next candidate after 4 seconds, then every 2. The context is a public
salt: accepting several weakens nothing, each one still requires the secret.

## Limits and compatibility

Shared-secret holders are trusted peers. The existing static key derivation is
unchanged: no forward secrecy, automatic key rotation or protection after secret
compromise is claimed. AEAD authenticates packets; capability assertions still
describe configured software functionality, not a live Internet reachability test.

The classic layering has no challenge binding, sequence window or
capability negotiation, only AES-GCM with a bounded nonce replay cache.
A client that falls back to it can be kept there by whoever drops the
exit's handshake answers; `--negotiate` rules that out on both sides.

ICMP-error support refers to errors returned from a raw exit to the client; it
does not promise bidirectional arbitrary ICMP, IPv6, echo or redirects. There
is no active path-MTU probing. The separate raw-exit ICMP/NAT implementation
reports kernel route MTU errors and restores Internet ICMP quotes for live flows.
