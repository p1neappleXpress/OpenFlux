import Foundation
import NetworkExtension
import Combine

/// Installs and controls the system VPN profile backed by the packet-tunnel
/// extension. Only non-secret settings and a Keychain record ID enter the profile.
@MainActor
final class VPNController: ObservableObject {
    @Published var status: String = "Disconnected"
    @Published var active = false
    @Published var connected = false
    @Published var loading = true

    private var manager: NETunnelProviderManager?
    private var startTask: Task<Void, Never>?
    private var preparing = false
    private let extensionBundleId = "com.p1neapplexpress-saharev.openflux.tunnel"

    init() {
        NotificationCenter.default.addObserver(
            self, selector: #selector(statusChanged),
            name: .NEVPNStatusDidChange, object: nil)
        Task { await load() }
    }

    private func load() async {
        defer { loading = false }
        let managers = (try? await NETunnelProviderManager.loadAllFromPreferences()) ?? []
        manager = managers.first { ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == extensionBundleId }
        refreshStatus()
    }

    func start(transport: String, url: String, maxToken: String, maxUid: String,
               codec: String, encryptionSecret: String, udpForwarding: Bool) {
        guard !loading, !active, !preparing else { return }
        preparing = true
        active = true
        status = "Preparing…"
        startTask = Task {
            defer { preparing = false; startTask = nil }
            let store = KeychainSecretStore()
            let recordID = "vpn-" + UUID().uuidString
            var profileSaved = false
            do {
                let secret = encryptionSecret.trimmingCharacters(in: .whitespacesAndNewlines)
                // scrypt's 32 MiB scratch is allocated in the containing app,
                // never in NE. All subsequent starts can run while the app is closed.
                let key = try await Task.detached(priority: .userInitiated) { () throws -> String in
                    if secret.isEmpty { return "" }
                    guard secret.unicodeScalars.count >= 16 else { throw SecretStoreError.invalidSecret }
                    return try transport.withCString { tt in
                        try url.withCString { u in
                            try secret.withCString { s in
                                guard let value = OpenFluxDeriveEncryptionKey(
                                    UnsafeMutablePointer(mutating: tt), UnsafeMutablePointer(mutating: u),
                                    UnsafeMutablePointer(mutating: s)) else { throw SecretStoreError.invalidSecret }
                                defer { OpenFluxFreeString(value) }
                                return String(cString: value)
                            }
                        }
                    }
                }.value
                try Task.checkCancellation()
                let record = TunnelCredentials(transport: transport, url: url, preparedKey: key, maxToken: maxToken)
                try store.write(JSONEncoder().encode(record), account: recordID)
                let m = manager ?? NETunnelProviderManager()
                let oldID = (m.protocolConfiguration as? NETunnelProviderProtocol)?.providerConfiguration?["credentialID"] as? String
                let proto = NETunnelProviderProtocol()
                proto.providerBundleIdentifier = extensionBundleId
                proto.serverAddress = "OpenFlux"
                proto.disconnectOnSleep = false
                proto.providerConfiguration = ["transport": transport, "url": url,
                    "codec": codec, "maxUid": maxUid, "credentialID": recordID,
                    "encryptionEnabled": !key.isEmpty, "udpForwarding": udpForwarding]
                m.protocolConfiguration = proto
                m.localizedDescription = "OpenFlux"
                m.isEnabled = true
                try await m.saveToPreferences()
                profileSaved = true
                try await m.loadFromPreferences()
                self.manager = m
                if let oldID = oldID, oldID != recordID, oldID.hasPrefix("vpn-") { try? store.delete(account: oldID) }
                try Task.checkCancellation()
                try m.connection.startVPNTunnel()
                preparing = false
                refreshStatus()
            } catch {
                if !profileSaved { try? store.delete(account: recordID) }
                // Framework errors may contain profile fields; never interpolate them.
                self.status = Task.isCancelled ? "Disconnected" : "Cannot start VPN. Check settings, Keychain and signing."
                self.active = false
            }
        }
    }

    func stop() {
        startTask?.cancel()
        manager?.connection.stopVPNTunnel()
    }

    @objc private func statusChanged() { refreshStatus() }

    private func refreshStatus() {
        guard !preparing else { return }
        connected = manager?.connection.status == .connected
        guard let conn = manager?.connection else { active = false; status = "Disconnected"; return }
        switch conn.status {
        case .connected:     status = "Connected";     active = true
        case .connecting:    status = "Connecting…";   active = true
        case .disconnecting: status = "Disconnecting…"; active = true
        case .reasserting:   status = "Reconnecting…";  active = true
        default:             status = "Disconnected";  active = false
        }
    }
}
