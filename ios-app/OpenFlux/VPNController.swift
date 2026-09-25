import Foundation
import NetworkExtension
import Combine

/// Installs and controls the system VPN profile backed by the packet-tunnel
/// extension. The transport config (URL / MAX creds) is passed to the extension
/// through the tunnel protocol's providerConfiguration.
@MainActor
final class VPNController: ObservableObject {
    @Published var status: String = "Disconnected"
    @Published var active = false

    private var manager: NETunnelProviderManager?
    private let extensionBundleId = "com.p1neapplexpress-saharev.openflux.tunnel"

    init() {
        NotificationCenter.default.addObserver(
            self, selector: #selector(statusChanged),
            name: .NEVPNStatusDidChange, object: nil)
        Task { await load() }
    }

    private func load() async {
        let managers = (try? await NETunnelProviderManager.loadAllFromPreferences()) ?? []
        manager = managers.first
        refreshStatus()
    }

    func start(transport: String, url: String, maxToken: String, maxUid: String,
               dns: String, tunnelUDP: Bool, split: String = "",
               directDomains: String = "", profileID: UUID? = nil,
               keySlot: String = "") {
        Task {
            let m = manager ?? NETunnelProviderManager()
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = extensionBundleId
            proto.serverAddress = "OpenFlux"
            proto.providerConfiguration = [
                "transport": transport, "url": url,
                "maxToken": maxToken, "maxUid": maxUid,
                "dns": dns,
                "udp": tunnelUDP ? "1" : "0",
                "split": split,   // "ru-direct" = GeoIP RU bypasses the tunnel
                "directDomains": directDomains,  // user's own bypass suffixes
                // Only the profile id — the encryption secret itself stays in the
                // shared Keychain and is fetched by the extension. This dictionary
                // is persisted with the VPN profile, so it must not hold secrets.
                "profileID": profileID?.uuidString ?? "",
                "keySlot": keySlot,   // "direct" = взять ключ прямого канала
            ]
            m.protocolConfiguration = proto
            m.localizedDescription = "OpenFlux"
            m.isEnabled = true
            // Auto-reconnect: with on-demand enabled, iOS relaunches the tunnel
            // whenever it drops (extension killed, network change, etc.) instead
            // of leaving the user to toggle it back on manually.
            m.isOnDemandEnabled = true
            m.onDemandRules = [NEOnDemandRuleConnect()]
            do {
                try await m.saveToPreferences()
                try await m.loadFromPreferences()   // required before starting
                self.manager = m
                try m.connection.startVPNTunnel()
            } catch {
                self.status = "Error: \(error.localizedDescription)"
            }
        }
    }

    func stop() {
        Task {
            // Disable on-demand first, otherwise iOS would immediately reconnect
            // the tunnel we're trying to stop.
            if let m = manager {
                m.isOnDemandEnabled = false
                try? await m.saveToPreferences()
                try? await m.loadFromPreferences()
            }
            manager?.connection.stopVPNTunnel()
        }
    }

    // MARK: - Log relay
    //
    // The tunnel runs in a separate process, so the app cannot read the Go core's
    // log directly — it asks the extension for it over the provider IPC channel.
    // Set `logSink` to receive lines; polling runs only while the tunnel is up.

    var logSink: ((String) -> Void)?

    /// Document URL waiting on an interactive captcha inside the extension, or
    /// nil. Polled over the same IPC channel as the log.
    @Published var captchaURL: String?
    /// Проверка в интересах узла: проходить НЕ отключая туннель.
    @Published var remoteCaptchaURL: String?

    private var logTimer: Timer?

    private func startLogPolling() {
        guard logTimer == nil else { return }
        logTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.pullLog() }
        }
    }

    private func stopLogPolling() {
        logTimer?.invalidate()
        logTimer = nil
    }

    private func pullLog() {
        guard let session = activeSession, let msg = "log".data(using: .utf8) else { return }
        // Throws if the extension isn't up yet; that is normal during startup.
        try? session.sendProviderMessage(msg) { [weak self] data in
            guard let data = data, !data.isEmpty,
                  let s = String(data: data, encoding: .utf8), !s.isEmpty
            else { return }
            Task { @MainActor in self?.logSink?(s) }
        }
        pullCaptcha(session)
        pullRemoteCaptcha(session)
    }

    private var activeSession: NETunnelProviderSession? {
        guard let s = manager?.connection as? NETunnelProviderSession,
              s.status == .connected || s.status == .reasserting
        else { return nil }
        return s
    }

    private func pullCaptcha(_ session: NETunnelProviderSession) {
        guard let msg = "captcha".data(using: .utf8) else { return }
        try? session.sendProviderMessage(msg) { [weak self] data in
            let url = data.flatMap { String(data: $0, encoding: .utf8) } ?? ""
            Task { @MainActor in
                self?.captchaURL = url.isEmpty ? nil : url
            }
        }
    }

    private func pullRemoteCaptcha(_ session: NETunnelProviderSession) {
        guard let msg = "remotecaptcha".data(using: .utf8) else { return }
        try? session.sendProviderMessage(msg) { [weak self] data in
            let url = data.flatMap { String(data: $0, encoding: .utf8) } ?? ""
            Task { @MainActor in self?.remoteCaptchaURL = url.isEmpty ? nil : url }
        }
    }

    /// Отдаёт узлу куки, добытые через туннель (проверка пройдена с его адреса).
    func offerCaptchaCookies(_ header: String) {
        guard let session = activeSession,
              let msg = "offer:\(header)".data(using: .utf8) else { return }
        try? session.sendProviderMessage(msg) { [weak self] data in
            let n = data.flatMap { String(data: $0, encoding: .utf8) } ?? "0"
            Task { @MainActor in
                self?.logSink?("[app] offered \(n) cookies to the exit node")
                self?.remoteCaptchaURL = nil
            }
        }
    }

    /// Relays cookies solved in the app's WebView into the extension's core.
    func applyCaptchaCookies(_ header: String) {
        guard let session = activeSession,
              let msg = "cookies:\(header)".data(using: .utf8) else { return }
        try? session.sendProviderMessage(msg) { [weak self] data in
            let n = data.flatMap { String(data: $0, encoding: .utf8) } ?? "0"
            Task { @MainActor in
                self?.logSink?("[app] captcha cookies handed to \(n) transport(s)")
                self?.captchaURL = nil
            }
        }
    }

    @objc private func statusChanged() { refreshStatus() }

    private func refreshStatus() {
        guard let conn = manager?.connection else { active = false; status = "Disconnected"; return }
        switch conn.status {
        case .connected:     status = "Connected";     active = true
        case .connecting:    status = "Connecting…";   active = true
        case .disconnecting: status = "Disconnecting…"; active = true
        case .reasserting:   status = "Reasserting…";  active = true
        default:             status = "Disconnected";  active = false
        }
        if conn.status == .connected || conn.status == .reasserting {
            startLogPolling()
        } else {
            stopLogPolling()
        }
    }
}
