import SwiftUI

struct ContentView: View {
    @StateObject private var tunnel = TunnelController()
    @StateObject private var vpn = VPNController()

    @AppStorage("transportKind") private var transportRaw: String = TransportKind.yandex.rawValue
    @AppStorage("docURL") private var docURL: String = ""
    @State private var maxToken: String = ""
    @State private var encryptionSecret: String = ""
    @State private var credentialsError: String?
    @AppStorage("codec") private var codec: String = "batched"
    @AppStorage("maxUid") private var maxUid: String = ""
    // Uncommon default port to avoid clashing with other local proxies.
    @AppStorage("socksPort") private var socksPort: String = "10808"
    @AppStorage("debugLog") private var debugLog: Bool = false
    @State private var showInfo = false

    private var transport: TransportKind {
        TransportKind(rawValue: transportRaw) ?? .yandex
    }

    // MAX has no document setting. Do not accidentally salt its encryption key
    // with a hidden Yandex URL left over from a previous transport selection.
    private var transportURL: String { transport == .max ? "" : docURL }

    private var canStart: Bool {
        guard (1...65535).contains(Int(socksPort) ?? 0), credentialsError == nil else { return false }
        let secret = encryptionSecret.trimmingCharacters(in: .whitespacesAndNewlines)
        guard secret.isEmpty || secret.unicodeScalars.count >= 16 else { return false }
        switch transport {
        case .yandex, .vyandex: return !docURL.trimmingCharacters(in: .whitespaces).isEmpty
        case .max:    return !maxToken.isEmpty && !maxUid.isEmpty
        }
    }

