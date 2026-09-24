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
    @State private var editing: Profile?      // profile being edited (or a fresh one)
    @State private var showEditor = false
    @State private var sharing: Profile?      // profile shown as a QR
    @State private var testHint: String?

    private var dnsSpec: String { dotSpec(dnsPreset, dnsCustom) }

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
                      split: splitRU ? "ru-direct" : "")
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
                         port: Int(socksPort) ?? 10808)
            testHint = "Локальный прокси запущен — нажмите ещё раз для проверки."
        }
    }

    // MARK: body

    var body: some View {
        NavigationView {
            VStack(spacing: 28) {
                Text(store.profiles.isEmpty
                     ? "Добавьте профиль: вставьте ссылку на документ или отсканируйте QR"
                     : "Выберите профиль и нажмите кнопку подключения")
                    .font(.footnote).foregroundColor(.secondary)
                    .multilineTextAlignment(.center)
                    .frame(maxWidth: .infinity)

                connectButton

                Text(statusText).font(.headline)

                profilePicker

                Button {
                    checkAvailability()
                } label: {
                    Label("Проверить доступность", systemImage: "waveform.path.ecg")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                .disabled(store.selected == nil || !(store.selected?.isValid ?? false))

                if let hint = testHint {
                    Text(hint).font(.caption2).foregroundColor(.secondary)
                        .multilineTextAlignment(.center)
                }

                Spacer()
            }
            .padding()
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
            .onAppear {
                OpenFluxSetDebug(debugLog ? 1 : 0)
                dnsSpec.withCString { OpenFluxSetDoTResolver(UnsafeMutablePointer(mutating: $0)) }
                migrateLegacyIfNeeded()
            }
            .sheet(isPresented: $showInfo) { InfoView() }
            .sheet(isPresented: $showSettings) {
                SettingsSheet(socksPort: $socksPort, debugLog: $debugLog,
                              splitRU: $splitRU, dnsPreset: $dnsPreset,
                              dnsCustom: $dnsCustom, tunnelUDP: $tunnelUDP,
                              tunnel: tunnel, vpn: vpn,
                              selectedProfile: store.selected,
                              port: Int(socksPort) ?? 10808)
            }
            .sheet(isPresented: $showEditor) {
                ProfileEditorView(profile: editing) { saved in
                    store.upsert(saved)
                }
            }
            .sheet(item: $sharing) { p in
                ShareQRView(profile: p)
            }
        }
        .navigationViewStyle(.stack)
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
                editing = nil; showEditor = true
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
                } label: {
                    Label(p.name, systemImage: p.id == store.selectedID ? "checkmark" : "")
                }
            }
            if !store.profiles.isEmpty { Divider() }
            if let sel = store.selected {
                Button {
                    editing = sel; showEditor = true
                } label: { Label("Изменить «\(sel.name)»", systemImage: "pencil") }
                Button {
                    sharing = sel
                } label: { Label("Поделиться (QR)", systemImage: "qrcode") }
                Button(role: .destructive) {
                    store.delete(sel)
                } label: { Label("Удалить «\(sel.name)»", systemImage: "trash") }
            }
            Button {
                editing = nil; showEditor = true
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

    private func transportIcon(_ t: String?) -> String {
        switch t {
        case "mailru": return "envelope"
        case "volga":  return "waveform"
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
                }

                Section("Подключение") {
                    switch transport {
                    case .yandex:
                        TextField("https://disk.yandex.ru/i/…", text: $url1).autocapitalization(.none).disableAutocorrection(true)
                        TextField("второй документ (необязательно)", text: $url2).autocapitalization(.none).disableAutocorrection(true)
                    case .volga:
                        TextField("https://disk.yandex.ru/i/…", text: $single).autocapitalization(.none).disableAutocorrection(true)
                    case .mail:
                        TextField("https://cloud.mail.ru/public/…", text: $single).autocapitalization(.none).disableAutocorrection(true)
                    case .max:
                        TextField("MAX token", text: $maxToken).autocapitalization(.none).disableAutocorrection(true)
                        TextField("MAX user ID", text: $maxUid).keyboardType(.numberPad)
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
                    Text("Подойдёт обычная ссылка на документ (disk.yandex.ru / cloud.mail.ru) или конфиг OFLUX1.")
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
        case .volga, .mail: return !single.trimmingCharacters(in: .whitespaces).isEmpty
        case .max: return !maxToken.isEmpty && !maxUid.isEmpty
        }
    }

    private func load() {
        guard let p = profile else { return }
        id = p.id; name = p.name; transportRaw = p.transport
        maxToken = p.maxToken; maxUid = p.maxUid
        let parts = p.url.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
        switch p.transportKind {
        case .yandex:
            url1 = parts.first ?? ""
            if parts.count > 1 { url2 = parts[1] }
        case .volga, .mail:
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
        }
        switch transport {
        case .yandex: url1 = s
        case .volga, .mail: single = s
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
        case .volga, .mail:
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
        case .volga, .mail:
            url = single.trimmingCharacters(in: .whitespaces)
        case .max:
            url = ""
        }
        onSave(Profile(id: id, name: name.trimmingCharacters(in: .whitespaces),
                       transport: transportRaw, url: url,
                       maxToken: maxToken, maxUid: maxUid))
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
    let selectedProfile: Profile?
    let port: Int
    @Environment(\.dismiss) private var dismiss

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
                                         maxToken: p.maxToken, maxUid: p.maxUid, port: port)
                        } label: {
                            Label("Запустить локальный прокси", systemImage: "play.fill")
                        }
                        .disabled(vpn.active)
                    }
                }

                Section("Журнал") {
                    Toggle("Подробный лог", isOn: $debugLog)
                        .onChange(of: debugLog) { on in OpenFluxSetDebug(on ? 1 : 0) }
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
