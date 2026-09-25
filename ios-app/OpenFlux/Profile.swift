import Foundation

/// A saved connection profile: one transport + its document/creds, named so the
/// user can keep several and pick one from the dropdown. The main screen drives
/// the System VPN from the selected profile.
struct Profile: Identifiable, Codable, Equatable {
    var id: UUID = UUID()
    var name: String
    var transport: String           // TransportKind rawValue: yandex/volga/mailru/max
    var url: String = ""            // comma-joined docs for yandex; single for volga/mail
    var maxToken: String = ""
    var maxUid: String = ""

    /// Адрес узла (host:port) для прямого канала, которым проходится капча.
    /// ОПЦИОНАЛЬНОЕ намеренно: синтезированный Decodable требует все
    /// необязательные-по-смыслу ключи, а декодирование идёт через `try?` —
    /// новое обязательное поле молча стёрло бы все сохранённые профили.
    var nodeAddr: String?

    var transportKind: TransportKind { TransportKind(rawValue: transport) ?? .yandex }

    var isValid: Bool {
        if transport == TransportKind.max.rawValue {
            return !maxToken.isEmpty && !maxUid.isEmpty
        }
        return !url.trimmingCharacters(in: .whitespaces).isEmpty
    }

    /// One-line subtitle for the dropdown row.
    var subtitle: String {
        if transport == TransportKind.max.rawValue { return "MAX token" }
        return url
    }
}

/// Persists the profile list + current selection in UserDefaults.
@MainActor
final class ProfileStore: ObservableObject {
    @Published var profiles: [Profile] = []
    @Published var selectedID: UUID?

    private let key = "profiles.v1"
    private let selKey = "profiles.selected.v1"

    init() { load() }

    var selected: Profile? {
        if let id = selectedID, let p = profiles.first(where: { $0.id == id }) { return p }
        return profiles.first
    }

    func load() {
        if let data = UserDefaults.standard.data(forKey: key),
           let list = try? JSONDecoder().decode([Profile].self, from: data) {
            profiles = list
        }
        if let s = UserDefaults.standard.string(forKey: selKey), let id = UUID(uuidString: s) {
            selectedID = id
        }
        if selectedID == nil { selectedID = profiles.first?.id }
    }

    func save() {
        if let data = try? JSONEncoder().encode(profiles) {
            UserDefaults.standard.set(data, forKey: key)
        }
        UserDefaults.standard.set(selectedID?.uuidString, forKey: selKey)
    }

    func upsert(_ p: Profile) {
        if let i = profiles.firstIndex(where: { $0.id == p.id }) {
            profiles[i] = p
        } else {
            profiles.append(p)
            selectedID = p.id
        }
        save()
    }

    func delete(_ p: Profile) {
        profiles.removeAll { $0.id == p.id }
        if selectedID == p.id { selectedID = profiles.first?.id }
        save()
    }

    func select(_ id: UUID) { selectedID = id; save() }
}

/// Parse an "OFLUX1:" config string (base64url of {t,u}) into a transport kind
/// and its document URL(s). Shared by import-from-clipboard.
func parseOFLUX(_ raw: String) -> (kind: String, urls: [String])? {
    let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    guard s.hasPrefix("OFLUX1:") else { return nil }
    var b64 = String(s.dropFirst("OFLUX1:".count))
        .replacingOccurrences(of: "-", with: "+")
        .replacingOccurrences(of: "_", with: "/")
    while b64.count % 4 != 0 { b64 += "=" }
    guard let data = Data(base64Encoded: b64),
          let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let u = obj["u"] as? String, !u.isEmpty else { return nil }
    let t = (obj["t"] as? String) ?? "volga"
    let kind = (t == "vyandex" ? "volga" : t)
    let urls = u.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
        .filter { !$0.isEmpty }
    return (kind, urls.isEmpty ? [u] : urls)
}

/// Encode a profile as an "OFLUX1:" config string (base64url of {t,u}) for
/// sharing (QR / clipboard). Returns nil for transports with no shareable URL
/// (MAX carries token/uid, not a document link).
func makeOFLUXString(_ p: Profile) -> String? {
    let url = p.url.trimmingCharacters(in: .whitespaces)
    guard p.transport != TransportKind.max.rawValue, !url.isEmpty else { return nil }
    let obj: [String: String] = ["t": p.transport, "u": url]
    guard let data = try? JSONSerialization.data(withJSONObject: obj) else { return nil }
    let b64 = data.base64EncodedString()
        .replacingOccurrences(of: "+", with: "-")
        .replacingOccurrences(of: "/", with: "_")
        .replacingOccurrences(of: "=", with: "")
    return "OFLUX1:" + b64
}
