import Foundation
import Combine

enum TransportKind: String, CaseIterable, Identifiable {
    case yandex = "yandex"
    case vyandex = "vyandex"
    case max = "oneme"
    var id: String { rawValue }
    var title: String {
        switch self {
        case .yandex: return "Yandex Docs"
        case .vyandex: return "Yandex Docs (Volga)"
        case .max: return "MAX"
        }
    }
}

/// Swift wrapper around the OpenFlux Go static library (liboflux.a).
@MainActor
final class TunnelController: ObservableObject {
    @Published var running = false
    @Published var connected = false
    @Published var log: String = ""
    @Published var stats: String = ""
    @Published var startError: String?

    private var timer: Timer?
    private let bridgeQueue = DispatchQueue(label: "OpenFlux.socks-bridge", qos: .userInitiated)
    private var operation: UInt64 = 0

    /// Local SOCKS5 listen address for the currently running session.
    private(set) var socksAddr = ""

    /// Starts the client tunnel over the selected transport.
    /// - port: local SOCKS5 port to listen on (127.0.0.1:port).
    func start(transport: TransportKind, url: String, maxToken: String, maxUid: String, port: Int, codec: String, encryptionSecret: String) {
        guard !running else { return }
        startError = nil
        let addr = "127.0.0.1:\(port)"
        socksAddr = addr
        running = true
        connected = false
        operation &+= 1
        let id = operation
        // Volga authorization and the encryption KDF may block. Serialize Go
        // start/stop away from the main actor so neither freezes the app UI.
        bridgeQueue.async { [weak self] in
            let rc = Self.startBridge(transport: transport, url: url, maxToken: maxToken,
                maxUid: maxUid, addr: addr, codec: codec, encryptionSecret: encryptionSecret)
            Task { @MainActor in
                guard let self = self, self.operation == id else { return }
                self.finishStart(rc: rc, addr: addr, transport: transport, port: port)
            }
        }
    }

    nonisolated private static func startBridge(transport: TransportKind, url: String, maxToken: String,
        maxUid: String, addr: String, codec: String, encryptionSecret: String) -> Int32 {
        transport.rawValue.withCString { tt in
            url.withCString { u in
                addr.withCString { a in
                    maxToken.withCString { tok in
                        maxUid.withCString { uid in
                            codec.withCString { c in
                                encryptionSecret.withCString { key in
                                    OpenFluxStartClientV2(
                                        UnsafeMutablePointer(mutating: tt),
                                        UnsafeMutablePointer(mutating: u),
                                        UnsafeMutablePointer(mutating: a),
                                        UnsafeMutablePointer(mutating: tok),
                                        UnsafeMutablePointer(mutating: uid),
                                        UnsafeMutablePointer(mutating: c),
                                        UnsafeMutablePointer(mutating: key)
                                    )
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    private func finishStart(rc: Int32, addr: String, transport: TransportKind, port: Int) {
        switch rc {
        case 0:
            appendLog("[app] started on \(addr) via \(transport.title)")
        case 1:
            appendLog("[app] already running")
        case 2:
            appendLog("[app] unknown transport")
        case 3:
            appendLog("[app] transport failed to start")
        case 4:
            appendLog("[app] port \(port) is busy — pick another port")
        case 6:
            appendLog("[app] invalid transport, codec or encryption settings")
        default:
            appendLog("[app] start failed (code \(rc))")
        }

        running = OpenFluxIsRunning() != 0
        if !running {
            startError = rc == 4 ? "Local port is busy. Choose another port in Advanced settings." : "Cannot start proxy. Check connection settings and the diagnostic log."
        }
        startPolling()
    }

    func stop() {
        timer?.invalidate()
        operation &+= 1
        let id = operation
        running = false
        connected = false
        bridgeQueue.async { [weak self] in
            OpenFluxStop()
            Task { @MainActor in
                guard let self = self, self.operation == id else { return }
                self.pollOnce()
            }
        }
    }

    private func startPolling() {
        timer?.invalidate()
        let id = operation
        timer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self = self, self.operation == id else { return }
                self.pollOnce()
            }
        }
    }

    private func pollOnce() {
        running = OpenFluxIsRunning() != 0
        connected = OpenFluxIsConnected() != 0

        if let c = OpenFluxReadLog() {
            let s = String(cString: c)
            OpenFluxFreeString(c)
            if !s.isEmpty { appendLog(s) }
        }
        if let c = OpenFluxStatsJSON() {
            stats = String(cString: c)
            OpenFluxFreeString(c)
        }
    }

    private func appendLog(_ s: String) {
        log += (log.isEmpty ? "" : "\n") + s
        if log.count > 20000 {
            log = String(log.suffix(20000))
        }
    }

}
