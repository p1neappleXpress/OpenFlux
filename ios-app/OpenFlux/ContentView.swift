import SwiftUI
import UIKit

/// DoT resolver spec for the Go core ("" = built-in defaults).
func dotSpec(_ preset: String, _ custom: String) -> String {
    switch preset {
    case "cloudflare": return "1.1.1.1@cloudflare-dns.com"
    case "google":     return "8.8.8.8@dns.google"
    case "quad9":      return "9.9.9.9@dns.quad9.net"
    case "adguard":    return "94.140.14.14@dns.adguard-dns.com"
    case "custom":     return custom.trimmingCharacters(in: .whitespaces)
    default:           return ""
    }
}

struct ContentView: View {
    @StateObject private var tunnel = TunnelController()
    @StateObject private var vpn = VPNController()
    @StateObject private var store = ProfileStore()
    @StateObject private var directDomains = DirectDomainStore()
    @StateObject private var ping = PingService()

    // Legacy single-config fields, kept only to migrate old installs into a Profile.
    @AppStorage("transportKind") private var transportRaw: String = TransportKind.yandex.rawValue
    @AppStorage("docURL") private var docURL: String = ""
    @AppStorage("docURL2") private var docURL2: String = ""
    @AppStorage("volgaURL") private var volgaURL: String = ""
    @AppStorage("mailURL") private var mailURL: String = ""
    @AppStorage("maxToken") private var legacyMaxToken: String = ""
    @AppStorage("maxUid") private var legacyMaxUid: String = ""

    // Global settings (apply to whichever profile is connected).
    @AppStorage("socksPort") private var socksPort: String = "10808"
    @AppStorage("debugLog") private var debugLog: Bool = false
    @AppStorage("splitRU") private var splitRU: Bool = false
    @AppStorage("dnsPreset") private var dnsPreset: String = "default"
    @AppStorage("dnsCustom") private var dnsCustom: String = ""
    @AppStorage("tunnelUDP") private var tunnelUDP: Bool = false

    @State private var showInfo = false
    @State private var showSettings = false
    /// Обёртка для .sheet(item:). С .sheet(isPresented:) редактор получал nil
    /// вместо профиля: SwiftUI строит содержимое шита по состоянию на момент
    /// показа, а `editing = sel`, выставленный в том же действии, до него не
    /// доезжает — форма открывалась пустой, и профиль приходилось вводить
    /// заново. nil внутри = создание нового.
    private struct EditorTask: Identifiable {
        let id = UUID()
        let profile: Profile?
    }
    @State private var editorTask: EditorTask?
    @State private var sharing: Profile?      // profile shown as a QR
    @State private var testHint: String?
    /// Обёртка для .sheet(item:). С .sheet(isPresented:) и `if let` внутри тело
    /// шита становилось пустым (белый экран), как только опрос обнулял URL —
    /// item-вариант захватывает значение на момент показа.
    private struct CaptchaTask: Identifiable {
        let id = UUID()
        let url: URL
        /// true — проверка за узел: туннель не гасим, куки отдаём ему.
        var forPeer = false
    }
    @State private var captchaTask: CaptchaTask?
    /// VPN был включён и его выключили ради капчи — значит после передачи кук
    /// его надо поднять обратно.
    @State private var resumeVPNAfterCaptcha = false

    /// Сценарий «решить капчу доков через прямой канал».
    ///
    /// Смысл: сидя на доках, капчу решить НЕЛЬЗЯ — это и есть заблокированный
    /// носитель. Нужен рабочий канал, выходящий с адреса узла. Поэтому
    /// переключаемся на direct-профиль, проходим проверку, отдаём куки узлу и
    /// возвращаемся обратно на доки — всё одной кнопкой.
    private enum DirectCaptchaStage { case idle, connecting, solving }
    @State private var dcStage: DirectCaptchaStage = .idle
    @State private var dcRestoreProfile: UUID?
    @State private var dcDoc: URL?

    /// Отдельный профиль Прямой TCP — запасной путь, если в доковом профиле
    /// адрес узла не заполнен.
    private var directProfile: Profile? {
        store.profiles.first { $0.transportKind == .direct && $0.isValid }
    }

    /// Прямой канал, настроенный ВНУТРИ выбранного докового профиля.
    private var inlineDirect: (addr: String, id: UUID)? {
        guard let p = store.selected,
              p.transportKind == .yandex || p.transportKind == .volga,
              let addr = p.nodeAddr?.trimmingCharacters(in: .whitespaces), !addr.isEmpty,
              Secrets.directKey(for: p.id) != nil
        else { return nil }
        return (addr, p.id)
    }

    private var dnsSpec: String { dotSpec(dnsPreset, dnsCustom) }

    /// Документ, упёршийся в интерактивную капчу. Ядро живёт либо в расширении
    /// (системный VPN), либо в самом приложении (локальный прокси) — берём
    /// оттуда, где оно сейчас работает.
    /// Проверка, которую надо пройти В ИНТЕРЕСАХ УЗЛА. Принципиально другой
    /// случай: туннель гасить нельзя, потому что проверка привязывается к адресу,
    /// с которого её прошли, а куки нужны годные для адреса узла. Значит выходить
    /// надо через туннель.
    private var remoteCaptchaTarget: URL? {
        let raw = vpn.active ? vpn.remoteCaptchaURL : tunnel.remoteCaptchaURL
        guard let raw = raw, let u = URL(string: raw) else { return nil }
        return u
    }

