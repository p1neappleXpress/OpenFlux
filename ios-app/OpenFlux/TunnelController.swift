import Foundation
import Combine

/// Swift wrapper around the OpenFlux Go static library (liboflux.a).
@MainActor
final class TunnelController: ObservableObject {
    @Published var running = false
    @Published var connected = false
    @Published var log: String = ""
    @Published var stats: String = ""

    let socksAddr = "127.0.0.1:1080"

    private var timer: Timer?

    /// Starts the SOCKS5 client tunnel over the Yandex.Docs transport.
    func start(url: String) {
        guard !running else { return }
        let rc = "yandex".withCString { tt in
            url.withCString { u in
                socksAddr.withCString { addr in
                    "".withCString { tok in
                        "".withCString { uid in
                            OpenFluxStartClient(
                                UnsafeMutablePointer(mutating: tt),
                                UnsafeMutablePointer(mutating: u),
                                UnsafeMutablePointer(mutating: addr),
                                UnsafeMutablePointer(mutating: tok),
                                UnsafeMutablePointer(mutating: uid)
                            )
                        }
                    }
                }
            }
        }
        if rc != 0 {
            appendLog("[app] start failed, code \(rc)")
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
        // keep the tail bounded
        if log.count > 20000 {
            log = String(log.suffix(20000))
        }
    }

    /// Simple connectivity check that routes an HTTP request through the local
    /// SOCKS5 proxy, proving the tunnel actually carries traffic.
    func testThroughProxy() {
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
        let task = session.dataTask(with: url) { [weak self] data, resp, err in
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
