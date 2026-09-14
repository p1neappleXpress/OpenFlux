import SwiftUI
import UIKit

struct ContentView: View {
    @StateObject private var tunnel = TunnelController()
    @StateObject private var vpn = VPNController()

    @AppStorage("transportKind") private var transportRaw: String = TransportKind.yandex.rawValue
    @AppStorage("docURL") private var docURL: String = ""
    @AppStorage("maxToken") private var maxToken: String = ""
    @AppStorage("maxUid") private var maxUid: String = ""
    // Uncommon default port to avoid clashing with other local proxies.
    @AppStorage("socksPort") private var socksPort: String = "10808"
    @AppStorage("debugLog") private var debugLog: Bool = false
    @AppStorage("dnsPreset") private var dnsPreset: String = "default"
    @AppStorage("dnsCustom") private var dnsCustom: String = ""
    @AppStorage("tunnelUDP") private var tunnelUDP: Bool = false
    @State private var showInfo = false
    @State private var showImportResult = false
    @State private var importOK = false

    private var transport: TransportKind {
        TransportKind(rawValue: transportRaw) ?? .yandex
    }

    /// DoT resolver spec passed to the Go core ("" = built-in defaults).
    private var dnsSpec: String {
        switch dnsPreset {
        case "cloudflare": return "1.1.1.1@cloudflare-dns.com"
        case "google":     return "8.8.8.8@dns.google"
        case "quad9":      return "9.9.9.9@dns.quad9.net"
        case "adguard":    return "94.140.14.14@dns.adguard-dns.com"
        case "custom":     return dnsCustom.trimmingCharacters(in: .whitespaces)
        default:           return ""
        }
    }

    /// Push the current DoT resolver into the in-app Go core (the SOCKS/test
    /// path). The VPN extension gets it separately via providerConfiguration.
    private func applyDNS() {
        dnsSpec.withCString { OpenFluxSetDoTResolver(UnsafeMutablePointer(mutating: $0)) }
    }

    /// Import an "OFLUX1:" config string (base64url of {t,u}) — sets the
    /// transport and document URL in one paste. Returns false if it can't parse.
    @discardableResult
    private func importConfig(_ raw: String) -> Bool {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard s.hasPrefix("OFLUX1:") else { return false }
        var b64 = String(s.dropFirst("OFLUX1:".count))
            .replacingOccurrences(of: "-", with: "+")
            .replacingOccurrences(of: "_", with: "/")
        while b64.count % 4 != 0 { b64 += "=" }
        guard let data = Data(base64Encoded: b64),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let u = obj["u"] as? String, !u.isEmpty else { return false }
        let t = (obj["t"] as? String) ?? "volga"
        docURL = u
        transportRaw = (t == "vyandex" ? "volga" : t)
        return true
    }

    private var canStart: Bool {
        guard (Int(socksPort) ?? 0) > 0 else { return false }
        switch transport {
        case .yandex, .volga: return !docURL.trimmingCharacters(in: .whitespaces).isEmpty
        case .max:            return !maxToken.isEmpty && !maxUid.isEmpty
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
                    .disabled(tunnel.running)

                    Button {
                        importOK = importConfig(UIPasteboard.general.string ?? "")
                        showImportResult = true
                    } label: {
                        Label("Import config from clipboard", systemImage: "square.and.arrow.down")
                            .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.bordered)
                    .disabled(tunnel.running)

                    connectionFields

                    portField

                    dnsSection

                    controls

                    vpnSection

                    logView
                }
                .padding()
            }
            .navigationTitle("OpenFlux")
            .onAppear {
                OpenFluxSetDebug(debugLog ? 1 : 0)
                applyDNS()
            }
            .onChange(of: dnsPreset) { _ in applyDNS() }
            .onChange(of: dnsCustom) { _ in applyDNS() }
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button { showInfo = true } label: {
                        Image(systemName: "info.circle")
                    }
                }
            }
            .sheet(isPresented: $showInfo) { InfoView() }
            .alert(importOK ? "Config imported" : "No valid config",
                   isPresented: $showImportResult) {
                Button("OK", role: .cancel) {}
            } message: {
                Text(importOK
                     ? "Transport and document URL were filled in. Tap Start VPN."
                     : "Copy an OFLUX1:… config string, then tap Import again.")
            }
        }
        .navigationViewStyle(.stack)
    }

    @ViewBuilder
    private var connectionFields: some View {
        switch transport {
        case .yandex, .volga:
            field(title: "Yandex Docs URL",
                  placeholder: "https://docs.yandex.ru/docs/view?url=...",
                  text: $docURL)
        case .max:
            field(title: "MAX token", placeholder: "auth token", text: $maxToken)
            field(title: "MAX user ID", placeholder: "numeric id", text: $maxUid,
                  keyboard: .numberPad)
        }
    }

    private var portField: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Local SOCKS5 port").font(.caption).foregroundColor(.secondary)
            TextField("10808", text: $socksPort)
                .keyboardType(.numberPad)
                .textFieldStyle(.roundedBorder)
                .disabled(tunnel.running)
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
                        tunnel.start(transport: transport,
                                     url: docURL,
                                     maxToken: maxToken,
                                     maxUid: maxUid,
                                     port: Int(socksPort) ?? 10808)
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

    private var dnsSection: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("DNS (DNS-over-TLS)").font(.caption).foregroundColor(.secondary)
            Picker("DNS", selection: $dnsPreset) {
                Text("Default (Yandex/Google/CF)").tag("default")
                Text("Cloudflare").tag("cloudflare")
                Text("Google").tag("google")
                Text("Quad9").tag("quad9")
                Text("AdGuard").tag("adguard")
                Text("Custom…").tag("custom")
            }
            .pickerStyle(.menu)
            .disabled(tunnel.running)
            if dnsPreset == "custom" {
                TextField("1.1.1.1@cloudflare-dns.com", text: $dnsCustom)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled(true)
                    .keyboardType(.URL)
                    .textFieldStyle(.roundedBorder)
                    .disabled(tunnel.running)
                Text("Format: address[:port]@tls-hostname")
                    .font(.caption2).foregroundColor(.secondary)
            }
        }
    }

    private var vpnSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Divider()
            HStack {
                Text("System VPN (all traffic)").font(.subheadline).bold()
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
                    vpn.start(transport: transport.rawValue, url: docURL,
                              maxToken: maxToken, maxUid: maxUid, dns: dnsSpec,
                              tunnelUDP: tunnelUDP)
                } label: {
                    Label("Start VPN", systemImage: "bolt.fill").frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .disabled(!canStart)
            }
            Text("Routes the whole device through the exit node (TCP + DNS-over-TCP).")
                .font(.caption2).foregroundColor(.secondary)
            Toggle(isOn: $tunnelUDP) {
                Text("Tunnel UDP / QUIC").font(.caption)
            }
            .disabled(vpn.active)
            Text("Off = QUIC falls back to TCP (works on any node). On = tunnel UDP — needs a UDP-capable exit node.")
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
                .disabled(tunnel.running)
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
