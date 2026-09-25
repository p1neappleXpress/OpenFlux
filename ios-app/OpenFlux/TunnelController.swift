import Foundation
import Combine

enum TransportKind: String, CaseIterable, Identifiable {
    case yandex = "yandex"
    case volga = "volga"
    case boards = "boards"
    case direct = "direct"
    case mail = "mailru"
    case max = "oneme"
    var id: String { rawValue }
    var title: String {
        switch self {
        case .yandex: return "Yandex Docs"
        case .volga:  return "VOLGA"
        case .boards: return "Yandex Boards"
        case .direct: return "Прямой TCP (нужен ключ)"
        case .mail:   return "Mail.ru"
        case .max:    return "MAX"
        }
    }
    /// Document-based transports that take a public document URL / weblink.
    var usesDocURL: Bool { self == .yandex || self == .volga || self == .boards || self == .mail }
    /// direct берёт в том же поле не ссылку на документ, а host:port узла.
    var usesNodeAddr: Bool { self == .direct }
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
    func start(transport: TransportKind, url: String, maxToken: String, maxUid: String,
               port: Int, encryptionKey: String = "") {
        guard !running else { return }
        let addr = "127.0.0.1:\(port)"
        socksAddr = addr

        // Must precede the start call: the core reads the secret when it builds
        // the transport stack. Empty clears any previously set secret, so a
        // profile without encryption never inherits the last one's.
        encryptionKey.withCString {
            OpenFluxSetEncryption(UnsafeMutablePointer(mutating: $0))
        }
        if !encryptionKey.isEmpty { appendLog("[app] encryption: on") }

        // Сохранённые куки капчи — как и в расширении, ДО старта, иначе первый
        // же запрос к документу упрётся в ту же капчу, хотя куки уже есть.
        // Раньше это делало только расширение, и локальный прокси стартовал без
        // кук вслепую.
        let savedCookies = Secrets.captchaCookies() ?? ""
        savedCookies.withCString {
            OpenFluxSetInitialCookies(UnsafeMutablePointer(mutating: $0))
        }

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
        case 6:
            appendLog("[app] encryption key rejected (min 16 characters)")
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
        pollCaptcha()
    }

    private func appendLog(_ s: String) {
        log += (log.isEmpty ? "" : "\n") + s
        if log.count > 20000 {
            log = String(log.suffix(20000))
        }
    }

    /// Lines relayed from the packet-tunnel extension (a separate process, whose
    /// Go core has its own log buffer). Wired up by the view via VPNController.
    func appendExternal(_ s: String) {
        appendLog(s)
    }

    /// Clears the on-screen log. The Go ring buffer keeps filling; this only
    /// drops what has already been shown.
    func clearLog() {
        log = ""
    }

    // MARK: - Interactive captcha (in-app core only)
    //
    // When the local SOCKS core runs, the transport lives in THIS process, so the
    // captcha state is a direct C call. The system-VPN case goes through
    // VPNController's IPC instead, because there the core is in the extension.

    /// Document URL waiting on an interactive captcha, or nil.
    @Published var captchaURL: String?
    /// Проверка, которую надо пройти в интересах УЗЛА: не отключая туннель,
    /// чтобы куки были выданы на его адрес.
    @Published var remoteCaptchaURL: String?

    private func pollCaptcha() {
        guard running else {
            if captchaURL != nil { captchaURL = nil }
            return
        }
        if let c = OpenFluxCaptchaPending() {
            let s = String(cString: c)
            OpenFluxFreeString(c)
            let next = s.isEmpty ? nil : s
            if next != captchaURL { captchaURL = next }
        }
        if let c = OpenFluxRemoteCaptchaPending() {
            let s = String(cString: c)
            OpenFluxFreeString(c)
            let next = s.isEmpty ? nil : s
            if next != remoteCaptchaURL { remoteCaptchaURL = next }
        }
    }

    /// Отдаёт узлу куки, добытые через туннель.
    func offerCaptchaCookies(_ header: String) {
        let n = header.withCString {
            OpenFluxOfferCaptchaCookies(UnsafeMutablePointer(mutating: $0))
        }
        appendLog("[app] offered \(n) cookies to the exit node")
        remoteCaptchaURL = nil
    }

    /// Hands cookies solved in the WebView to the in-app core.
    func applyCaptchaCookies(_ header: String) {
        let n = header.withCString {
            OpenFluxApplyCaptchaCookies(UnsafeMutablePointer(mutating: $0))
        }
        appendLog("[app] captcha cookies handed to \(n) transport(s)")
        captchaURL = nil
    }

    /// Connectivity check WITHOUT the local proxy — used when the system VPN is
    /// active (all device traffic already routes through the tunnel), so a plain
    /// request exercises the VPN path itself.
    func testDirect() {
        appendLog("[app] test request (system VPN path) ...")
        let config = URLSessionConfiguration.ephemeral
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
