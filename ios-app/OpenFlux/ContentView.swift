import SwiftUI

struct ContentView: View {
    @StateObject private var tunnel = TunnelController()
    @StateObject private var vpn = VPNController()
    @StateObject private var check = ConnectionCheck()
    @AppStorage("useSystemVPN") private var useSystemVPN = true

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
    @AppStorage("udpForwarding") private var udpForwarding: Bool = false
    @State private var showInfo = false

    private var transport: TransportKind {
        TransportKind(rawValue: transportRaw) ?? .yandex
    }

    // MAX has no document setting. Do not accidentally salt its encryption key
    // with a hidden Yandex URL left over from a previous transport selection.
    private var transportURL: String { transport == .max ? "" : docURL }

    private var active: Bool { vpn.active || tunnel.running }
    private var systemMode: Bool { vpn.active || (!tunnel.running && useSystemVPN) }
    private var connected: Bool { systemMode ? vpn.connected : tunnel.connected }
    private var status: String {
        systemMode ? vpn.status : (tunnel.startError ?? (tunnel.connected ? "Connected" : (tunnel.running ? "Connecting…" : "Disconnected")))
    }
    private var validationMessage: String? {
        if !useSystemVPN && !(1...65535).contains(Int(socksPort) ?? 0) { return "Enter a SOCKS port from 1 to 65535." }
        let secret = encryptionSecret.trimmingCharacters(in: .whitespacesAndNewlines)
        if !secret.isEmpty && secret.unicodeScalars.count < 16 { return "Use an encryption key of at least 16 characters." }
        switch transport {
        case .yandex, .vyandex:
            guard let url = URL(string: docURL), ["https", "http"].contains(url.scheme?.lowercased() ?? ""),
                  let host = url.host, !host.isEmpty else { return "Enter the full document link to connect." }
        case .max:
            if maxToken.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return "Enter your MAX token." }
            if (Int64(maxUid) ?? 0) <= 0 { return "Enter a valid numeric MAX user ID." }
        }
        return nil
    }

    var body: some View {
        NavigationView {
            Form {
                Section {
                    statusHeader
                    controls
                    if !active, let message = validationMessage {
                        Text(message).font(.footnote).foregroundColor(.secondary)
                    }
                    if let error = credentialsError {
                        Text(error).font(.footnote).foregroundColor(.red)
                        Button("Retry reading credentials") { loadCredentials() }
                            .disabled(active)
                    }
                }
                Section("Connection") {
                    Picker("Transport", selection: $transportRaw) {
                        ForEach(TransportKind.allCases) { t in
                            Text(t.title).tag(t.rawValue)
                        }
                    }
                    .disabled(active)
                    connectionFields
                    encryptionFields
                }
                Section {
                    DisclosureGroup("Advanced settings") {
                        Toggle("System VPN", isOn: $useSystemVPN).disabled(active || vpn.loading)
                        Text(useSystemVPN ? "Routes IPv4 traffic for your apps. IPv6 is not tunneled." : "Local SOCKS5 proxy only. Other apps must be configured to use it.")
                            .font(.footnote).foregroundColor(.secondary)
                        if !useSystemVPN { portField }
                        Picker("Codec", selection: $codec) {
                            Text("Batched").tag("batched")
                            Text("Legacy").tag("legacy")
                        }.disabled(active)
                        Text("Use the same codec and encryption key on the exit node.")
                            .font(.footnote).foregroundColor(.secondary)
                        if useSystemVPN { vpnSection }
                    }
                }
                Section {
                    DisclosureGroup("Diagnostics") {
                        Button(check.running ? "Checking…" : "Check internet access") {
                            check.start(socksAddress: systemMode ? nil : tunnel.socksAddr)
                        }.disabled(!connected || check.running)
                        if let result = check.result { Text(result).font(.footnote).textSelection(.enabled) }
                        Text("Contacts ipify over HTTPS to check IPv4 access and show the public IP. This is not a UDP or leak test.")
                            .font(.footnote).foregroundColor(.secondary)
                        if !systemMode { logView }
                    }
                }
            }
            .navigationTitle("OpenFlux")
            .onAppear { OpenFluxSetDebug(debugLog ? 1 : 0); loadCredentials() }
            .onChange(of: connected) { _ in check.cancel() }
            .onChange(of: systemMode) { _ in check.cancel() }
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button { showInfo = true } label: {
                        Image(systemName: "info.circle")
                    }.accessibilityLabel("About OpenFlux")
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
                  text: $docURL, keyboard: .URL)
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
                if !encryptionSecret.isEmpty { Label("Key entered", systemImage: "key.fill").font(.caption) }
            }.buttonStyle(.borderless)
            Text("Optional. Leave empty only if the exit node also has encryption disabled.")
                .font(.caption2).foregroundColor(.secondary)
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
            Button {
                check.cancel()
                if active {
                    if vpn.active { vpn.stop() }
                    if tunnel.running { tunnel.stop() }
                } else {
                    guard validationMessage == nil, saveCredentials() else { return }
                    if useSystemVPN {
                        vpn.start(transport: transport.rawValue, url: transportURL,
                                  maxToken: maxToken, maxUid: maxUid, codec: codec,
                                  encryptionSecret: encryptionSecret, udpForwarding: udpForwarding)
                    } else {
                        tunnel.start(transport: transport, url: transportURL, maxToken: maxToken,
                                     maxUid: maxUid, port: Int(socksPort) ?? 10808,
                                     codec: codec, encryptionSecret: encryptionSecret)
                    }
                }
            } label: {
                Label(active ? (connected ? "Disconnect" : "Cancel connection") : "Connect",
                      systemImage: active ? "stop.fill" : "power")
                    .frame(maxWidth: .infinity, minHeight: 44)
            }
            .buttonStyle(.borderedProminent)
            .disabled(!active && (vpn.loading || validationMessage != nil || credentialsError != nil))
            if tunnel.running {
                Text("SOCKS5 proxy: \(tunnel.socksAddr)")
                    .font(.footnote).foregroundColor(.secondary)
            }
        }
    }

    private var vpnSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Toggle("Forward UDP", isOn: $udpForwarding)
                .disabled(active)
            Text("Requires an exit node with UDP support. DNS still uses DNS-over-TLS.")
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
            Image(systemName: connected ? "checkmark.circle.fill" : "network")
                .font(.title2).foregroundColor(connected ? .green : .secondary)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 4) {
                Text(status).font(.headline)
                Text(systemMode ? "System VPN · IPv4" : "Local SOCKS5 proxy")
                    .font(.subheadline).foregroundColor(.secondary)
            }
            Spacer()
            if active && !connected { ProgressView().accessibilityLabel("Connection in progress") }
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
