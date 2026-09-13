# OpenFlux

**English** | [Русский](README.ru.md)

Network stack research tool. TCP tunnel with pluggable transports.


# Disclaimer

The author of OpenFlux **does not encourage** the use of this project to bypass restrictions or violate the rules of any platform, and **is not responsible** for the final scenarios of how users apply this tool in real life or on the Internet. Any specific technical features of the application are nothing more than an **architectural coincidence**, created **without any intent**.

The project is **entirely non-commercial**, contains **no paid features, hidden subscriptions, or commercial benefit**.

The author **is not responsible** for forks, modifications, or derivative versions of OpenFlux created by third parties. Any changes added to a fork are the responsibility of its author.

The author **is not responsible** for:

- Any use of OpenFlux by third parties
- Consequences caused by the use of forks and modifications
- Damage resulting from derivative versions
- Violations committed using forks

The original code is provided **as is**, **without any warranties**.

## Clients

| Platform | Download | Notes |
|----------|----------|-------|
| **Android** | [OpenFluxAndroid releases](https://github.com/p1neappleXpress/OpenFluxAndroid) | Standalone APK |
| **iOS** | [TestFlight beta](https://testflight.apple.com/join/BwnAcdus) | System-wide VPN via Network Extension |

> **iOS app** built by [@saharev1](https://github.com/saharev1) — full iOS client, TestFlight pipeline, system VPN support, DNS-over-TLS, and many stability fixes. HUGE thanks! 🙏
>
> **Android app** — [p1neappleXpress/OpenFluxAndroid](https://github.com/p1neappleXpress/OpenFluxAndroid).

## Overview
```
Client (SOCKS5) --> Transport --> Exit Node --> Internet
```

## Requirements
1. Golang v. 1.26.3+ - is required for building desktop client / exit node binary (universal-bypass-tool);
2. Android Native Development Kit (NDK) v.27.0.12077973+ - is required for building Android client binary;
3. XCode v. 26.6+ - is required for building iOS client binary;
4. Linux VPS / VDS exit node.

## Overview

TCP packets are sent via Transport. Currently, there are three transports available:
1. Yandex - sends packets via Yandex Docs cursor messages;
2. Max - sends packets via WebRTC DataChannel
    WARNING:
   - **Do not use** your primary or important MAX account.
   - **Do not use** an account whose deletion or loss of access would be critical.
   - Usage via an **external VPS** may lead to **account restrictions**.
   - The **restriction may persist** after stopping OpenFlux.
   - MAX transport should be considered **experimental** until the blocking mechanism is understood.
3. Cups.online - sends packets via live-coding interview rooms (Centrifugo channels)
    WARNING:
   - Cups.online is a public interview service; rooms are open to anyone who knows
     their UUID.
   - Use --encryption-key-file if you care about confidentiality.
   - Do not abuse the room-creation endpoint; the exit node creates a small fixed
     number of rooms (default 4) at startup and keeps them for the session.

Client side runs a SOCKS5 proxy, exit node decapsulates and forwards packets to destination point.

## Structure

```
OpenFlux/
├── main.go                     # CLI entry (client / exit-node)
├── export_ios.go               # cgo bridge for the iOS static library (build tag: ios)
├── transport/
│   ├── transport.go            # Transport interface
│   ├── compressor.go           # Compression wrapper
│   ├── yandex/                 # Yandex Docs backend
│   ├── oneme/                  # MAX Messenger backend
│   └── cupsonline/             # Cups.online interview-room backend
├── tunnel/
│   ├── tunnel.go               # TCP tunnel core (proxy + raw exit modes)
│   ├── endpoint.go             # Virtual NIC
│   ├── rawsocket_linux.go      # Raw-socket exit mode (Linux, root)
│   └── rawsocket_{darwin,windows}.go  # stubs (raw mode unsupported)
├── socks5/                     # SOCKS5 server
├── network/                    # Checksums, packet parsing
├── utils/                      # Logging
├── ios-app/                    # SwiftUI iOS client (XcodeGen), links liboflux.a
├── build_ios.sh                # Build the iOS static library (liboflux.a)
├── build_ios_app.sh            # Build + archive + export the iOS app IPA
└── build_android.sh            # Build the Android client binary
```

## Build (desktop client / exit-node binary)

```bash
go mod tidy
go build -o universal-bypass-tool .
```

## Build for Android (client binary)
```bash
export ANDROID_NDK_HOME=<your Android NDK path>
./build_android.sh
```

## Build for iOS (client binary)
```bash
export XCODE_PATH="<your Xcode.app path>" # optional, defaults to /Applications/Xcode.app
./build_ios.sh
```

## Usage

### 1. Setting up exit node

The exit node runs a userspace TCP/IP stack (gvisor) in one of two modes:

- **proxy** (default, recommended) - every TCP connection from the client is terminated locally and re-originated through an ordinary `net.Dial` to the real destination. **No root, no raw sockets, no iptables** - just a normal process. Works on Linux, Windows, macOS.
- **raw** - gVisor forwards raw IP packets through a raw socket (Linux only, needs root + a scoped RST-drop iptables rule). Slightly faster end-to-end, but requires privileges.

Run in proxy mode (default):
```bash
./universal-bypass-tool --exit-node --url "YOUR_YANDEX_DOC_URL" --debug
```

Run in raw mode (Linux, root):
```bash
sudo ./universal-bypass-tool --exit-node --mode raw --local-ip 203.0.113.10 \
    --url "YOUR_YANDEX_DOC_URL" --debug
```

### 1. Setting up desktop client:

Setup commands for desktop client:
```bash
./universal-bypass-tool --client --url "YOUR_YANDEX_DOC_URL" --socks5 :1080 --debug
```

Then set up SOCKS5 proxy in your browser at localhost:1080.

### 2. Using the Cups.online transport

Cups.online is a public live-coding interview service. Each interview room is a Centrifugo channel (`$shared_editor:room-<uuid>`) that carries arbitrary base64 blobs - exactly what OpenFlux needs to move TCP packets.

**Exit node:** creates a small set of rooms at startup and prints a base64 room list that the client must use:

```bash
./universal-bypass-tool --exit-node --transport cupsonline --debug
```

```
=== COPY THIS TO CLIENT ===
eyJyb29tcyI6WyI0YTFh...base64...
===========================
```

**Client:** paste the printed base64 into `--url`:

```bash
./universal-bypass-tool --client --transport cupsonline \
    --url "eyJyb29tcyI6WyI0YTFh...base64..." --socks5 :1080 --debug
```

Notes:
- The room list is a session key: rooms live only as long as the exit node keeps them, and each exit-node restart produces a new list.
- TCP flows are pinned to a single room by flow-hash, so packet ordering inside a connection is preserved.
- Add `--encryption-key-file <path>` on both sides if you do not want Cups.online to see the contents.

## Flags

| Flag          | Default             | Description                |
|---------------|---------------------|----------------------------|
| `--client`    |                     | Run as client              |
| `--exit-node` |                     | Run as exit node           |
| `--socks5`    | `:1080`             | SOCKS5 listen address      |
| `--url`       | `https://localhost` | Document URL (Yandex Docs) |
| `--maxToken`  | ``                  | Auth token (Max)           |
| `--maxUid`    | ``                  | User ID (Max)              |
| `--debug`     | `false`             | Enable verbose logging     |
| `--transport` | `yandex`            | `yandex`, `vyandex`, `oneme`, `cupsonline` |
| `--mode`      | `proxy`             | Exit-node mode: `proxy` (default) or `raw` (Linux only, needs root) |
| `--local-ip`  | ``                  | Egress IP for exit node (raw mode only, scoped RST drop) |

## Implementing custom transports

You are free to implement the `Transport` interface from `transport/transport.go` and register your custom transport in main.go switch block.

## License

This project is licensed under the **GNU General Public License v3.0 or later**.
See [LICENSE](LICENSE) for the full text.

Third-party licenses are listed in [NOTICE](NOTICE).

## Disclaimer

Educational use only. Test on your own machines and networks.

## Support the project

**USDT · TRC20**

```
TXyTj5DqJNcQpd2yWwdVuXdabvQibXgLKC
```
