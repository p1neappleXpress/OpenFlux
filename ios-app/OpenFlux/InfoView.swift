import SwiftUI
import UIKit

/// About screen with donation addresses (tap a row to copy).
struct InfoView: View {
    @Environment(\.dismiss) private var dismiss
    @State private var copied: String?

    private let sol = "7yXWW2iAkKadyVvMjZLPYo1PqizvKqseG2ZQ1sZk9X1k"
    private let eth = "0xd043E852158C13C8064a73b9cDd920DaAa80f0c1"

    var body: some View {
        NavigationView {
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("OpenFlux").font(.title2).bold()
                        Text("TCP-туннель через скрытый транспорт. Клиент поднимает локальный SOCKS5 и системный VPN, трафик идёт через exit-node.")
                            .font(.footnote).foregroundColor(.secondary)
                    }

                    VStack(alignment: .leading, spacing: 12) {
                        Text("Поддержать разработку ♥")
                            .font(.headline)
                        Text("Нажми на адрес, чтобы скопировать.")
                            .font(.caption).foregroundColor(.secondary)

                        donationRow(title: "Solana (SOL)", address: sol)
                        donationRow(title: "Ethereum (ETH)", address: eth)

                        if let c = copied {
                            Label("\(c) скопирован", systemImage: "checkmark.circle.fill")
                                .font(.caption).foregroundColor(.green)
                        }
                    }
                    .padding()
                    .background(Color(.secondarySystemBackground))
                    .clipShape(RoundedRectangle(cornerRadius: 12))

                    Spacer(minLength: 0)
                }
                .padding()
            }
            .navigationTitle("О приложении")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button("Закрыть") { dismiss() }
                }
            }
        }
        .navigationViewStyle(.stack)
    }

    private func donationRow(title: String, address: String) -> some View {
        Button {
            UIPasteboard.general.string = address
            copied = title
        } label: {
            VStack(alignment: .leading, spacing: 4) {
                HStack {
                    Text(title).font(.subheadline).bold()
                    Spacer()
                    Image(systemName: "doc.on.doc").font(.caption)
                }
                Text(address)
                    .font(.system(.caption2, design: .monospaced))
                    .foregroundColor(.secondary)
                    .multilineTextAlignment(.leading)
                    .lineLimit(2)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(10)
            .background(Color(.tertiarySystemBackground))
            .clipShape(RoundedRectangle(cornerRadius: 8))
        }
        .buttonStyle(.plain)
    }
}
