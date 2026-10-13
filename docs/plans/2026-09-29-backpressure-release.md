# Backpressure Release Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Publish the verified backpressure fix as the next CLI and installable node releases of meepo161/OpenFlux.

**Architecture:** Keep the CLI and node release workflows separate. Build node assets reproducibly with the Go version in go.mod, pin their hashes in the installer, then pin that exact installer commit for client builds. Publish both tags only after the branch and CI checks are ready.

**Tech Stack:** Go 1.26.4, Git, GitHub Actions, SHA-256.

---

### Task 1: Prepare node-v1.2.1

**Files:** `deploy/node-install.sh`, `CHANGELOG.md`

1. Build Linux amd64, arm64 and arm/v7 with `GOTOOLCHAIN=go1.26.4`, `CGO_ENABLED=0`, `-trimpath -buildvcs=false -tags exitnode -ldflags="-s -w -buildid="`.
2. Record SHA-256 for each binary; update `CORE_VERSION` and the three SHA fields in `deploy/node-install.sh`.
3. Add a `0.3.1 / node-v1.2.1` entry to `CHANGELOG.md` describing the packet queue fix and diagnostics.
4. Commit the installer and changelog; preserve this commit ID for the client pin.

### Task 2: Pin the installer for the CLI release

**Files:** `provision/pin.go`

1. Hash `deploy/node-install.sh` as raw bytes.
2. Set `PinnedCommit` to the Task 1 commit and `PinnedSHA256` to the script hash.
3. Run `go test -count=1 ./...`, `go vet ./...`, and `git diff --check`; commit the pin update.
4. Build Windows amd64 and Linux amd64 CLI artifacts and record SHA-256.

### Task 3: Publish and verify

1. Fast-forward `meepo/fork` from the current tip after checking it has not moved unexpectedly.
2. Push annotated tags `node-v1.2.1` and `v0.3.1` at the verified commit.
3. Check both GitHub Actions runs and both release pages. Confirm expected assets and hashes, including the node installer's hash gate.
4. Fast-forward the local `fork` checkout, and report release links and any remaining deployment step.
