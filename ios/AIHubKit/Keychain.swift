import Foundation
import Security

/// Small Keychain wrapper for secrets: tokens and the channel's openflux://
/// link (it carries the channel key). Items stay on this device only.
public struct Keychain: Sendable {
    public enum Failure: Error, Equatable {
        case status(OSStatus)
    }

    public let service: String

    public init(service: String = "com.itzchelove4ik.aihub") {
        self.service = service
    }

    private func base(_ key: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: key,
        ]
    }

    public func set(_ value: Data, for key: String) throws {
        var query = base(key)
        let attrs: [String: Any] = [
            kSecValueData as String: value,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let status = SecItemUpdate(query as CFDictionary, attrs as CFDictionary)
        if status == errSecItemNotFound {
            query.merge(attrs) { $1 }
            let add = SecItemAdd(query as CFDictionary, nil)
            guard add == errSecSuccess else { throw Failure.status(add) }
        } else if status != errSecSuccess {
            throw Failure.status(status)
        }
    }

    public func data(for key: String) throws -> Data? {
        var query = base(key)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &out)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess else { throw Failure.status(status) }
        return out as? Data
    }

    public func remove(_ key: String) throws {
        let status = SecItemDelete(base(key) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else { throw Failure.status(status) }
    }

    public func setString(_ value: String, for key: String) throws { try set(Data(value.utf8), for: key) }

    public func string(for key: String) throws -> String? {
        try data(for: key).map { String(decoding: $0, as: UTF8.self) }
    }

    public func setCodable<T: Encodable>(_ value: T, for key: String) throws {
        try set(JSONCoding.encoder.encode(value), for: key)
    }

    public func codable<T: Decodable>(_ type: T.Type, for key: String) throws -> T? {
        guard let d = try data(for: key) else { return nil }
        return try JSONCoding.decoder.decode(T.self, from: d)
    }
}

public enum KeychainKeys {
    public static let tokens = "tokens"
    public static let channelLink = "channel.link"
}
