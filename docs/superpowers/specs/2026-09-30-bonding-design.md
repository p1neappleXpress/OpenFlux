# Bonding: one Session over several networks at once

Status: approved 2026-09-30 (variant A). Scope: core, Android, Desktop. iOS later.

## Goal

A client with two networks (a phone's mobile data and Wi-Fi from another
operator's modem; a PC's Ethernet and Wi-Fi) sends one Session over both at
once: their speeds add up, and when one network drops the traffic moves to
the other in under a second. It works over any carriers, including the
documents that pass whitelist-only networks.

Success: several connections (or one UDP stream) over several documents
get at least 1.4x one document; a connection whose carrier dies moves to
another in about a second.

## What exists

- Session keeps several carriers (links) with priorities; every IPv4 packet
  carries `DataTail.Sequence`; a 4096-packet replay window accepts packets
  out of order and drops duplicates.
- `Session.Send` spreads *flows* (hash of the 5-tuple) over the live links
  of the top priority. One flow always rides one link.
- Every carrier dials through `netbind`, which binds all sockets to one
  interface on Windows (full tunnel) and does nothing elsewhere.

## Design

### 1. Agreement (backward compatible)

A new hello capability bit would break old exits (`validParameters` rejects
unknown bits, so the handshake fails). Instead, after the handshake a client
with bonding on sends a control message `SubtypeBonding` (payload: version
byte 1). An exit that supports bonding answers `SubtypeBonding` too and
switches bonding on for this peer; an old exit ignores the unknown subtype.
The client repeats the offer on each keepalive until answered, and stripes
only once answered. A new session (peer restart, takeover) starts with
bonding off until agreed again.

### 2. Sending: TCP per connection, UDP per packet

Carriers queue internally (a document's write queue), so the sender cannot
see a link's real speed from its own queue. The receiver reports what
arrived instead, as SRTLA does with SRT ACKs:

- With bonding agreed, the receiver sends `SubtypeBondingAck` every 50 ms
  while data arrives, on the carrier data last arrived on (the first by
  priority may be the dead one): the newest sequence it accepted and a
  512-bit map of the numbers up to it. A lost ack is covered by the next.
- The sender remembers each packet in flight with its link, size and send
  time; an ack credits the link (in-flight down, delivered bytes and an RTT
  sample up). Unacknowledged for four RTTs (1 s to 2 s) counts as lost: the
  link then gets single probe packets until an ack shows it is back.
- A link's rate is the best delivery rate of its last eight busy 250 ms
  windows (idle windows say nothing), floor 16 KiB/s, start 256 KiB/s.
- A packet's link is the one where it is due first:
  `MinRTT/2 + (inflight + size) / rate`.

Measured on Mail.ru documents, a TCP connection split packet by packet
over several of them collapses (0.09 MB/s against 0.59 on one document):
documents deliver in bursts hundreds of milliseconds apart, TCP takes the
reordering for loss. So:

- **TCP**: each connection stays on one carrier, chosen as above when it
  starts; it moves when its carrier is lost, or after 500 ms idle. Several
  connections add up (three documents: 0.84 MB/s against 0.59).
- **UDP** (SRT, QUIC): split packet by packet; these protocols reorder in
  their own buffers, so one SRT stream uses every carrier.

The receiver delivers packets as they come; there is no reorder buffer.

### 4. Binding a carrier to a network

- `TransportConfig.Network`: `""` (default route), `cellular`, `wifi`,
  `ethernet`. In a `.conf`: `Network = cellular` in a `[Transport]`
  section; in Session specs JSON: `"network"`.
- `netbind.DialContextFor(network)` / `WrapFor(network, *net.Dialer)`
  apply the global binding and then the per-network one. Every carrier
  dials through them with its config's network.
- `netbind.SetNetworkBinder(func(network string, fd uintptr) error)`:
  platforms register how to bind a socket. Android: the mobile library's
  `NetworkBinder` interface, implemented in Kotlin with
  `Network.bindSocket`. Windows: the interface index of the first adapter
  that is up and of that type (Wi-Fi 71, Ethernet 6, WWAN 243/244), via
  `IP_UNICAST_IF`.
- A network that is not available fails the dial; the carrier retries as
  today and stays out of the bonding while down.

### 5. Android

- Profile: `bonding: Boolean`; each carrier: `network` (any, mobile,
  Wi-Fi). Kept on the device, not in `openflux://` links.
- The service requests the networks the profile names
  (`ConnectivityManager.requestNetwork`, `CHANGE_NETWORK_STATE`), so mobile
  data stays up next to Wi-Fi, and binds sockets with `Network.bindSocket`.
- Profile editor: a «Бондинг» switch and a network choice per carrier. Home:
  the bonded links with their network and speed.

### 6. Desktop

Same profile fields (network choice adds Ethernet); the `.conf` gets
`Network =`; the core binds by adapter type. Home shows the bonded links.

## Testing

- Unit (transport): links with set rate/latency/drop; the scheduler's split
  follows the acknowledged rates and keeps low load on the quickest link;
  acks encode and decode; a TCP connection stays on one carrier while
  several spread; UDP is split; a connection moves off a dead carrier; a
  bonding client with an old exit never splits.
- Unit (netbind): `DialContextFor` calls the binder with the network.
- Live: two local peers over two Mail.ru documents, one TCP flow, against
  one document.

## Out of scope

iOS; duplicating packets over several links; bonding classic peers.
