import NetworkExtension

/// System VPN entry point. Bridges the device's IP packets to the OpenFlux Go
/// tun2socks stack (TCP forwarded through the transport; DNS proxied over TCP).
class PacketTunnelProvider: NEPacketTunnelProvider {

    /// Networks that must NOT go through the tunnel: the Yandex backend the
    /// transport talks to, plus the DoT DNS resolvers. Otherwise the
    /// extension's own traffic loops back into itself.
    static let bypassRoutes: [NEIPv4Route] = {
        let cidrs: [(String, String)] = [
            ("5.45.192.0", "255.255.192.0"),
            ("5.255.192.0", "255.255.192.0"),
            ("37.9.64.0", "255.255.192.0"),
            ("37.140.128.0", "255.255.192.0"),
            ("77.88.0.0", "255.255.192.0"),
            ("84.201.128.0", "255.255.192.0"),
            ("87.250.224.0", "255.255.224.0"),
            ("90.156.176.0", "255.255.252.0"),
            ("93.158.128.0", "255.255.192.0"),
            ("95.108.128.0", "255.255.128.0"),
            ("100.43.64.0", "255.255.224.0"),
            ("178.154.128.0", "255.255.128.0"),
            ("213.180.192.0", "255.255.224.0"),
            // DoT DNS resolvers used by the Go client.
            ("8.8.8.8", "255.255.255.255"),
            ("1.1.1.1", "255.255.255.255"),
        ]
        return cidrs.map { NEIPv4Route(destinationAddress: $0.0, subnetMask: $0.1) }
    }()

    /// GeoIP split tunneling: routes for the RU address set that must bypass the
    /// exit node (go direct). Loaded lazily from the bundled ru-cidr.txt. Adding
    /// these to excludedRoutes makes RU-destined packets take the normal OS path
    /// instead of the tunnel — faster local access and less load on the covert
    /// channel. Only RU IPs are ever sent direct, so an incomplete list just
    /// tunnels some RU traffic (graceful) and never leaks a foreign IP to direct.
    static let ruDirectRoutes: [NEIPv4Route] = {
        guard let url = Bundle(for: PacketTunnelProvider.self)
            .url(forResource: "ru-cidr", withExtension: "txt"),
              let text = try? String(contentsOf: url, encoding: .utf8) else {
            return []
        }
        var routes: [NEIPv4Route] = []
        routes.reserveCapacity(3000)
        for line in text.split(separator: "\n") {
            let s = line.trimmingCharacters(in: .whitespaces)
            if s.isEmpty || s.hasPrefix("#") { continue }
            let parts = s.split(separator: "/")
            guard parts.count == 2, let prefix = Int(parts[1]),
                  prefix >= 0, prefix <= 32 else { continue }
            // prefix length -> dotted subnet mask
            let m = prefix == 0 ? UInt32(0) : (~UInt32(0) << (32 - prefix))
            let mask = "\((m >> 24) & 0xff).\((m >> 16) & 0xff).\((m >> 8) & 0xff).\(m & 0xff)"
            routes.append(NEIPv4Route(destinationAddress: String(parts[0]), subnetMask: mask))
        }
        return routes
    }()

    /// Drives `reasserting` from the Go transport's live state (issue #36).
    private var healthTimer: Timer?
    /// When the transport first went down; nil while it is up. A short grace on
    /// this avoids flapping `reasserting` for a sub-second reconnect.
    private var outageSince: Date?