    /// Документ Яндекса из любого профиля — цель для ручного прохождения капчи
    /// в интересах узла. Нужен потому, что узел может быть вообще не в состоянии
    /// попросить: если его носитель заблокирован, канала нет. А подключившись
    /// другим носителем (direct/boards), выход всё равно идёт с адреса узла —
    /// и капчу можно пройти за него заранее.
    private var yandexDocForPeer: URL? {
        for p in store.profiles where p.transportKind == .yandex || p.transportKind == .volga {
            let first = p.url.split(separator: ",").first.map(String.init) ?? ""
            if let u = URL(string: first.trimmingCharacters(in: .whitespaces)), u.host != nil {
                return u
            }
        }
        return nil
    }

    private var captchaTarget: URL? {
        let raw = vpn.active ? vpn.captchaURL : tunnel.captchaURL
        guard let raw = raw, let u = URL(string: raw) else { return nil }
        return u
    }

    // MARK: connection state

    private var statusColor: Color {
        if vpn.status.contains("Connected") { return .green }
        if vpn.active { return .orange }          // connecting / reasserting / disconnecting
        return Color(.systemGray3)
    }

    private var statusText: String {
        let s = vpn.status
        if s.contains("Connected") { return "Подключено" }
        if s.contains("Reasserting") { return "Переподключение…" }
        if s.contains("Connecting") { return "Подключение…" }
        if s.contains("Disconnecting") { return "Отключение…" }
        if s.hasPrefix("Error") { return "Ошибка" }
        return "Отключено"
    }

    private func toggleConnect() {
        if vpn.active {
            vpn.stop()
        } else if let p = store.selected, p.isValid {
            tunnel.stop() // in-app core and system VPN can't share a document
            vpn.start(transport: p.transport, url: p.url,
                      maxToken: p.maxToken, maxUid: p.maxUid,
                      dns: dnsSpec, tunnelUDP: tunnelUDP,
                      split: splitRU ? "ru-direct" : "",
                      directDomains: directDomains.joined,
                      profileID: p.id)
        }
    }

    private func checkAvailability() {
        if vpn.active {
            tunnel.testDirect()
            testHint = "Проверка через VPN — результат в логе (Настройки)."
        } else if tunnel.running {
            tunnel.testThroughProxy()
            testHint = "Проверка через локальный прокси — результат в логе."
        } else if let p = store.selected, p.isValid {
            tunnel.start(transport: p.transportKind, url: p.url,
                         maxToken: p.maxToken, maxUid: p.maxUid,
                         port: Int(socksPort) ?? 10808,
                         encryptionKey: Secrets.encryptionKey(for: p.id) ?? "")
            testHint = "Локальный прокси запущен — нажмите ещё раз для проверки."
        }
    }

    // MARK: body

