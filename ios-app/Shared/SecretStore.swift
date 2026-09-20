import Foundation
import Security

enum SecretStoreError: Error, LocalizedError {
    case unavailable, invalidRecord, invalidSecret
    var errorDescription: String? {
        switch self {
        case .unavailable: return "Shared Keychain unavailable. Check signing and unlock the device."
        case .invalidRecord: return "VPN credentials are missing or outdated. Save the profile in OpenFlux again."
        case .invalidSecret: return "Encryption key must contain at least 16 characters."
        }
    }
}

protocol SecretStore {
    func read(account: String) throws -> Data?
    func write(_ data: Data, account: String) throws
    func delete(account: String) throws
}

/// Both targets use the same signed access group. No UserDefaults, iCloud,
/// providerConfiguration, logging, or plaintext fallback for credential data.
struct KeychainSecretStore: SecretStore {
    private func query(_ account: String) throws -> [String: Any] {
        guard let group = Bundle.main.object(forInfoDictionaryKey: "OpenFluxKeychainGroup") as? String,
              !group.isEmpty, !group.contains("$(") else { throw SecretStoreError.unavailable }
        return [kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: "OpenFlux.transport.v1",
                kSecAttrAccount as String: account,
                kSecAttrAccessGroup as String: group,
                kSecAttrSynchronizable as String: false]
    }

    func read(account: String) throws -> Data? {
        var q = try query(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &item)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = item as? Data else { throw SecretStoreError.unavailable }
        return data
    }

    func write(_ data: Data, account: String) throws {
        var q = try query(account)
        let values: [String: Any] = [kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly]
        let status = SecItemUpdate(q as CFDictionary, values as CFDictionary)
        if status == errSecItemNotFound {
            q.merge(values) { _, new in new }
            guard SecItemAdd(q as CFDictionary, nil) == errSecSuccess else { throw SecretStoreError.unavailable }
        } else if status != errSecSuccess { throw SecretStoreError.unavailable }
    }

    func delete(account: String) throws {
        let status = SecItemDelete(try query(account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw SecretStoreError.unavailable }
    }
}

struct AppCredentials: Codable {
    var encryptionSecret = ""
    var maxToken = ""
    static let account = "app-settings"
}

/// Immutable snapshot referenced by a random, non-secret ID in the VPN profile.
/// preparedKey is the upstream scrypt result, not an alternative KDF/wire format.
struct TunnelCredentials: Codable {
    let transport: String
    let url: String
    let preparedKey: String
    let maxToken: String
}
