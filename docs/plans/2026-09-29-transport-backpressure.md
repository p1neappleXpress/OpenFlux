# Transport Backpressure Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Prevent avoidable packet loss under carrier backpressure, expose low-cost counters, and compare L3 and L4 throughput on an isolated test node.

**Architecture:** Keep the existing finite batch queue, but make its producer wait for capacity or shutdown. Keep one encoded batch pending when the inner carrier is temporarily full or disconnected; retry in order with a bounded delay, stopping promptly on shutdown. Export queue-wait and retry counters through `TransportStats`, and report them with TCP retransmits at a low rate. Verify against a controllable fake carrier before repeating the same live transfer.

**Tech Stack:** Go, existing `transport` and `tunnel` packages, Go tests, temporary OpenFlux processes on the test VPS.

---

### Task 1: Establish the backpressure contract

**Files:** `transport/batched_test.go`, `transport/batched.go`

1. Add a test that fills the queue behind a blocked inner carrier and expects another `Send` to wait, then succeed after capacity is released. Run it and confirm it fails because current `Send` returns `batch queue full`.
2. Add a test that `Stop` releases a blocked `Send`. Run it red.
3. Change `BatchedTransport.Send` to select on queue capacity and `stopCh` without holding `lifecycle` while waiting. Run the focused tests green, then run `go test ./transport`.

### Task 2: Preserve batches across transient carrier failures

**Files:** `transport/batched_test.go`, `transport/batched.go`

1. Add a test carrier that rejects the first few sends and then accepts. Assert that the first batch is eventually delivered once, before later batches. Run it red.
2. Add a test that `Stop` interrupts an inner retry. Run it red.
3. Retry the pending encoded batch after a short wait until accepted or stopped. Do not dequeue a later batch first. Run focused tests green and `go test ./transport`.

### Task 3: Lightweight diagnostics

**Files:** `transport/transport.go`, `transport/batched.go`, `transport/session.go`, `transport/batched_test.go`, `tunnel/tunnel.go`

1. Add tests for queue-wait and inner-retry counters in `TransportStats`, including Session aggregation. Run them red.
2. Implement atomic counters and aggregation. Report nonzero queue waits/retries with TCP retransmits once per existing 30-second tunnel stats tick; avoid per-packet info logs. Run focused and full tests.

### Task 4: Measure and verify

**Files:** `docs/plans/2026-09-29-transport-backpressure.md`

1. Run `go test ./...` and `go test -race ./transport` on the isolated worktree.
2. Build the Windows client and Linux exit binaries from this worktree.
3. On the test VPS, use temporary processes and a temporary HTTP payload to compare L4 and L3 through the same Mail.ru document; capture speed, queue waits, retries, and TCP retransmits. Clean up processes, keys, and temporary files.
4. Compare the revised L4 result with the measured 4.7–5.9 Mbit/s baseline. Record any unverified limitation, including external path differences.

## Verification and measurements

- Focused backpressure tests passed 30 consecutive runs; `go test ./...` passed.
- Windows race instrumentation was unavailable because `CGO_ENABLED=0` and no C compiler is installed.
- A temporary 24 MiB transfer through the Mail.ru document and revised L4 exit completed at 14.2 Mbit/s. A separate L3 run, with a temporary scoped kernel RST rule, completed at 5.1 Mbit/s; it logged 444 carrier send retries in 30 seconds and the Mail.ru WebSocket disconnected. This run does not isolate L3 overhead.
- An alternating L4 comparison of a 16 MiB file on the same source measured 6.6 Mbit/s with the original build, then 14.2 Mbit/s with the revised build. A subsequent original-build run failed during Mail.ru negotiation. Network and document conditions varied substantially, so this is evidence of a successful faster run rather than a stable speedup estimate.
- Temporary remote processes, files, keys, and the scoped firewall rule were removed after the runs. No production service was changed.