    var body: some View {
        NavigationView {
            ScrollView {
                VStack(spacing: 16) {
                    statusHeader

                    Picker("Transport", selection: $transportRaw) {
                        ForEach(TransportKind.allCases) { t in
                            Text(t.title).tag(t.rawValue)
                        }
                    }
                    .pickerStyle(.segmented)
                    .disabled(tunnel.running || vpn.active)

                    connectionFields

                    Picker("Codec", selection: $codec) {
                        Text("Batched").tag("batched")
                        Text("Legacy").tag("legacy")
                    }.pickerStyle(.segmented).disabled(tunnel.running || vpn.active)

                    encryptionFields

                    portField

                    controls

                    vpnSection

                    logView
                }
                .padding()
            }
            .navigationTitle("OpenFlux")
            .onAppear { OpenFluxSetDebug(debugLog ? 1 : 0); loadCredentials() }
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button { showInfo = true } label: {
                        Image(systemName: "info.circle")
                    }
                }
            }
            .sheet(isPresented: $showInfo) { InfoView() }
        }
        .navigationViewStyle(.stack)
    }

    @ViewBuilder
    private var connectionFields: some View {
        switch transport {
        case .yandex, .vyandex:
            field(title: "Yandex Docs URL",
                  placeholder: "https://docs.yandex.ru/docs/view?url=...",
                  text: $docURL)
        case .max:
            SecureField("MAX token", text: $maxToken)
                .textInputAutocapitalization(.never).autocorrectionDisabled(true)
                .textFieldStyle(.roundedBorder).disabled(tunnel.running || vpn.active)
            field(title: "MAX user ID", placeholder: "numeric id", text: $maxUid,
                  keyboard: .numberPad)
        }
    }

    private var encryptionFields: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Encryption").font(.caption).foregroundColor(.secondary)
            SecureField("Encryption key", text: $encryptionSecret)
                .textInputAutocapitalization(.never).autocorrectionDisabled(true)
                .textFieldStyle(.roundedBorder)
            HStack {
                Button("Paste") { encryptionSecret = UIPasteboard.general.string ?? "" }
                Button("Clear") { encryptionSecret = ""; _ = saveCredentials() }
                Spacer()
                if !encryptionSecret.isEmpty { Label("Encryption enabled", systemImage: "lock.fill").font(.caption) }
            }
            Text("Оставьте пустым, чтобы отключить сквозное шифрование.")
                .font(.caption2).foregroundColor(.secondary)
            if let error = credentialsError { Text(error).font(.caption).foregroundColor(.red) }
        }.disabled(tunnel.running || vpn.active)
    }

    private func loadCredentials() {
        do {
            let store = KeychainSecretStore()
            if let data = try store.read(account: AppCredentials.account) {
                let value = try JSONDecoder().decode(AppCredentials.self, from: data)
                encryptionSecret = value.encryptionSecret
                maxToken = value.maxToken
            } else if let oldToken = UserDefaults.standard.string(forKey: "maxToken") {
                // Migrate the pre-Keychain app setting once, removing it only
                // after successful Keychain storage.
                maxToken = oldToken
                try store.write(JSONEncoder().encode(AppCredentials(maxToken: oldToken)), account: AppCredentials.account)
            }
            UserDefaults.standard.removeObject(forKey: "maxToken")
            credentialsError = nil
        } catch { credentialsError = "Cannot read shared Keychain. Check signing and device unlock." }
    }

    private func saveCredentials() -> Bool {
        do {
            encryptionSecret = encryptionSecret.trimmingCharacters(in: .whitespacesAndNewlines)
            guard encryptionSecret.isEmpty || encryptionSecret.unicodeScalars.count >= 16 else { throw SecretStoreError.invalidSecret }
            let value = AppCredentials(encryptionSecret: encryptionSecret, maxToken: maxToken)
            try KeychainSecretStore().write(JSONEncoder().encode(value), account: AppCredentials.account)
            credentialsError = nil
            return true
        } catch { credentialsError = "Cannot save credentials. Check key length, signing and device unlock."; return false }
    }

    private var portField: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Local SOCKS5 port").font(.caption).foregroundColor(.secondary)
            TextField("10808", text: $socksPort)
                .keyboardType(.numberPad)
                .textFieldStyle(.roundedBorder)
                .disabled(tunnel.running || vpn.active)
        }
    }

    private var controls: some View {
        VStack(spacing: 12) {
            HStack(spacing: 12) {
                if tunnel.running {
                    Button(role: .destructive) { tunnel.stop() } label: {
                        Label("Stop", systemImage: "stop.fill").frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                } else {
                    Button {
                        guard saveCredentials() else { return }
                        tunnel.start(transport: transport,
                                     url: transportURL,
                                     maxToken: maxToken,
                                     maxUid: maxUid,
                                     port: Int(socksPort) ?? 10808, codec: codec, encryptionSecret: encryptionSecret)
                    } label: {
                        Label("Start", systemImage: "play.fill").frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(!canStart)
                }
                Button { tunnel.testThroughProxy() } label: {
                    Label("Test", systemImage: "network").frame(maxWidth: .infinity)
                }
                .buttonStyle(.bordered)
                .disabled(!tunnel.running)
            }
            if tunnel.running {
                Text("SOCKS5 proxy: \(tunnel.socksAddr)")
                    .font(.footnote).foregroundColor(.secondary)
            }
        }
    }

    private var vpnSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Divider()
            HStack {
                Text("System VPN").font(.subheadline).bold()
                Spacer()
                Text(vpn.status).font(.caption).foregroundColor(.secondary)
            }
            if vpn.active {
                Button(role: .destructive) { vpn.stop() } label: {
                    Label("Stop VPN", systemImage: "bolt.slash.fill").frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
            } else {
                Button {
                    guard saveCredentials() else { return }
                    vpn.start(transport: transport.rawValue, url: transportURL,
                              maxToken: maxToken, maxUid: maxUid, codec: codec, encryptionSecret: encryptionSecret)
                } label: {
                    Label("Start VPN", systemImage: "bolt.fill").frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(!canStart)
            }
            Text("Routes IPv4 TCP through the exit node; DNS uses DNS-over-TLS.")
                .font(.caption2).foregroundColor(.secondary)
        }
    }

    private func field(title: String, placeholder: String, text: Binding<String>,
                       keyboard: UIKeyboardType = .default) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.caption).foregroundColor(.secondary)
            TextField(placeholder, text: text)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled(true)
                .keyboardType(keyboard)
                .textFieldStyle(.roundedBorder)
                .disabled(tunnel.running || vpn.active)
        }
    }

    private var statusHeader: some View {
        HStack {
            Circle()
                .fill(tunnel.connected ? Color.green : (tunnel.running ? Color.orange : Color.gray))
                .frame(width: 12, height: 12)
            Text(tunnel.connected ? "Connected" : (tunnel.running ? "Connecting…" : "Stopped"))
                .font(.headline)
            Spacer()
        }
    }

    private var logView: some View {
        VStack(alignment: .leading, spacing: 4) {
            Toggle(isOn: $debugLog) {
                Text("Verbose log").font(.caption).foregroundColor(.secondary)
            }
            .onChange(of: debugLog) { on in OpenFluxSetDebug(on ? 1 : 0) }
            Text("Log").font(.caption).foregroundColor(.secondary)
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
            .frame(height: 240)
            .background(Color(.secondarySystemBackground))
            .clipShape(RoundedRectangle(cornerRadius: 8))
        }
    }
}

#Preview {
    ContentView()
}
