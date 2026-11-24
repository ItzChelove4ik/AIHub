import Security
import XCTest
@testable import AIHubKit

final class KeychainTests: XCTestCase {
    func testRoundTrip() throws {
        let kc = Keychain(service: "com.itzchelove4ik.aihub.tests.\(UUID().uuidString)")
        do {
            try kc.setString("openflux://v1/secret", for: KeychainKeys.channelLink)
        } catch Keychain.Failure.status(let s) where s == errSecMissingEntitlement || s == errSecNotAvailable {
            throw XCTSkip("Keychain unavailable for a hostless test bundle (\(s))")
        }
        XCTAssertEqual(try kc.string(for: KeychainKeys.channelLink), "openflux://v1/secret")
        try kc.setString("updated", for: KeychainKeys.channelLink)
        XCTAssertEqual(try kc.string(for: KeychainKeys.channelLink), "updated")

        let t = Tokens(accessToken: "a", refreshToken: "b", expiresAt: Date(timeIntervalSince1970: 1_800_000_000), tokenType: "Bearer")
        try kc.setCodable(t, for: KeychainKeys.tokens)
        XCTAssertEqual(try kc.codable(Tokens.self, for: KeychainKeys.tokens), t)

        try kc.remove(KeychainKeys.channelLink)
        try kc.remove(KeychainKeys.tokens)
        XCTAssertNil(try kc.string(for: KeychainKeys.channelLink))
        XCTAssertNoThrow(try kc.remove(KeychainKeys.channelLink)) // removing twice is fine
    }

    func testDatesFromGo() {
        XCTAssertNotNil(JSONCoding.parseDate("2026-11-24T18:09:15.123456789Z"))
        XCTAssertNotNil(JSONCoding.parseDate("2026-11-24T18:09:15Z"))
        XCTAssertNotNil(JSONCoding.parseDate("2026-11-24T21:09:15.5+03:00"))
    }
}