    var body: some View {
        NavigationView {
            ScrollView {
                VStack(spacing: 22) {
                    Text(store.profiles.isEmpty
                         ? "Добавьте профиль: вставьте ссылку на документ или отсканируйте QR"
                         : "Выберите профиль и нажмите кнопку подключения")
                        .font(.footnote).foregroundColor(.secondary)
                        .multilineTextAlignment(.center)
                        .frame(maxWidth: .infinity)

                    connectButton

                    Text(statusText).font(.headline)

                    profilePicker

                    HStack(spacing: 10) {
                        Button {
                            checkAvailability()
                        } label: {
                            Label("Доступность", systemImage: "waveform.path.ecg")
                                .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.bordered)
                        .disabled(store.selected == nil || !(store.selected?.isValid ?? false))

                        Button {
                            ping.pingAll(store.profiles)
                        } label: {
                            Label("Пинг всех", systemImage: "dot.radiowaves.left.and.right")
                                .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.bordered)
                        .disabled(store.profiles.isEmpty)
                    }

                    if let hint = testHint {
                        Text(hint).font(.caption2).foregroundColor(.secondary)
                            .multilineTextAlignment(.center)
                    }

                    solveViaDirectButton

                    manualPeerCaptcha

                    remoteCaptchaBanner

                    captchaBanner

                    logPanel
                }
                .padding()
            }
            .navigationTitle("OpenFlux")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button { showSettings = true } label: { Image(systemName: "gearshape") }
                }
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button { showInfo = true } label: { Image(systemName: "info.circle") }
                }
            }
            .onChange(of: vpn.status) { st in
                if dcStage == .connecting, st.contains("Connected"), let doc = dcDoc {
                    dcStage = .solving
                    captchaTask = CaptchaTask(url: doc, forPeer: true)
                }
            }
            .onAppear {
                OpenFluxSetDebug(debugLog ? 1 : 0)
                dnsSpec.withCString { OpenFluxSetDoTResolver(UnsafeMutablePointer(mutating: $0)) }
                migrateLegacyIfNeeded()
                // Pipe the extension's log (separate process) into the same panel
                // as the in-app core's.
                vpn.logSink = { [weak tunnel] line in tunnel?.appendExternal(line) }
            }
            .sheet(item: $captchaTask) { task in
                CaptchaView(url: task.url) { header in
                    if task.forPeer {
                        handlePeerCaptchaCookies(header)
                    } else {
                        handleCaptchaCookies(header)
                    }
                }
            }
            .sheet(isPresented: $showInfo) { InfoView() }
            .sheet(isPresented: $showSettings) {
                SettingsSheet(socksPort: $socksPort, debugLog: $debugLog,
                              splitRU: $splitRU, dnsPreset: $dnsPreset,
                              dnsCustom: $dnsCustom, tunnelUDP: $tunnelUDP,
                              tunnel: tunnel, vpn: vpn,
                              directDomains: directDomains,
                              selectedProfile: store.selected,
                              port: Int(socksPort) ?? 10808)
            }
            .sheet(item: $editorTask) { task in
                ProfileEditorView(profile: task.profile) { saved in
                    store.upsert(saved)
                }
            }
            .sheet(item: $sharing) { p in
                ShareQRView(profile: p)
            }
        }
        .navigationViewStyle(.stack)
    }

    // MARK: interactive captcha

    /// Одна кнопка на весь сценарий: доки → прямой канал → капча → куки узлу →
    /// обратно на доки. Видна, когда выбран доковый профиль и есть куда сходить.
    @ViewBuilder
    private var solveViaDirectButton: some View {
        if let sel = store.selected,
           sel.transportKind == .yandex || sel.transportKind == .volga,
           (inlineDirect != nil || directProfile != nil),
           yandexDocForPeer != nil, dcStage == .idle {
            Button {
                startCaptchaViaDirect()
            } label: {
                Label("Решить капчу через прямой канал", systemImage: "arrow.triangle.swap")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.bordered)
        } else if dcStage == .connecting {
            HStack(spacing: 8) {
                ProgressView()
                Text("Переключаюсь на прямой канал…").font(.caption2).foregroundColor(.secondary)
            }
        }
    }

    private func startCaptchaViaDirect() {
        guard let doc = yandexDocForPeer else { return }
        dcDoc = doc
        dcRestoreProfile = store.selectedID
        dcStage = .connecting

        if let inline = inlineDirect {
            // Адрес и ключ лежат в самом доковом профиле — отдельный профиль не
            // нужен. Ключ берётся из своего слота Keychain (keySlot: "direct"),
            // чтобы не спутать его с ключом самих документов.
            if vpn.active { vpn.stop() }
            DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) {
                vpn.start(transport: "direct", url: inline.addr,
                          maxToken: "", maxUid: "",
                          dns: dnsSpec, tunnelUDP: tunnelUDP,
                          split: splitRU ? "ru-direct" : "",
                          directDomains: directDomains.joined,
                          profileID: inline.id, keySlot: "direct")
            }
        } else if let d = directProfile {
            store.select(d.id)
            if vpn.active { vpn.stop() }
            DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { toggleConnect() }
        } else {
            dcStage = .idle
            return
        }
        // Страховка от зависания: если за 40 с не поднялось — выходим из сценария.
        DispatchQueue.main.asyncAfter(deadline: .now() + 40) {
            if dcStage == .connecting {
                dcStage = .idle
                testHint = "Прямой канал не поднялся — проверьте профиль и ключ."
            }
        }
    }

    /// Возврат на доковый профиль после передачи кук.
    private func finishCaptchaViaDirect() {
        guard dcStage == .solving, let back = dcRestoreProfile else { return }
        dcStage = .idle
        dcRestoreProfile = nil
        store.select(back)
        vpn.stop()
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { toggleConnect() }
        testHint = "Куки переданы узлу — возвращаюсь на документы."
    }

    /// Ручной запуск проверки за узел. Виден, когда туннель поднят и есть
    /// yandex-профиль: тогда выход идёт с адреса узла, и выданная spravka (в неё
    /// зашит IP) будет годна именно ему.
    @ViewBuilder
    private var manualPeerCaptcha: some View {
        if vpn.active, remoteCaptchaTarget == nil, let u = yandexDocForPeer {
            Button {
                captchaTask = CaptchaTask(url: u, forPeer: true)
            } label: {
                Label("Пройти капчу Яндекса за узел", systemImage: "shield.lefthalf.filled")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.bordered)
        }
    }

    /// Баннер для капчи УЗЛА: туннель остаётся поднятым.
    @ViewBuilder
    private var remoteCaptchaBanner: some View {
        if let u = remoteCaptchaTarget {
            VStack(alignment: .leading, spacing: 8) {
                Label("Узел не может пройти проверку", systemImage: "arrow.triangle.2.circlepath.circle")
                    .font(.subheadline.bold())
                    .foregroundColor(.blue)
                Text("Пройдите её, НЕ отключая туннель: выход пойдёт с адреса узла, и куки подойдут именно ему. Приложение передаст их узлу само.")
                    .font(.caption2).foregroundColor(.secondary)
                Button {
                    captchaTask = CaptchaTask(url: u, forPeer: true)
                } label: {
                    Label("Пройти за узел", systemImage: "hand.tap")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
            }
            .padding(12)
            .background(Color.blue.opacity(0.12))
            .clipShape(RoundedRectangle(cornerRadius: 10))
        }
    }

    @ViewBuilder
    private var captchaBanner: some View {
        if captchaTarget != nil {
            VStack(alignment: .leading, spacing: 8) {
                Label("Яндекс требует проверку", systemImage: "exclamationmark.shield.fill")
                    .font(.subheadline.bold())
                    .foregroundColor(.orange)
                Text("Эту капчу нельзя пройти автоматически. Откройте её, пройдите проверку — приложение само подхватит куки и переподключится.")
                    .font(.caption2).foregroundColor(.secondary)
                Button {
                    startCaptchaFlow()
                } label: {
                    Label("Пройти проверку", systemImage: "hand.tap")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
            }
            .padding(12)
            .background(Color.orange.opacity(0.12))
            .clipShape(RoundedRectangle(cornerRadius: 10))
        }
    }

    /// Пакеты в мёртвый туннель — чёрная дыра: страница капчи не грузится и не
    /// падает, просто висит белым листом. Поэтому на время проверки туннель
    /// гасим (он всё равно нерабочий — из-за этой самой капчи), а потом
    /// поднимаем. Куки при этом должны пережить перезапуск, за это отвечает
    /// Keychain.
    private func startCaptchaFlow() {
        guard let u = captchaTarget else { return }
        if vpn.active {
            resumeVPNAfterCaptcha = true
            vpn.stop()
            // Даём iOS снять маршруты, иначе WebView стартует ещё в туннеле.
            DispatchQueue.main.asyncAfter(deadline: .now() + 1.2) {
                captchaTask = CaptchaTask(url: u)
            }
        } else {
            resumeVPNAfterCaptcha = false
            captchaTask = CaptchaTask(url: u)
        }
    }

    /// Куки, добытые ЧЕРЕЗ туннель, — они годны для адреса узла, а не нашего,
    /// поэтому локально их не применяем и в Keychain не кладём.
    private func handlePeerCaptchaCookies(_ header: String) {
        if vpn.active {
            vpn.offerCaptchaCookies(header)
        } else {
            tunnel.offerCaptchaCookies(header)
        }
        testHint = "Куки отправлены узлу."
        finishCaptchaViaDirect()
    }

    private func handleCaptchaCookies(_ header: String) {
        // Сохраняем всегда: если ядра сейчас нет (туннель погашен ради капчи),
        // это единственный способ донести куки до следующего старта.
        Secrets.setCaptchaCookies(header)

        if vpn.active {
            vpn.applyCaptchaCookies(header)
        } else if tunnel.running {
            tunnel.applyCaptchaCookies(header)
        }

        if resumeVPNAfterCaptcha {
            resumeVPNAfterCaptcha = false
            testHint = "Проверка пройдена — поднимаю туннель заново."
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { toggleConnect() }
        }
    }

    // MARK: log panel

    private var logPanel: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Label("Журнал", systemImage: "text.alignleft")
                    .font(.subheadline.bold())
                Spacer()
                Button {
                    UIPasteboard.general.string = tunnel.log
                } label: {
                    Image(systemName: "doc.on.doc")
                }
                .disabled(tunnel.log.isEmpty)
                Button {
                    tunnel.clearLog()
                } label: {
                    Label("Очистить", systemImage: "trash")
                        .labelStyle(.titleAndIcon)
                        .font(.caption)
                }
                .disabled(tunnel.log.isEmpty)
            }

            ScrollViewReader { proxy in
                ScrollView {
                    Text(tunnel.log.isEmpty ? "Пусто" : tunnel.log)
                        .font(.system(.caption2, design: .monospaced))
                        .foregroundColor(tunnel.log.isEmpty ? .secondary : .primary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .textSelection(.enabled)
                        .id("logtail-main")
                }
                .onChange(of: tunnel.log) { _ in
                    withAnimation { proxy.scrollTo("logtail-main", anchor: .bottom) }
                }
            }
            .frame(height: 220)
            .padding(8)
            .background(Color(.secondarySystemBackground))
            .clipShape(RoundedRectangle(cornerRadius: 10))

            if vpn.active {
                Text("Строки туннеля приходят из расширения раз в секунду.")
                    .font(.caption2).foregroundColor(.secondary)
            }
        }
    }

    // MARK: central connect button

    private var connectButton: some View {
        Button { toggleConnect() } label: {
            ZStack {
                Circle()
                    .stroke(statusColor.opacity(0.25), lineWidth: 10)
                    .frame(width: 168, height: 168)
                Circle()
                    .fill(statusColor.opacity(0.12))
                    .frame(width: 148, height: 148)
                VStack(spacing: 8) {
                    Image(systemName: vpn.active ? "bolt.slash.fill" : "power")
                        .font(.system(size: 44, weight: .semibold))
                        .foregroundColor(statusColor)
                    Text(vpn.active ? "Отключить" : "Подключить")
                        .font(.subheadline).foregroundColor(.secondary)
                }
            }
        }
        .buttonStyle(.plain)
        .disabled(!vpn.active && !(store.selected?.isValid ?? false))
    }

    // MARK: profile dropdown

    @ViewBuilder
    private var profilePicker: some View {
        if store.profiles.isEmpty {
            Button {
                editorTask = EditorTask(profile: nil)
            } label: {
                Label("Добавить профиль", systemImage: "plus.circle.fill")
                    .frame(maxWidth: .infinity).padding(.vertical, 6)
            }
            .buttonStyle(.borderedProminent)
        } else {
            profileMenu
        }
    }

    private var profileMenu: some View {
        Menu {
            ForEach(store.profiles) { p in
                Button {
                    store.select(p.id)
                    ping.ping(p)          // выбрал — сразу меряем
                } label: {
                    Label(pingLabel(for: p),
                          systemImage: p.id == store.selectedID ? "checkmark" : "")
                }
            }
            if !store.profiles.isEmpty { Divider() }
            Button {
                ping.pingAll(store.profiles)
            } label: { Label("Пинговать все", systemImage: "dot.radiowaves.left.and.right") }
            if let sel = store.selected {
                Button {
                    editorTask = EditorTask(profile: sel)
                } label: { Label("Изменить «\(sel.name)»", systemImage: "pencil") }
                Button {
                    sharing = sel
                } label: { Label("Поделиться (QR)", systemImage: "qrcode") }
                Button(role: .destructive) {
                    store.delete(sel)
                } label: { Label("Удалить «\(sel.name)»", systemImage: "trash") }
            }
            Button {
                editorTask = EditorTask(profile: nil)
            } label: { Label("Добавить профиль…", systemImage: "plus") }
        } label: {
            HStack(spacing: 10) {
                Image(systemName: transportIcon(store.selected?.transport))
                    .foregroundColor(.secondary)
                VStack(alignment: .leading, spacing: 2) {
                    Text(store.selected?.name ?? "Нет профиля")
                        .font(.subheadline).bold()
                        .foregroundColor(.primary)
                        .lineLimit(1)
                    Text(store.selected?.subtitle ?? "Добавьте профиль или вставьте ссылку")
                        .font(.caption2).foregroundColor(.secondary)
                        .lineLimit(1).truncationMode(.middle)
                }
                Spacer(minLength: 8)
                if let sel = store.selected, let o = ping.outcome(for: sel) {
                    Text(o.short)
                        .font(.caption2).monospacedDigit()
                        .foregroundColor(pingColor(o))
                }
                Image(systemName: "chevron.up.chevron.down")
                    .font(.caption).foregroundColor(.secondary)
            }
            .padding(.horizontal, 14).padding(.vertical, 12)
            .frame(maxWidth: .infinity)
            .background(Color(.secondarySystemBackground))
            .clipShape(RoundedRectangle(cornerRadius: 12))
            .contentShape(Rectangle())
        }
        .disabled(vpn.active)
    }

    /// Row title in the dropdown: name plus its last ping, so a config can be
    /// compared without selecting it first.
    private func pingLabel(for p: Profile) -> String {
        guard let o = ping.outcome(for: p) else { return p.name }
        return "\(p.name) — \(o.short)"
    }

    private func pingColor(_ o: PingService.Outcome) -> Color {
        switch o {
        case .running:      return .secondary
        case .failed:       return .red
        case .ms(let v):    return v < 400 ? .green : (v < 1200 ? .orange : .red)
        }
    }

    private func transportIcon(_ t: String?) -> String {
        switch t {
        case "mailru": return "envelope"
        case "volga":  return "waveform"
        case "boards": return "rectangle.3.group"
        case "direct": return "arrow.left.arrow.right"
        case "oneme":  return "m.square"
        default:       return "doc.text"
        }
    }

    private func migrateLegacyIfNeeded() {
        guard store.profiles.isEmpty else { return }
        let kind = TransportKind(rawValue: transportRaw) ?? .yandex
        var url = ""
        switch kind {
        case .yandex:
            let a = docURL.trimmingCharacters(in: .whitespaces)
            let b = docURL2.trimmingCharacters(in: .whitespaces)
            url = b.isEmpty ? a : "\(a),\(b)"
        case .volga: url = volgaURL.trimmingCharacters(in: .whitespaces)
        // boards появился уже после профилей — легаси-конфига для него не бывает
        case .boards, .direct: break
        case .mail:  url = mailURL.trimmingCharacters(in: .whitespaces)
        case .max:   break
        }
        let hasMax = !legacyMaxToken.isEmpty && !legacyMaxUid.isEmpty
        guard !url.isEmpty || hasMax else { return }
        store.upsert(Profile(name: kind.title, transport: kind.rawValue, url: url,
                             maxToken: legacyMaxToken, maxUid: legacyMaxUid))
    }
}

