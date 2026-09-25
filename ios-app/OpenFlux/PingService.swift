import Foundation

/// Latency probe for a profile, runnable straight from the profile dropdown
/// without connecting anything.
///
/// It times a request to the profile's own document host — the service the
/// transport actually rides on (disk/docs.yandex, boards.yandex, cloud.mail.ru).
/// That is what a user means by "is this config alive": if the backend is
/// unreachable or slow from here, the tunnel cannot be better. It deliberately
/// does NOT measure the exit node, which is only reachable once connected.
///
/// ICMP is unavailable to a sandboxed app, so this is an HTTP round trip; the
/// number is therefore a touch higher than a raw ping, but comparable between
/// profiles, which is what it is for.
@MainActor
final class PingService: ObservableObject {
    enum Outcome: Equatable {
        case running
        case ms(Int)
        case failed(String)

        var short: String {
            switch self {
            case .running:      return "…"
            case .ms(let v):    return "\(v) мс"
            case .failed:       return "нет ответа"
            }
        }
    }

    @Published private(set) var results: [UUID: Outcome] = [:]

    private var inFlight = Set<UUID>()

    func outcome(for p: Profile) -> Outcome? { results[p.id] }

    /// Probes one profile. Repeat calls while a probe is in flight are ignored.
    func ping(_ p: Profile) {
        guard !inFlight.contains(p.id) else { return }
        guard let target = Self.probeURL(for: p) else {
            results[p.id] = .failed("нет URL")
            return
        }
        inFlight.insert(p.id)
        results[p.id] = .running

        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 10
        cfg.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        let session = URLSession(configuration: cfg)

        var req = URLRequest(url: target)
        req.httpMethod = "HEAD"
        let started = Date()

        session.dataTask(with: req) { [weak self] _, response, err in
            let elapsed = Int(Date().timeIntervalSince(started) * 1000)
            Task { @MainActor in
                guard let self = self else { return }
                self.inFlight.remove(p.id)
                if let err = err {
                    self.results[p.id] = .failed((err as NSError).localizedDescription)
                } else if response is HTTPURLResponse {
                    self.results[p.id] = .ms(elapsed)
                } else {
                    self.results[p.id] = .failed("нет ответа")
                }
            }
        }.resume()
    }

    func pingAll(_ profiles: [Profile]) {
        for p in profiles where p.isValid { ping(p) }
    }

    /// The host to probe for a profile. MAX carries no document URL, so it has
    /// no meaningful target and returns nil.
    static func probeURL(for p: Profile) -> URL? {
        guard p.transportKind != .max else { return nil }
        let first = p.url
            .split(separator: ",")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .first { !$0.isEmpty }
        guard let s = first, let u = URL(string: s), u.host != nil else { return nil }
        // Probe the host root rather than the document itself: the document path
        // may redirect into the captcha/login flow, which says nothing about
        // reachability and costs an extra round trip.
        var comps = URLComponents()
        comps.scheme = u.scheme ?? "https"
        comps.host = u.host
        comps.path = "/"
        return comps.url
    }
}