    // GeoSite (phase 2): the tunnel settings and the static part of excludedRoutes
    // (Yandex/DoT bypass + optional GeoIP RU), so the geosite poller can re-apply
    // settings with the dynamically-discovered direct IPs appended.
    private var netSettings: NEPacketTunnelNetworkSettings?
    private var baseExcluded: [NEIPv4Route] = []
    private var dynamicDirectSeen = Set<String>()
    private var dynamicDirectRoutes: [NEIPv4Route] = []
    private var geoTimer: Timer?

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        let conf = (protocolConfiguration as? NETunnelProviderProtocol)?.providerConfiguration ?? [:]
        let transport = (conf["transport"] as? String) ?? "yandex"
        let url = (conf["url"] as? String) ?? ""
        let maxToken = (conf["maxToken"] as? String) ?? ""
        let maxUid = (conf["maxUid"] as? String) ?? ""
        let dnsSpec = (conf["dns"] as? String) ?? ""
        let tunnelUDP = (conf["udp"] as? String) == "1"
        let splitRU = (conf["split"] as? String) == "ru-direct"

        // Override the DNS-over-TLS upstream if the user configured one (empty =
        // built-in defaults). Must run in the extension process before start.
        dnsSpec.withCString { d in
            OpenFluxSetDoTResolver(UnsafeMutablePointer(mutating: d))
        }
        // UDP tunneling (default off = legacy-safe on any exit node).
        OpenFluxSetTunnelUDP(tunnelUDP ? 1 : 0)

        // Virtual interface: capture all IPv4 + all DNS.
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        // 10.10.10.2 is the address the exit node expects the client to use
        // (it hardcodes return packets to 10.10.10.2), enabling pure L3
        // forwarding with no gvisor stack in the extension.
        let ipv4 = NEIPv4Settings(addresses: ["10.10.10.2"], subnetMasks: ["255.255.255.0"])
        ipv4.includedRoutes = [NEIPv4Route.default()]
        // Exclude the transport's own backend (Yandex ranges) and the DoT DNS
        // servers so the extension's own connections bypass the tunnel instead
        // of looping back into it. With split tunneling on, also exclude the
        // GeoIP RU set so Russian destinations go direct.
        self.baseExcluded = Self.bypassRoutes + (splitRU ? Self.ruDirectRoutes : [])
        ipv4.excludedRoutes = self.baseExcluded
        settings.ipv4Settings = ipv4
        settings.mtu = 1500
        // A benign in-tunnel DNS address: queries to it are captured and
        // answered locally over DoT (the real resolvers are excluded above).
        let dns = NEDNSSettings(servers: ["198.18.0.1"])
        dns.matchDomains = [""]
        settings.dnsSettings = dns
        self.netSettings = settings

        // GeoSite: load the direct-domain list so the DNS proxy tags matching
        // answers; the poller (started after the core is up) routes them direct.
        if splitRU, let url = Bundle(for: PacketTunnelProvider.self)
            .url(forResource: "geosite-ru", withExtension: "txt"),
           let list = try? String(contentsOf: url, encoding: .utf8) {
            list.withCString { OpenFluxSetGeositeDirect(UnsafeMutablePointer(mutating: $0)) }
        }

        setTunnelNetworkSettings(settings) { error in
            if let error = error {
                completionHandler(error)
                return
            }
            let rc = transport.withCString { tt in
                url.withCString { u in
                    maxToken.withCString { tok in
                        maxUid.withCString { uid in
                            OpenFluxStartPacketTunnel(
                                UnsafeMutablePointer(mutating: tt),
                                UnsafeMutablePointer(mutating: u),
                                UnsafeMutablePointer(mutating: tok),
                                UnsafeMutablePointer(mutating: uid))
                        }
                    }
                }
            }
            if rc != 0 {
                completionHandler(NSError(domain: "OpenFlux", code: Int(rc),
                    userInfo: [NSLocalizedDescriptionKey: "start failed (\(rc))"]))
                return
            }
            self.startReadLoop()
            self.startWriteLoop()
            self.startHealthMonitor()
            if splitRU { self.startGeoSiteMonitor() }
            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        stopHealthMonitor()
        stopGeoSiteMonitor()
        OpenFluxStopPacketTunnel()
        completionHandler()
    }

