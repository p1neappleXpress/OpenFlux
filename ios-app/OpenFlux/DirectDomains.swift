import Foundation

/// User-added domain suffixes that must bypass the tunnel (go direct), on top of
/// the bundled `geosite-ru.txt` set.
///
/// The extension feeds this list to the same GeoSite path the bundled file uses:
/// the Go DNS proxy tags matching answers, and their IPs are appended to
/// `excludedRoutes` as /32 routes. So a custom entry behaves exactly like a
/// built-in one — no separate engine.
///
/// Stored as one suffix per line in UserDefaults and handed to the extension
/// through `providerConfiguration`, which is why it is a plain string rather
/// than an array: the tunnel protocol dictionary only carries property-list
/// values, and the Go side already parses newline-separated lists.
@MainActor
final class DirectDomainStore: ObservableObject {
    @Published private(set) var domains: [String] = []

    private let key = "directDomains.v1"

    init() { load() }

    /// Newline-separated form, as handed to the extension and to Go.
    var joined: String { domains.joined(separator: "\n") }

    /// Normalizes user input into a bare suffix: strips scheme, path, `www.`,
    /// a leading dot and any `domain:` prefix, so pasting a full URL works.
    /// Returns nil if nothing usable is left.
    static func normalize(_ raw: String) -> String? {
        var s = raw.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        guard !s.isEmpty else { return nil }
        if s.hasPrefix("domain:") { s = String(s.dropFirst("domain:".count)) }
        if let r = s.range(of: "://") { s = String(s[r.upperBound...]) }
        if let slash = s.firstIndex(of: "/") { s = String(s[..<slash]) }
        if let at = s.lastIndex(of: "@") { s = String(s[s.index(after: at)...]) }
        if let colon = s.firstIndex(of: ":") { s = String(s[..<colon]) }
        while s.hasPrefix(".") { s = String(s.dropFirst()) }
        if s.hasPrefix("www.") { s = String(s.dropFirst(4)) }
        s = s.trimmingCharacters(in: .whitespaces)
        // Must look like a domain: at least one dot, no spaces, allowed charset.
        guard s.contains("."), !s.contains(" "),
              s.range(of: "^[a-z0-9.-]+$", options: .regularExpression) != nil
        else { return nil }
        return s
    }

    /// Adds a normalized suffix. Returns false if it is unusable or a duplicate.
    @discardableResult
    func add(_ raw: String) -> Bool {
        guard let d = Self.normalize(raw), !domains.contains(d) else { return false }
        domains.append(d)
        domains.sort()
        save()
        return true
    }

    func remove(at offsets: IndexSet) {
        // Done by hand rather than via remove(atOffsets:) so this file stays
        // Foundation-only (that helper comes in with SwiftUI).
        for i in offsets.sorted(by: >) where domains.indices.contains(i) {
            domains.remove(at: i)
        }
        save()
    }

    func remove(_ domain: String) {
        domains.removeAll { $0 == domain }
        save()
    }

    private func load() {
        domains = (UserDefaults.standard.stringArray(forKey: key) ?? []).sorted()
    }

    private func save() {
        UserDefaults.standard.set(domains, forKey: key)
    }
}
