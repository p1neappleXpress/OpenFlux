# fluxcore — the Flux Core brain (Phase 0 + Phase 1)

`fluxcore` is the dependency-free heart of Flux Core: metrics, health scoring,
route identity, the route registry, the event bus, config, and a local Control
API. It is a **separate Go module on purpose** — it builds and unit-tests with
plain `go` (1.22+), without the gVisor / pion transport stack that pins the main
OpenFlux module to Go 1.26.4. That keeps the brain fast to test and easy to
reason about.

```
cd fluxcore
go test ./...            # 30 tests, ~80% coverage
go run ./cmd/fluxdemo    # live Flux Heart demo on http://127.0.0.1:8787
```

The demo drives the **real** scoring engine from a *simulated* network, so you
can watch a heartbeat rise into "network under stress" during a storm and settle
back — no carriers, no root, no VPS.

## What's inside

| File | Role |
|------|------|
| `metrics.go`  | `Metrics` — the extended telemetry the brain reasons over (RTT, jitter, loss, throughput, liveness). |
| `ewma.go`     | `Ewma` + `Sampler` — turn raw observations into smoothed metrics. |
| `score.go`    | `HealthScore` → 0..100 **plus an explainable `Breakdown`**; `DegradationProbability` from the score trend. |
| `dna.go`      | `RouteDNA` — the identity of a path (transport+relay+exit+params), with a stable ID. |
| `route.go`    | `Route` + `RouteState` (unknown/standby/active/quarantined) + `RouteView`. |
| `registry.go` | `Registry` — all routes, `Best()` selector, Shadow-Network `Pools()`. |
| `events.go`   | `Bus` — structured pub/sub with replay; human-voice messages (§99). |
| `weather.go`  | `Forecast` — the now/+15/+30/+60 Network Weather projection (§7). |
| `probe.go`    | `MetricsProbe` — a **decorator** (like `CompressedTransport`) that frames data, injects ping/pong, and measures RTT/loss on the *same* carrier. |
| `config.go`   | JSON config with multi-route support and a probe cadence. |
| `api.go`      | `Server` — `/api/snapshot` (JSON) + `/api/stream` (SSE) + `BPM()` metaphor. |

## How it plugs into OpenFlux (the adapter)

The real `transport.Transport` already satisfies everything `fluxcore` needs.
Integration is a thin adapter that (a) frames traffic through a `MetricsProbe`
and (b) feeds a `Sampler`. Sketch:

```go
// In the OpenFlux main module, wrapping an existing transport.Transport.
type fluxLink struct{ t transport.Transport }
func (l fluxLink) Send(b []byte) error { return l.t.Send(b) }

route := registry.Add(fluxcore.RouteDNA{Transport: "yandex", Exit: exitID})
probe := fluxcore.NewMetricsProbe(fluxLink{trans}, route.Sampler())

// Outgoing: tunnel → probe.SendData → transport.Send
tunnelEP.onOutgoingPacket = func(p []byte) { probe.SendData(p) }

// Incoming: transport.Receive → probe.Inbound → tunnel.InjectInbound
trans.Receive(func(raw []byte) {
    if payload, isData := probe.Inbound(raw); isData {
        tunnelEP.InjectInbound(payload)
    }
})

// A ticker calls probe.Ping() every cfg.PingEvery and probe.SweepTimeouts(cfg.PingTimeout).
// route.Sampler().SetConnected(trans.IsConnected()) mirrors carrier state.
```

Because `fluxcore` is its own module, wire it into the main build with a
`require fluxcore v0.0.0` + `replace fluxcore => ./fluxcore` in the root
`go.mod` at integration time (Phase 2), or merge the module once the toolchain
is unblocked.

## Design principles carried from the spec

- **Explainable, not magic.** Every score ships its `Breakdown`; every forecast
  is a labelled *projection* of observed trend, never a claim about the future.
- **Honest liveness.** A route that isn't connected can't score as healthy; the
  headline BPM tracks the *active* path, so a healthy standby never masks a
  struggling active one.
- **Decorator, not rewrite.** `MetricsProbe` wraps the existing transport the
  same way `CompressedTransport` does. The working core stays untouched.
