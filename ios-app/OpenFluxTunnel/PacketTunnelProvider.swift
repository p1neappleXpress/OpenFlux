import NetworkExtension

/// System VPN entry point. Bridges the device's IP packets to the OpenFlux Go
/// tun2socks stack (TCP forwarded through the transport; DNS proxied over TCP).
class PacketTunnelProvider: NEPacketTunnelProvider {

    /// Networks that must NOT go through the tunnel: the Yandex backend the
    /// transport talks to, plus the DoT DNS resolvers. Otherwise the
    /// extension's own traffic loops back into itself.
    /// Домены, которые всегда идут мимо туннеля: собственный бэкенд транспорта
    /// и всё, из чего состоит страница капчи (её скрипты и статика живут на
    /// отдельных хостах). Попадают в тот же GeoSite-путь, что и RU-список, то
    /// есть их IP добавляются в excludedRoutes по факту DNS-ответа.
    static let alwaysDirectHosts = """
    yandex.ru
    yandex.com
    yandex.net
    yastatic.net
    captcha-api.yandex.ru
    smartcaptcha.yandexcloud.net
    passport.yandex.ru
    passport.yandex.com
    """

    /// Литерал IPv4 или имя хоста — от этого зависит, можно ли добавить
    /// статический /32 или надо ждать DNS-ответа.
    static func isIPv4(_ s: String) -> Bool {
        let parts = s.split(separator: ".")
        guard parts.count == 4 else { return false }
        return parts.allSatisfy { p in
            guard let v = Int(p), v >= 0, v <= 255, !p.isEmpty else { return false }
            return true
        }
    }

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
    /// True when there is any direct-domain list at all — the bundled RU set,
    /// the user's own suffixes, or both. Gates the GeoSite poller, which used to
    /// hang off the RU split alone.
    private var geoSiteActive = false

    /// Rolling tail of the Go core's log, drained on a timer so the app can pull
    /// it over IPC. The extension runs in its own process, so without this the
    /// app's log panel would only ever show its own lines.
    private var logTail: [String] = []
    private var logTimer: Timer?

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        let conf = (protocolConfiguration as? NETunnelProviderProtocol)?.providerConfiguration ?? [:]
        let transport = (conf["transport"] as? String) ?? "yandex"
        let url = (conf["url"] as? String) ?? ""
        let maxToken = (conf["maxToken"] as? String) ?? ""
        let maxUid = (conf["maxUid"] as? String) ?? ""
        let dnsSpec = (conf["dns"] as? String) ?? ""
        let tunnelUDP = (conf["udp"] as? String) == "1"
        let splitRU = (conf["split"] as? String) == "ru-direct"
        // User-added direct domains (newline-separated suffixes). They ride the
        // same GeoSite path as the bundled list, so they work with the RU split
        // off as well — hence they are read independently of `splitRU`.
        let customDirect = ((conf["directDomains"] as? String) ?? "")
            .trimmingCharacters(in: .whitespacesAndNewlines)

        // End-to-end encryption: the secret never rides in providerConfiguration
        // (which is persisted with the VPN profile), only the profile id does —
        // the secret itself comes from the Keychain group shared with the app.
        // Must be set before the core builds its transport stack.
        // Слот ключа: у докового профиля их два — свой ключ доков и ключ прямого
        // канала до узла. Спутать их значит отдать доковому транспорту чужой
        // ключ и молча ронять все пакеты.
        var encryptionKey = ""
        if let idString = conf["profileID"] as? String, let id = UUID(uuidString: idString) {
            if (conf["keySlot"] as? String) == "direct" {
                encryptionKey = Secrets.directKey(for: id) ?? ""
            } else {
                encryptionKey = Secrets.encryptionKey(for: id) ?? ""
            }
        }
        encryptionKey.withCString { k in
            OpenFluxSetEncryption(UnsafeMutablePointer(mutating: k))
        }

        // Куки капчи, пройденной с выключенным туннелем. Отдаём ДО старта, чтобы
        // первый же запрос к документу пошёл с ними и не упёрся в ту же капчу.
        let savedCookies = Secrets.captchaCookies() ?? ""
        savedCookies.withCString { c in
            OpenFluxSetInitialCookies(UnsafeMutablePointer(mutating: c))
        }

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
        // КРИТИЧНО для транспорта direct: адрес самого узла обязан идти мимо
        // туннеля. Иначе соединение расширения к узлу захватывается туннелем,
        // который оно же и поднимает, — петля, и не соединяется ничего. На маке
        // это не проявляется: там CLI-клиент отдаёт SOCKS5 без системного
        // туннеля, поэтому проверка проходила, а на телефоне всё вставало.
        var nodeRoutes: [NEIPv4Route] = []
        var nodeHostForDNS = ""
        if transport == "direct" {
            let hostPart = url.split(separator: ":").first.map(String.init) ?? url
            let host = hostPart.trimmingCharacters(in: .whitespaces)
            if !host.isEmpty {
                if Self.isIPv4(host) {
                    nodeRoutes.append(NEIPv4Route(destinationAddress: host,
                                                  subnetMask: "255.255.255.255"))
                } else {
                    // Имя, а не адрес: пустим его через тот же GeoSite-путь —
                    // маршрут добавится по DNS-ответу.
                    nodeHostForDNS = host
                }
            }
        }