// MARK: - Profile editor

struct ProfileEditorView: View {
    let profile: Profile?
    let onSave: (Profile) -> Void
    @Environment(\.dismiss) private var dismiss

    @State private var id = UUID()
    @State private var name = ""
    @State private var transportRaw = TransportKind.yandex.rawValue
    @State private var url1 = ""
    @State private var url2 = ""
    @State private var single = ""
    @State private var maxToken = ""
    @State private var maxUid = ""
    @State private var encryptionKey = ""
    @State private var nodeAddr = ""
    @State private var directKey = ""
    @State private var importMsg: String?
    @State private var showScanner = false

    private var transport: TransportKind { TransportKind(rawValue: transportRaw) ?? .yandex }

    var body: some View {
        NavigationView {
            Form {
                Section("Профиль") {
                    TextField("Название", text: $name)
                    Picker("Транспорт", selection: $transportRaw) {
                        ForEach(TransportKind.allCases) { t in Text(t.title).tag(t.rawValue) }
                    }
                    if transport == .direct {
                        Text("Прямой TCP до узла: в поле ниже — его host:port, ключ шифрования обязателен. Канал не скрытый: адрес узла виден.")
                            .font(.caption2).foregroundColor(.secondary)
                    }
                }

                Section("Подключение") {
                    switch transport {
                    case .yandex:
                        TextField("https://disk.yandex.ru/i/…", text: $url1).autocapitalization(.none).disableAutocorrection(true)
                        TextField("второй документ (необязательно)", text: $url2).autocapitalization(.none).disableAutocorrection(true)
                    case .volga:
                        TextField("https://disk.yandex.ru/i/…", text: $single).autocapitalization(.none).disableAutocorrection(true)
                    case .boards:
                        TextField("https://boards.yandex.ru/whiteboard/?hash=…", text: $single).autocapitalization(.none).disableAutocorrection(true)
                    case .direct:
                        TextField("адрес узла, например 1.2.3.4:9443", text: $single).autocapitalization(.none).disableAutocorrection(true)
                    case .mail:
                        TextField("https://cloud.mail.ru/public/…", text: $single).autocapitalization(.none).disableAutocorrection(true)
                    case .max:
                        TextField("MAX token", text: $maxToken).autocapitalization(.none).disableAutocorrection(true)
                        TextField("MAX user ID", text: $maxUid).keyboardType(.numberPad)
                    }
                }

                Section("Шифрование (AES-256-GCM)") {
                    SecureField("Общий секрет (от 16 символов)", text: $encryptionKey)
                        .autocapitalization(.none)
                        .disableAutocorrection(true)
                    if encryptionKey.isEmpty {
                        Text(transport == .direct
                             ? "Для прямого TCP ключ ОБЯЗАТЕЛЕН — без него подключение не запустится."
                             : "Пусто — трафик идёт через документ без шифрования.")
                            .font(.caption2)
                            .foregroundColor(transport == .direct ? .red : .secondary)
                    } else if encryptionKey.trimmingCharacters(in: .whitespaces).count < 16 {
                        Text("Слишком короткий: нужно минимум 16 символов.")
                            .font(.caption2).foregroundColor(.red)
                    } else {
                        Label("Ключ сохранится в Keychain, не в настройках VPN.",
                              systemImage: "lock.fill")
                            .font(.caption2).foregroundColor(.secondary)
                    }
                    Text("На узле — тот же секрет в файле: --encryption-key-file. Ссылка на документ должна совпадать с --url узла до символа: она входит в вывод ключа.")
                        .font(.caption2).foregroundColor(.secondary)
                }

                if transport == .yandex || transport == .volga {
                    Section("Прямой канал для капчи") {
                        TextField("адрес узла, например 1.2.3.4:9443", text: $nodeAddr)
                            .autocapitalization(.none).disableAutocorrection(true)
                        SecureField("ключ прямого канала", text: $directKey)
                            .autocapitalization(.none).disableAutocorrection(true)
                        Text("Капчу нельзя пройти, сидя на документах — это и есть заблокированный носитель. Приложение сходит через прямой канал до узла, чтобы выход шёл с ЕГО адреса, и вернётся обратно. Ключ отдельный от поля выше: там ключ самих документов.")
                            .font(.caption2).foregroundColor(.secondary)
                    }
                }

                Section {
                    Button {
                        importFromClipboard()
                    } label: {
                        Label("Вставить из буфера", systemImage: "doc.on.clipboard")
                    }
                    Button {
                        showScanner = true
                    } label: {
                        Label("Сканировать QR-код", systemImage: "qrcode.viewfinder")
                    }
                    Text("Подойдёт обычная ссылка на документ (disk.yandex.ru / boards.yandex.ru / cloud.mail.ru) или конфиг OFLUX1.")
                        .font(.caption2).foregroundColor(.secondary)
                    if let m = importMsg {
                        Text(m).font(.caption2).foregroundColor(.secondary)
                    }
                }
            }
            .sheet(isPresented: $showScanner) {
                QRScannerView { code in
                    showScanner = false
                    if ingest(code) {
                        importMsg = "QR распознан: \(transport.title)."
                    } else {
                        importMsg = "QR не содержит ссылки или конфига."
                    }
                }
            }
            .navigationTitle(profile == nil ? "Новый профиль" : "Изменить профиль")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Отмена") { dismiss() }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Сохранить") { save() }.disabled(!canSave)
                }
            }
            .onAppear(perform: load)
        }
    }

    private var canSave: Bool {
        guard !name.trimmingCharacters(in: .whitespaces).isEmpty else { return false }
        switch transport {
        case .yandex: return !url1.trimmingCharacters(in: .whitespaces).isEmpty
        case .volga, .boards, .mail, .direct: return !single.trimmingCharacters(in: .whitespaces).isEmpty
        case .max: return !maxToken.isEmpty && !maxUid.isEmpty
        }
    }

    private func load() {
        guard let p = profile else { return }
        id = p.id; name = p.name; transportRaw = p.transport
        maxToken = p.maxToken; maxUid = p.maxUid
        encryptionKey = Secrets.encryptionKey(for: p.id) ?? ""
        nodeAddr = p.nodeAddr ?? ""
        directKey = Secrets.directKey(for: p.id) ?? ""
        let parts = p.url.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
        switch p.transportKind {
        case .yandex:
            url1 = parts.first ?? ""
            if parts.count > 1 { url2 = parts[1] }
        case .volga, .boards, .mail, .direct:
            single = parts.first ?? p.url
        case .max: break
        }
    }

    private func importFromClipboard() {
        if ingest(UIPasteboard.general.string ?? "") {
            importMsg = "Вставлено: \(transport.title)."
        } else {
            importMsg = "В буфере нет ссылки или OFLUX1-конфига."
        }
    }

    /// Accept EITHER an OFLUX1 config OR a plain document link. A plain link goes
    /// into the field of the currently selected transport (Mail.ru is detected by
    /// host); the name is auto-filled if empty. Returns false if it is neither.
    @discardableResult
    private func ingest(_ raw: String) -> Bool {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if applyParsed(parseOFLUX(s)) { return true }
        guard s.lowercased().hasPrefix("http") else { return false }
        if s.lowercased().contains("cloud.mail.ru") {
            transportRaw = TransportKind.mail.rawValue
        } else if s.lowercased().contains("boards.yandex.ru") {
            transportRaw = TransportKind.boards.rawValue
        }
        switch transport {
        case .yandex: url1 = s
        case .volga, .boards, .mail, .direct: single = s
        case .max: return false
        }
        if name.trimmingCharacters(in: .whitespaces).isEmpty { name = transport.title }
        return true
    }

    /// Fill the editor fields from a parsed OFLUX config. Returns false if nil.
    @discardableResult
    private func applyParsed(_ parsed: (kind: String, urls: [String])?) -> Bool {
        guard let parsed = parsed else { return false }
        transportRaw = parsed.kind
        switch TransportKind(rawValue: parsed.kind) ?? .yandex {
        case .yandex:
            url1 = parsed.urls.first ?? ""
            url2 = parsed.urls.count > 1 ? parsed.urls[1] : ""
        case .volga, .boards, .mail, .direct:
            single = parsed.urls.first ?? ""
        case .max: break
        }
        if name.trimmingCharacters(in: .whitespaces).isEmpty {
            name = transport.title
        }
        return true
    }

    private func save() {
        var url = ""
        switch transport {
        case .yandex:
            let a = url1.trimmingCharacters(in: .whitespaces)
            let b = url2.trimmingCharacters(in: .whitespaces)
            url = b.isEmpty ? a : "\(a),\(b)"
        case .volga, .boards, .mail, .direct:
            url = single.trimmingCharacters(in: .whitespaces)
        case .max:
            url = ""
        }
        // The secret goes to the shared Keychain, never into the profile record
        // (which lives in plain UserDefaults). An emptied field removes it.
        Secrets.setEncryptionKey(encryptionKey, for: id)
        Secrets.setDirectKey(directKey, for: id)
        onSave(Profile(id: id, name: name.trimmingCharacters(in: .whitespaces),
                       transport: transportRaw, url: url,
                       maxToken: maxToken, maxUid: maxUid,
                       nodeAddr: nodeAddr.trimmingCharacters(in: .whitespaces)))
        dismiss()
    }
}

