# Transport speed tests

Scripts used for TRANSPORT_PERFORMANCE.md. They are a harness for one particular
setup (a Windows PC with PowerShell, a Linux VPS reached over SSH), kept so the
numbers can be reproduced; adjust the variables at the top.

- `SPEED_HOST` the VPS, `SPEED_LAN_IP` the PC's LAN address (the tunnel refuses
  loopback destinations, so the local rig serves its test files on it).
- Build `cmd/speedbench` (layer-by-layer stack benchmark, delay/rate/stall
  proxy, HTTP server) and the core for both OSes; the VPS needs the Linux core,
  `speedbench`, python3 and the files here under /root.
- `vps-e2e.sh`   exit + client + curl on one host over loopback: throughput, TTFB
  and CPU seconds of both processes (the CPU-bound figure of a small VPS).
- `vps-prof.sh`  the same with CPU profiles of both processes (OPENFLUX_PPROF).
- `server-loop.sh` speedbench stacks (tcp, direct, enc, full) over loopback.
- `rig-lib.ps1`, `rig.ps1`, `bench-rig.ps1` local rig: HTTP server <- exit <-
  delay/rate/stall proxy <- client, all on the PC; `-RateKB`, `-QueueKB`,
  `-StallHz`, `-StallMs` shape the emulated WAN.
- `wan-final.sh`, `wan-var.sh`, `wan-bloat.sh` fresh client per sample over the
  real path: TTFB, upload, download, parallel downloads; latency under load.
- `compat.sh` md5 integrity both ways and 4 parallel flows for every
  client/exit pair of two core builds.
- `httpsrv2.py` the test server (`/big?mb=N`, `/det?mb=N` deterministic,
  POST `/up` answers with the md5 of the body).

Test traffic is always high-entropy: plain low-entropy test flows were stalled
by something on the PC <-> VPS path (see TRANSPORT_PERFORMANCE.md).