    /// Polls the Go DNS proxy for GeoSite-matched direct IPs and appends them to
    /// excludedRoutes so those domains go direct — even on foreign CDN IPs not in
    /// the GeoIP RU set. Batched: settings are re-applied at most once per tick
    /// and only when a new IP appeared, so steady-state churn is zero. Re-applying
    /// tunnel settings does not touch the Go transport (the covert WebSocket), so
    /// it never trips the reasserting monitor.
    private func startGeoSiteMonitor() {
        DispatchQueue.main.async {
            self.geoTimer?.invalidate()
            self.geoTimer = Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
                self?.drainGeoSiteDirectIPs()
            }
        }
    }

    private func stopGeoSiteMonitor() {
        DispatchQueue.main.async {
            self.geoTimer?.invalidate()
            self.geoTimer = nil
        }
    }

    private func drainGeoSiteDirectIPs() {
        let cap = 16384
        var buf = [CChar](repeating: 0, count: cap)
        let n = Int(OpenFluxDrainDirectIPs(&buf, Int32(cap)))
        guard n > 0 else { return }
        let bytes = buf[0..<n].map { UInt8(bitPattern: $0) }
        let text = String(decoding: bytes, as: UTF8.self)

        var added = false
        for line in text.split(separator: "\n") {
            let ip = line.trimmingCharacters(in: .whitespaces)
            if ip.isEmpty || dynamicDirectSeen.contains(ip) { continue }
            dynamicDirectSeen.insert(ip)
            dynamicDirectRoutes.append(NEIPv4Route(destinationAddress: ip,
                                                   subnetMask: "255.255.255.255"))
            added = true
        }
        guard added, let settings = netSettings else { return }
        settings.ipv4Settings?.excludedRoutes = baseExcluded + dynamicDirectRoutes
        setTunnelNetworkSettings(settings) { _ in }
    }

    /// Polls the Go transport once a second and mirrors its up/down state into
    /// `reasserting`. When the Yandex WebSocket drops, the Go side reconnects on
    /// its own; without this, iOS reads the gap as a tunnel failure and tears the
    /// VPN down (issue #36). Holding `reasserting = true` across the gap keeps the
    /// tunnel alive (shown as "Reasserting…") and lets it resume when the
    /// transport is back — no cancelTunnelWithError, no stopping readPackets.
    private func startHealthMonitor() {
        DispatchQueue.main.async {
            self.healthTimer?.invalidate()
            self.healthTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
                guard let self = self else { return }
                if OpenFluxPacketTunnelConnected() != 0 {
                    self.outageSince = nil
                    if self.reasserting { self.reasserting = false }
                } else {
                    if self.outageSince == nil { self.outageSince = Date() }
                    if let s = self.outageSince,
                       Date().timeIntervalSince(s) > 3,
                       !self.reasserting {
                        self.reasserting = true
                    }
                }
            }
        }
    }

    private func stopHealthMonitor() {
        DispatchQueue.main.async {
            self.healthTimer?.invalidate()
            self.healthTimer = nil
            self.outageSince = nil
        }
    }

    /// Device -> Go stack.
    private func startReadLoop() {
        packetFlow.readPackets { [weak self] packets, _ in
            guard let self = self else { return }
            for p in packets {
                p.withUnsafeBytes { raw in
                    if let base = raw.bindMemory(to: CChar.self).baseAddress {
                        OpenFluxTunWritePacket(UnsafeMutablePointer(mutating: base), Int32(p.count))
                    }
                }
            }
            self.startReadLoop()
        }
    }

    /// Go stack -> device.
    private func startWriteLoop() {
        DispatchQueue.global(qos: .userInitiated).async {
            let maxLen: Int32 = 4096
            let buf = UnsafeMutablePointer<CChar>.allocate(capacity: Int(maxLen))
            defer { buf.deallocate() }
            while true {
                let n = OpenFluxTunReadPacket(buf, maxLen)
                if n <= 0 { break }
                let data = Data(bytes: buf, count: Int(n))
                self.packetFlow.writePackets([data], withProtocols: [NSNumber(value: AF_INET)])
            }
        }
    }
}
