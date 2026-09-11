import Foundation
import Combine

enum TransportKind: String, CaseIterable, Identifiable {
    case yandex = "yandex"
    case max = "oneme"
    var id: String { rawValue }
    var title: String {
        switch self {
        case .yandex: return "Yandex Docs"
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

    private var timer: Timer?

    /// Local SOCKS5 listen address for the currently running session.
    private(set) var socksAddr = ""

    /// Starts the client tunnel over the selected transport.
    /// - port: local SOCKS5 port to listen on (127.0.0.1:port).
    func start(transport: TransportKind, url: String, maxToken: String, maxUid: String, port: Int) {
        guard !running else { return }
        let addr = "127.0.0.1:\(port)"
        socksAddr = addr

        let rc = transport.rawValue.withCString { tt in
            url.withCString { u in
                addr.withCString { a in
                    maxToken.withCString { tok in
                        maxUid.withCString { uid in
                            OpenFluxStartClient(
                                UnsafeMutablePointer(mutating: tt),
                                UnsafeMutablePointer(mutating: u),
                                UnsafeMutablePointer(mutating: a),
                                UnsafeMutablePointer(mutating: tok),
                                UnsafeMutablePointer(mutating: uid)
                            )
                        }
                    }
                }
            }
        }

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
        default:
            appendLog("[app] start failed (code \(rc))")
        }

        running = OpenFluxIsRunning() != 0
        startPolling()
    }

    func stop() {
        OpenFluxStop()
        running = false
        connected = false
        pollOnce()
    }

    private func startPolling() {
        timer?.invalidate()
        timer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.pollOnce() }
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

    /// Connectivity check routed through the local SOCKS5 proxy.
    func testThroughProxy() {
        guard !socksAddr.isEmpty else { return }
        appendLog("[app] test request via SOCKS5 \(socksAddr) ...")
        let config = URLSessionConfiguration.ephemeral
        let parts = socksAddr.split(separator: ":")
        let host = String(parts.first ?? "127.0.0.1")
        let port = Int(parts.last ?? "1080") ?? 1080
        config.connectionProxyDictionary = [
            "SOCKSEnable": 1,
            "SOCKSProxy": host,
            "SOCKSPort": port
        ]
        config.timeoutIntervalForRequest = 20
        let session = URLSession(configuration: config)
        let url = URL(string: "http://ifconfig.me/ip")!
        let task = session.dataTask(with: url) { [weak self] data, _, err in
            Task { @MainActor in
                if let err = err {
                    self?.appendLog("[app] test failed: \(err.localizedDescription)")
                } else if let data = data, let body = String(data: data, encoding: .utf8) {
                    self?.appendLog("[app] test OK, exit IP: \(body.trimmingCharacters(in: .whitespacesAndNewlines))")
                } else {
                    self?.appendLog("[app] test returned no data")
                }
            }
        }
        task.resume()
    }
}
