# Universal Bypass Tool

Network stack research tool. TCP tunnel with pluggable transports.

```
Client (SOCKS5) --> Transport --> Exit Node --> Internet
```

## What it does

Sends TCP packets through Yandex Docs cursor messages (or webRTC datachannel if MAX transport selected). Client side runs a SOCKS5 proxy, exit node decapsulates and forwards to real internet.

## Structure

```
universal-bypass-tool/
├── main.go
├── transport/
│   ├── transport.go      # Transport interface
│   └── yandex/           # Yandex Docs backend
│   └── oneme/            # MAX Messenger backend
├── tunnel/
│   ├── tunnel.go         # TCP tunnel core
│   ├── endpoint.go       # Virtual NIC
│   └── rawsocket.go      # Raw socket (exit node)
├── socks5/               # SOCKS5 server
├── network/              # Checksums, packet parsing
└── utils/                # Debug logging
```

## Build

```bash
go mod tidy
go build -o universal-bypass-tool .
```

## Build for Android
```bash
export ANDROID_NDK_HOME=<YOUR ANDROID NDK PATH>
./build_android.sh
```

## Build for iOS
```bash
XCODE_PATH="/Applications/Xcode.app"  # or Xcode-beta path
SDK_PATH="$XCODE_PATH/Contents/Developer/Platforms/iPhoneOS.platform/Developer/SDKs/iPhoneOS.sdk"
./build_ios.sh
```

## Usage

Exit node (needs root):
```bash
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP
sudo ./universal-bypass-tool --exit-node --url "YOUR_YANDEX_DOC_URL" --debug
```

Client:
```bash
./universal-bypass-tool --client --url "YOUR_YANDEX_DOC_URL" --socks5 :1080 --debug
```

Then point your browser to SOCKS5 proxy at localhost:1080.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--client` | | Run as client |
| `--exit-node` | | Run as exit node |
| `--socks5` | `:1080` | SOCKS5 listen addr |
| `--url` | `https://localhost` | Document URL (Yandex Docs) |
| `--maxToken` | `` | Token (Max) |
| `--maxUid` | `` | User ID (Max) |
| `--debug` | `false` | Verbose logging |
| `--transport` | `yandex` | Transport backend |

## Adding new transports

Implement the `Transport` interface from `transport/transport.go`, add your package, register in main.go switch.

## License

Educational use only. Test on your own machines and networks.

