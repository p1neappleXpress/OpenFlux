# Volga Recovery Implementation Plan

**Goal:** Restore a stalled Yandex Volga channel automatically without restarting the process.

**Architecture:** Share an atomic authorization snapshot between the WebSocket listener and relay workers. Refresh it on reconnect, rotate WebSocket sessions periodically, and request reconnection when outbound packets have no inbound response for a sustained interval.

**Tech Stack:** Go, gorilla/websocket, Docker, Android Go binary.

## Tasks

1. Add a failing test for authorization refresh that verifies the relay sees new credentials after a WebSocket reconnect.
2. Implement atomic authorization sharing and a retrying refresh path; run package tests and race tests.
3. Add a failing test for sustained one-way traffic triggering a reconnect without triggering during idle traffic.
4. Implement the stall detector and periodic WebSocket rotation; run package and full Go tests.
5. Build the exit image and client binaries, deploy each node, and verify startup and live traffic.
6. Build and install Android APK updates, verify ADB status and live packet flow, and document any devices that could not be reached.