// MARK: - Settings sheet

struct SettingsSheet: View {
    @Binding var socksPort: String
    @Binding var debugLog: Bool
    @Binding var splitRU: Bool
    @Binding var dnsPreset: String
    @Binding var dnsCustom: String
    @Binding var tunnelUDP: Bool
    @ObservedObject var tunnel: TunnelController
    @ObservedObject var vpn: VPNController
    @ObservedObject var directDomains: DirectDomainStore
    let selectedProfile: Profile?
    let port: Int
    @Environment(\.dismiss) private var dismiss

    @State private var newDomain = ""
    @State private var domainError: String?

    private var dnsPresetLabel: String {
        switch dnsPreset {
        case "cloudflare": return "Cloudflare"
        case "google":     return "Google"
        case "quad9":      return "Quad9"
        case "adguard":    return "AdGuard"
        case "custom":     return "Custom…"
        default:           return "Default (Yandex/Google/CF)"
        }
    }

    private func addDomain() {
        let raw = newDomain
        guard !raw.trimmingCharacters(in: .whitespaces).isEmpty else { return }
        if directDomains.add(raw) {
            newDomain = ""
            domainError = nil
        } else if DirectDomainStore.normalize(raw) == nil {
            domainError = "Не похоже на домен"
        } else {
            domainError = "Уже в списке"
            newDomain = ""
        }
    }

