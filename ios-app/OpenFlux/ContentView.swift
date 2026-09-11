import SwiftUI

struct ContentView: View {
    @StateObject private var tunnel = TunnelController()
    @AppStorage("docURL") private var docURL: String = ""

    var body: some View {
        NavigationView {
            VStack(spacing: 16) {
                statusHeader

                VStack(alignment: .leading, spacing: 6) {
                    Text("Yandex.Docs URL").font(.caption).foregroundColor(.secondary)
                    TextField("https://docs.yandex.ru/docs/view?url=...", text: $docURL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled(true)
                        .textFieldStyle(.roundedBorder)
                        .disabled(tunnel.running)
                }

                HStack(spacing: 12) {
                    if tunnel.running {
                        Button(role: .destructive) { tunnel.stop() } label: {
                            Label("Stop", systemImage: "stop.fill").frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.borderedProminent)
                    } else {
                        Button { tunnel.start(url: docURL) } label: {
                            Label("Start", systemImage: "play.fill").frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.borderedProminent)
                        .disabled(docURL.isEmpty)
                    }
                    Button { tunnel.testThroughProxy() } label: {
                        Label("Test", systemImage: "network").frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.bordered)
                    .disabled(!tunnel.running)
                }

                Text("SOCKS5 proxy: \(tunnel.socksAddr)")
                    .font(.footnote).foregroundColor(.secondary)

                logView

                Spacer(minLength: 0)
            }
            .padding()
            .navigationTitle("OpenFlux")
        }
        .navigationViewStyle(.stack)
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
            .frame(maxHeight: 260)
            .background(Color(.secondarySystemBackground))
            .clipShape(RoundedRectangle(cornerRadius: 8))
        }
    }
}

#Preview {
    ContentView()
}