        self.baseExcluded = Self.bypassRoutes + nodeRoutes + (splitRU ? Self.ruDirectRoutes : [])
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
        // The bundled RU set comes in only under the RU split; the user's own
        // suffixes are always appended, so they work on their own too.
        // Всегда напрямую — хосты, нужные самому транспорту и странице капчи.
        //
        // Без этого интерактивная капча нерешаема в принципе: она появляется
        // именно тогда, когда туннель НЕ работает, а весь трафик телефона идёт
        // в туннель. HTML ещё мог прийти через жёсткий список IP-диапазонов
        // ниже, но её скрипты с yastatic.net — уже нет, и WebView показывал
        // белый лист. Домены, а не IP: CDN меняет адреса, а имена стабильны.
        var geoLines: [String] = [Self.alwaysDirectHosts]
        if !nodeHostForDNS.isEmpty { geoLines.append(nodeHostForDNS) }
        if splitRU, let url = Bundle(for: PacketTunnelProvider.self)
            .url(forResource: "geosite-ru", withExtension: "txt"),
           let list = try? String(contentsOf: url, encoding: .utf8) {
            geoLines.append(list)
        }
        if !customDirect.isEmpty { geoLines.append(customDirect) }
        self.geoSiteActive = !geoLines.isEmpty
        if self.geoSiteActive {
            let merged = geoLines.joined(separator: "\n")
            merged.withCString { OpenFluxSetGeositeDirect(UnsafeMutablePointer(mutating: $0)) }
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
            if self.geoSiteActive { self.startGeoSiteMonitor() }
            self.startLogDrain()
            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        stopHealthMonitor()
        stopGeoSiteMonitor()
        stopLogDrain()
        OpenFluxStopPacketTunnel()
        completionHandler()
    }

    // MARK: - Log relay to the app
    //
    // OpenFluxReadLog() DRAINS the Go ring buffer, so it must be called from one
    // place only — here. Letting the app call it directly would race the two
    // readers and each would see half the lines.

    private func startLogDrain() {
        DispatchQueue.main.async {
            self.logTimer?.invalidate()
            self.logTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
                self?.drainLog()
            }
        }
    }

    private func stopLogDrain() {
        DispatchQueue.main.async {
            self.logTimer?.invalidate()
            self.logTimer = nil
        }
    }

    private func drainLog() {
        guard let c = OpenFluxReadLog() else { return }
        let s = String(cString: c)
        OpenFluxFreeString(c)
        guard !s.isEmpty else { return }
        logTail.append(contentsOf: s.split(separator: "\n").map(String.init))
        if logTail.count > 600 {
            logTail.removeFirst(logTail.count - 600)
        }
    }

    /// IPC from the containing app. "log" hands back everything accumulated since
    /// the last request and clears the tail, so the app can append incrementally.
    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)?) {
        let cmd = String(data: messageData, encoding: .utf8) ?? ""
        switch cmd {
        case "log":
            DispatchQueue.main.async {
                self.drainLog()                       // pick up the last second too
                let out = self.logTail.joined(separator: "\n")
                self.logTail.removeAll(keepingCapacity: true)
                completionHandler?(out.data(using: .utf8) ?? Data())
            }

        // The core lives in THIS process, so only the extension can see that a
        // SmartCaptcha is pending — and only it can hand the solved cookies to
        // the transport. The app drives the WebView and relays through here.
        case "remotecaptcha":
            var url = ""
            if let c = OpenFluxRemoteCaptchaPending() {
                url = String(cString: c)
                OpenFluxFreeString(c)
            }
            completionHandler?(url.data(using: .utf8) ?? Data())

        case "captcha":
            var url = ""
            if let c = OpenFluxCaptchaPending() {
                url = String(cString: c)
                OpenFluxFreeString(c)
            }
            completionHandler?(url.data(using: .utf8) ?? Data())

        default:
            if cmd.hasPrefix("offer:") {
                let raw = String(cmd.dropFirst("offer:".count))
                let n = raw.withCString { OpenFluxOfferCaptchaCookies(UnsafeMutablePointer(mutating: $0)) }
                completionHandler?("\(n)".data(using: .utf8) ?? Data())
                return
            }
            if cmd.hasPrefix("cookies:") {
                let raw = String(cmd.dropFirst("cookies:".count))
                let n = raw.withCString { OpenFluxApplyCaptchaCookies(UnsafeMutablePointer(mutating: $0)) }
                completionHandler?("\(n)".data(using: .utf8) ?? Data())
                return
            }
            completionHandler?(Data())
        }
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
