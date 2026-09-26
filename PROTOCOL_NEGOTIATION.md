# Authenticated negotiation v1

Opt-in CLI extension to the existing encrypted packet stack, not a new cipher
suite or a production security certification. Both peers need `--negotiate`,
batched codec and the same encryption secret/context. Old clients require the
unchanged default mode on the exit. No automatic downgrade is implemented.

## Envelope

Send order: IPv4 -> negotiation envelope -> existing AES-GCM packet -> batch-v2
framing/compression -> carrier. Receive reverses that order. The envelope is
inside AEAD, including its type, role, identities, capabilities and sequence.
Integers are unsigned big-endian; unknown versions/types/reserved bits fail closed.

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
other side has completed Start. The client's handshake deadline is 20 seconds;
the exit never initiates and waits for a client indefinitely.

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
effective packet limit, and pass the 64-entry sequence replay window. Duplicates,
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
carries cookies. Unknown subtypes are passed to the application and otherwise
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

## Limits and compatibility

Shared-secret holders are trusted peers. The existing static key derivation is
unchanged: no forward secrecy, automatic key rotation or protection after secret
compromise is claimed. AEAD authenticates packets; capability assertions still
describe configured software functionality, not a live Internet reachability test.

ICMP-error support refers to errors returned from a raw exit to the client; it
does not promise bidirectional arbitrary ICMP, IPv6, echo or redirects. There
is no active path-MTU probing. The separate raw-exit ICMP/NAT implementation
reports kernel route MTU errors and restores Internet ICMP quotes for live flows.
