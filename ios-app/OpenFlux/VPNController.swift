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

    func start(transport: String, url: String, maxToken: String, maxUid: String, dns: String) {
        Task {
            let m = manager ?? NETunnelProviderManager()
            let proto = NETunnelProviderProtocol()
            proto.providerBundleIdentifier = extensionBundleId
            proto.serverAddress = "OpenFlux"
            proto.providerConfiguration = [
                "transport": transport, "url": url,
                "maxToken": maxToken, "maxUid": maxUid,
                "dns": dns,
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
    }
}
