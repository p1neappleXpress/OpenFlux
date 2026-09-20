import Foundation
import Combine

/// User-initiated IPv4 check in the containing app, not the tunnel extension
/// (extension-originated requests can bypass the tunnel).
@MainActor
final class ConnectionCheck: ObservableObject {
    @Published private(set) var running = false
    @Published private(set) var result: String?
    private var task: Task<Void, Never>?
    private var generation = UUID()

    func cancel() {
        generation = UUID()
        task?.cancel()
        task = nil
        running = false
        result = nil
    }

    func start(socksAddress: String?) {
        cancel()
        let id = generation
        running = true
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 20
        config.timeoutIntervalForResource = 25
        config.urlCache = nil
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        if let address = socksAddress {
            let parts = address.split(separator: ":")
            guard parts.count == 2, let port = Int(parts[1]), (1...65535).contains(port) else {
                running = false
                result = "Invalid local proxy address."
                return
            }
            config.connectionProxyDictionary = ["SOCKSEnable": 1, "SOCKSProxy": String(parts[0]), "SOCKSPort": port]
        }
        task = Task {
            let session = URLSession(configuration: config)
            defer { session.invalidateAndCancel() }
            let message: String
            do {
                let (data, response) = try await session.data(from: URL(string: "https://api.ipify.org?format=json")!)
                try Task.checkCancellation()
                guard let http = response as? HTTPURLResponse, http.statusCode == 200,
                      data.count <= 1024,
                      let value = try? JSONDecoder().decode(IPResponse.self, from: data),
                      Self.isIPv4(value.ip) else { throw URLError(.badServerResponse) }
                message = "IPv4 internet reachable. Public IP: \(value.ip)"
            } catch {
                if Task.isCancelled { return }
                switch (error as? URLError)?.code {
                case .timedOut: message = "The check timed out. Check the exit node and try again."
                case .cannotFindHost, .dnsLookupFailed: message = "Could not resolve the test address. Check DNS and the exit node."
                case .notConnectedToInternet: message = "No internet connection. Check Wi-Fi or mobile data."
                default: message = "Could not reach the test service. Try again or check the exit node."
                }
            }
            guard generation == id else { return }
            result = message
            running = false
            task = nil
        }
    }

    private struct IPResponse: Decodable { let ip: String }
    private static func isIPv4(_ value: String) -> Bool {
        let parts = value.split(separator: ".", omittingEmptySubsequences: false)
        return parts.count == 4 && parts.allSatisfy {
            !$0.isEmpty && $0.allSatisfy { $0.isASCII && $0.isNumber } && UInt8($0) != nil
        }
    }
}
