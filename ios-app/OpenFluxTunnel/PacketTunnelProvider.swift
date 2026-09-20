import NetworkExtension

/// System VPN entry point. Bridges the device's IP packets to the OpenFlux Go
/// packet transport (IPv4 TCP, opt-in UDP, local DNS-over-TLS).
class PacketTunnelProvider: NEPacketTunnelProvider {

    /// Go supplies both the inherited Yandex CIDRs and fresh /32 routes. Its
    /// carrier dialer enforces this exact snapshot, including after reconnect.
    private struct BypassRoute: Decodable {
        let destination: String
        let mask: String
    }

    private let lifecycle = DispatchQueue(label: "OpenFlux.packet.lifecycle")
    private let stateLock = NSLock()
    private var generation: UInt64 = 0
    private var running = false
    private var healthTimer: DispatchSourceTimer?
    private let writeGroup = DispatchGroup()

    private func isRunning(_ id: UInt64) -> Bool {
        stateLock.lock(); defer { stateLock.unlock() }
        return running && generation == id
    }

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        lifecycle.async { [weak self] in
            guard let self = self else { return }
            self.stateLock.lock()
            if self.running {
                self.stateLock.unlock()
                completionHandler(Self.failure(1, "Tunnel already running"))
                return
            }
            self.generation &+= 1
            let id = self.generation
            self.running = true
            self.stateLock.unlock()
            do {
                let conf = (self.protocolConfiguration as? NETunnelProviderProtocol)?.providerConfiguration ?? [:]
                let transport = (conf["transport"] as? String) ?? "yandex"
                let url = (conf["url"] as? String) ?? ""
                // Profiles created before V2 used legacy, never silently upgrade them.
                let codec = (conf["codec"] as? String) ?? "legacy"
                let udpForwarding = (conf["udpForwarding"] as? Bool) ?? false
                let maxUid = (conf["maxUid"] as? String) ?? ""
                guard ["yandex", "vyandex", "oneme"].contains(transport),
                      ["batched", "legacy"].contains(codec) else { throw SecretStoreError.invalidRecord }
                var key = ""
                var token = ""
                if let recordID = conf["credentialID"] as? String {
                    guard recordID.hasPrefix("vpn-"),
                          let data = try KeychainSecretStore().read(account: recordID) else { throw SecretStoreError.invalidRecord }
                    let record = try JSONDecoder().decode(TunnelCredentials.self, from: data)
                    guard record.transport == transport, record.url == url,
                          ((conf["encryptionEnabled"] as? Bool) ?? false) == !record.preparedKey.isEmpty else {
                        throw SecretStoreError.invalidRecord
                    }
                    key = record.preparedKey
                    token = record.maxToken
                } else if (conf["encryptionEnabled"] as? Bool) == true || transport == "oneme" {
                    // Missing Keychain never downgrades encryption or falls back to plaintext MAX credentials.
                    throw SecretStoreError.invalidRecord
                }

                // Bootstrap BEFORE capturing DNS/default routes: DoT preferred,
                // system fallback restricted to Yandex carrier endpoints only.
                let routeJSON = transport.withCString { tt in
                    url.withCString { u in
                        OpenFluxResolveBypassIPv4V2(UnsafeMutablePointer(mutating: tt), UnsafeMutablePointer(mutating: u))
                    }
                }
                guard let routeJSON = routeJSON else { throw Self.failure(7, "Cannot resolve transport bypass routes") }
                let routeData = Data(String(cString: routeJSON).utf8)
                OpenFluxFreeString(routeJSON)
                let routes = try JSONDecoder().decode([BypassRoute].self, from: routeData)
                guard !routes.isEmpty else { throw Self.failure(7, "Missing transport bypass routes") }
                let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
                let ipv4 = NEIPv4Settings(addresses: ["10.10.10.2"], subnetMasks: ["255.255.255.0"])
                ipv4.includedRoutes = [NEIPv4Route.default()]
                ipv4.excludedRoutes = routes.map {
                    NEIPv4Route(destinationAddress: $0.destination, subnetMask: $0.mask)
                }
                settings.ipv4Settings = ipv4
                settings.mtu = 1500
                let dns = NEDNSSettings(servers: ["198.18.0.1"])
                dns.matchDomains = [""]
                settings.dnsSettings = dns
                self.setTunnelNetworkSettings(settings) { [weak self] error in
                    guard let self = self else { return }
                    self.lifecycle.async {
                        guard self.isRunning(id) else {
                            completionHandler(Self.failure(8, "Tunnel start cancelled")); return
                        }
                        if error != nil {
                            self.finishStop()
                            completionHandler(Self.failure(9, "Cannot configure tunnel network settings")); return
                        }
                        let rc = transport.withCString { tt in
                            url.withCString { u in
                                token.withCString { tok in
                                    maxUid.withCString { uid in
                                        codec.withCString { c in
                                            key.withCString { k in
                                                OpenFluxStartPacketTunnelWithKeyV2(
                                                    UnsafeMutablePointer(mutating: tt), UnsafeMutablePointer(mutating: u),
                                                    UnsafeMutablePointer(mutating: tok), UnsafeMutablePointer(mutating: uid),
                                                    UnsafeMutablePointer(mutating: c), UnsafeMutablePointer(mutating: k))
                                            }
                                        }
                                    }
                                }
                            }
                        }
                        guard rc == 0 else {
                            self.finishStop()
                            completionHandler(Self.failure(Int(rc), "Transport failed to start")); return
                        }
                        OpenFluxTunSetUDPEnabled(udpForwarding ? 1 : 0)
                        self.startReadLoop(id)
                        self.startWriteLoop(id)
                        self.startHealthMonitor(id)
                        completionHandler(nil)
                    }
                }
            } catch {
                self.finishStop()
                completionHandler(Self.failure(10, "Cannot load VPN credentials or resolve transport endpoints"))
            }
        }
    }

    private static func failure(_ code: Int, _ message: String) -> NSError {
        NSError(domain: "OpenFlux", code: code, userInfo: [NSLocalizedDescriptionKey: message])
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        lifecycle.async { [weak self] in
            self?.finishStop()
            completionHandler()
        }
    }

    private func finishStop() {
        stateLock.lock()
        running = false
        generation &+= 1
        stateLock.unlock()
        healthTimer?.cancel()
        healthTimer = nil
        reasserting = false
        OpenFluxStopPacketTunnel()
        // An explicit new start cannot overlap an old Go reader. Reconnects
        // never reach this path and retain the same pair of packet-flow loops.
        writeGroup.wait()
    }

    private func startHealthMonitor(_ id: UInt64) {
        healthTimer?.cancel()
        let timer = DispatchSource.makeTimerSource(queue: lifecycle)
        timer.schedule(deadline: .now(), repeating: 1)
        timer.setEventHandler { [weak self] in
            guard let self = self, self.isRunning(id) else { return }
            // A relay outage never cancels the provider or starts new read/write loops.
            self.reasserting = OpenFluxPacketTunnelIsConnected() == 0
        }
        healthTimer = timer
        timer.resume()
    }

    private func startReadLoop(_ id: UInt64) {
        guard isRunning(id) else { return }
        packetFlow.readPackets { [weak self] packets, protocols in
            guard let self = self else { return }
            self.lifecycle.async {
                guard self.isRunning(id) else { return }
                for (p, proto) in zip(packets, protocols) where proto.int32Value == AF_INET {
                    guard p.count <= 65535 else { continue }
                    p.withUnsafeBytes { raw in
                        if let base = raw.bindMemory(to: CChar.self).baseAddress {
                            OpenFluxTunWritePacket(UnsafeMutablePointer(mutating: base), Int32(p.count))
                        }
                    }
                }
                self.startReadLoop(id)
            }
        }
    }

    private func startWriteLoop(_ id: UInt64) {
        writeGroup.enter()
        let group = writeGroup
        DispatchQueue.global(qos: .userInitiated).async { [weak self, group] in
            defer { group.leave() }
            let maxLen: Int32 = 65535
            let buf = UnsafeMutablePointer<CChar>.allocate(capacity: Int(maxLen))
            defer { buf.deallocate() }
            while self?.isRunning(id) == true {
                // The Go read blocks with a bounded timeout; 0 must not terminate
                // packetFlow or cause a busy loop during reconnect.
                let n = OpenFluxTunReadPacket(buf, maxLen)
                if n < 0 { break }
                if n == 0 { continue }
                guard let self = self, self.isRunning(id) else { break }
                autoreleasepool {
                    let data = Data(bytes: buf, count: Int(n))
                    self.packetFlow.writePackets([data], withProtocols: [NSNumber(value: AF_INET)])
                }
            }
        }
    }
}