    private func applyDNS() {
        dotSpec(dnsPreset, dnsCustom).withCString {
            OpenFluxSetDoTResolver(UnsafeMutablePointer(mutating: $0))
        }
    }

    var body: some View {
        NavigationView {
            Form {
                Section("Маршрутизация") {
                    Toggle("Split tunneling — RU напрямую", isOn: $splitRU)
                        .disabled(vpn.active)
                    Text("RU-адреса (GeoIP) и RU-домены (GeoSite, включая сервисы на зарубежных CDN) идут мимо узла — быстрее и меньше нагрузки на канал. Заблокированное зарубежное — через узел.")
                        .font(.caption2).foregroundColor(.secondary)
                    Toggle("Туннелировать UDP / QUIC", isOn: $tunnelUDP)
                        .disabled(vpn.active)
                    Text("Выкл = QUIC падает на TCP (работает на любом узле). Вкл = требуется UDP-совместимый узел.")
                        .font(.caption2).foregroundColor(.secondary)
                }

                Section("Свои домены напрямую") {
                    HStack {
                        TextField("example.ru", text: $newDomain)
                            .autocapitalization(.none)
                            .disableAutocorrection(true)
                            .keyboardType(.URL)
                            .onSubmit(addDomain)
                        Button(action: addDomain) {
                            Image(systemName: "plus.circle.fill")
                        }
                        .disabled(newDomain.trimmingCharacters(in: .whitespaces).isEmpty)
                    }
                    if let e = domainError {
                        Text(e).font(.caption2).foregroundColor(.red)
                    }

                    if directDomains.domains.isEmpty {
                        Text("Список пуст — идут только встроенные RU-домены.")
                            .font(.caption2).foregroundColor(.secondary)
                    } else {
                        ForEach(directDomains.domains, id: \.self) { d in
                            HStack {
                                Image(systemName: "arrow.uturn.right")
                                    .font(.caption).foregroundColor(.secondary)
                                Text(d).font(.system(.callout, design: .monospaced))
                                Spacer()
                                Button(role: .destructive) {
                                    directDomains.remove(d)
                                } label: {
                                    Image(systemName: "minus.circle")
                                }
                                .buttonStyle(.plain)
                                .foregroundColor(.red)
                            }
                        }
                        .onDelete { directDomains.remove(at: $0) }
                    }

                    Text("Эти домены и их поддомены пойдут мимо туннеля, напрямую. Работают и при выключенном RU-сплите. Применяются при следующем подключении.")
                        .font(.caption2).foregroundColor(.secondary)
                }

                Section("DNS (DNS-over-TLS)") {
                    Picker("DNS", selection: $dnsPreset) {
                        Text("Default (Yandex/Google/CF)").tag("default")
                        Text("Cloudflare").tag("cloudflare")
                        Text("Google").tag("google")
                        Text("Quad9").tag("quad9")
                        Text("AdGuard").tag("adguard")
                        Text("Custom…").tag("custom")
                    }
                    .onChange(of: dnsPreset) { _ in applyDNS() }
                    if dnsPreset == "custom" {
                        TextField("1.1.1.1@cloudflare-dns.com", text: $dnsCustom)
                            .autocapitalization(.none).disableAutocorrection(true)
                            .keyboardType(.URL)
                            .onChange(of: dnsCustom) { _ in applyDNS() }
                    }
                }

                Section("Локальный прокси (SOCKS5)") {
                    HStack {
                        Text("Порт")
                        Spacer()
                        TextField("10808", text: $socksPort)
                            .keyboardType(.numberPad).multilineTextAlignment(.trailing)
                            .frame(width: 90)
                            .disabled(tunnel.running)
                    }
                    if tunnel.running {
                        Text("SOCKS5: \(tunnel.socksAddr)").font(.footnote).foregroundColor(.secondary)
                        Button(role: .destructive) { tunnel.stop() } label: {
                            Label("Остановить локальный прокси", systemImage: "stop.fill")
                        }
                    } else if let p = selectedProfile, p.isValid {
                        Button {
                            tunnel.start(transport: p.transportKind, url: p.url,
                                         maxToken: p.maxToken, maxUid: p.maxUid, port: port,
                                         encryptionKey: Secrets.encryptionKey(for: p.id) ?? "")
                        } label: {
                            Label("Запустить локальный прокси", systemImage: "play.fill")
                        }
                        .disabled(vpn.active)
                    }
                }

                Section("Журнал") {
                    Toggle("Подробный лог", isOn: $debugLog)
                        .onChange(of: debugLog) { on in OpenFluxSetDebug(on ? 1 : 0) }
                    HStack {
                        Button {
                            UIPasteboard.general.string = tunnel.log
                        } label: { Label("Скопировать", systemImage: "doc.on.doc") }
                        .disabled(tunnel.log.isEmpty)
                        Spacer()
                        Button(role: .destructive) {
                            tunnel.clearLog()
                        } label: { Label("Очистить", systemImage: "trash") }
                        .disabled(tunnel.log.isEmpty)
                    }
                    .buttonStyle(.borderless)
                    ScrollViewReader { proxy in
                        ScrollView {
                            Text(tunnel.log.isEmpty ? "—" : tunnel.log)
                                .font(.system(.caption2, design: .monospaced))
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .textSelection(.enabled)
                                .id("logtail")
                        }
                        .onChange(of: tunnel.log) { _ in
                            withAnimation { proxy.scrollTo("logtail", anchor: .bottom) }
                        }
                    }
                    .frame(height: 200)
                }
            }
            .navigationTitle("Настройки")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Готово") { dismiss() }
                }
            }
        }
    }
}

#Preview {
    ContentView()
}
