import XCTest
@testable import AIHubKit

/// Stub routes: the direct one can be made unreachable, the tunnel one
/// always answers. Each records the paths it served.
class StubProtocol: URLProtocol, @unchecked Sendable {
    enum Kind: String { case direct, tunnel }
    class var kind: Kind { .direct }

    nonisolated(unsafe) static var directDown = false
    nonisolated(unsafe) static var served: [String] = []
    nonisolated(unsafe) static var unauthorizedOnce = false
    static let lock = NSLock()

    static func clearServed() {
        lock.lock(); defer { lock.unlock() }
        served = []
    }

    static func reset() {
        lock.lock(); defer { lock.unlock() }
        directDown = false
        served = []
        unauthorizedOnce = false
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let path = request.url?.path ?? ""
        let kind = Self.kind
        StubProtocol.lock.lock()
        let down = StubProtocol.directDown
        var unauthorized = false
        if StubProtocol.unauthorizedOnce, path.hasSuffix("/v1/me") {
            StubProtocol.unauthorizedOnce = false
            unauthorized = true
        }
        StubProtocol.served.append("\(kind.rawValue) \(path)")
        StubProtocol.lock.unlock()

        if kind == .direct && down {
            client?.urlProtocol(self, didFailWithError: URLError(.cannotConnectToHost))
            return
        }
        var status = 200
        var body = Data(#"{"status":"ok"}"#.utf8)
        if path.hasSuffix("/v1/me") {
            if unauthorized {
                status = 401
                body = Data(#"{"error":{"code":"unauthorized","message":"expired"}}"#.utf8)
            } else {
                body = Data(#"{"user":{"id":1,"email":"a@example.com","is_admin":true,"created_at":"2026-11-24T18:09:15.123456Z"},"device_id":5}"#.utf8)
            }
        } else if path.hasSuffix("/v1/auth/refresh") {
            body = Data(#"{"access_token":"new","refresh_token":"r2","expires_at":"2026-11-24T18:24:15Z","token_type":"Bearer"}"#.utf8)
        } else if path.hasSuffix("/v1/missing") {
            status = 404
            body = Data(#"{"error":{"code":"not_found","message":"не найдено"}}"#.utf8)
        }
        let resp = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

final class DirectStub: StubProtocol, @unchecked Sendable {
    override class var kind: Kind { .direct }
}

final class TunnelStub: StubProtocol, @unchecked Sendable {
    override class var kind: Kind { .tunnel }
}

struct FakeTunnel: TunnelGate {
    let up: Bool
    func ensureConnected() async -> Bool { up }
}

final class MemoryTokens: TokenStore, @unchecked Sendable {
    private let lock = NSLock()
    private var tokens: Tokens?
    init(_ t: Tokens?) { tokens = t }
    func load() -> Tokens? { lock.lock(); defer { lock.unlock() }; return tokens }
    func save(_ t: Tokens?) { lock.lock(); tokens = t; lock.unlock() }
}

final class APIClientTests: XCTestCase {
    let base = URL(string: "https://aihub.test")!

    static func config(_ kind: StubProtocol.Kind) -> URLSessionConfiguration {
        let c = URLSessionConfiguration.ephemeral
        c.protocolClasses = [kind == .direct ? DirectStub.self : TunnelStub.self]
        return c
    }

    let factory = SessionFactory(direct: { APIClientTests.config(.direct) }, tunnel: { _ in APIClientTests.config(.tunnel) })

    func tokens() -> MemoryTokens {
        MemoryTokens(Tokens(accessToken: "old", refreshToken: "r1", expiresAt: Date(), tokenType: "Bearer"))
    }

    override func setUp() { StubProtocol.reset() }

    func testDirectWhenReachable() async throws {
        let api = APIClient(baseURL: base, tokens: tokens(), tunnel: FakeTunnel(up: true), factory: factory)
        let me = try await api.send(APIRequest("GET", "v1/me"), MeResponse.self)
        XCTAssertEqual(me.user.email, "a@example.com")
        let route = await api.lastRoute
        XCTAssertEqual(route, .direct)
        XCTAssertTrue(StubProtocol.served.allSatisfy { $0.hasPrefix("direct") })
    }

    func testFallsBackToTunnel() async throws {
        StubProtocol.directDown = true
        let api = APIClient(baseURL: base, tokens: tokens(), tunnel: FakeTunnel(up: true), factory: factory)
        _ = try await api.send(APIRequest("GET", "v1/me"), MeResponse.self)
        let route = await api.lastRoute
        XCTAssertEqual(route, .tunnel)
        XCTAssertTrue(StubProtocol.served.contains("tunnel /v1/me"))
        // The route is remembered: the next request skips the probe.
        StubProtocol.clearServed()
        _ = try await api.send(APIRequest("GET", "v1/me"), MeResponse.self)
        XCTAssertEqual(StubProtocol.served, ["tunnel /v1/me"])
    }

    func testDirectOnlyDoesNotUseTunnel() async {
        StubProtocol.directDown = true
        let api = APIClient(baseURL: base, mode: .directOnly, tokens: tokens(), tunnel: FakeTunnel(up: true), factory: factory)
        do {
            _ = try await api.send(APIRequest("GET", "v1/me"), MeResponse.self)
            XCTFail("expected failure")
        } catch {
            XCTAssertFalse(StubProtocol.served.contains { $0.hasPrefix("tunnel") })
        }
    }

    func testNoTunnelMeansUnreachable() async {
        StubProtocol.directDown = true
        let api = APIClient(baseURL: base, tokens: tokens(), tunnel: FakeTunnel(up: false), factory: factory)
        do {
            _ = try await api.send(APIRequest("GET", "v1/me"), MeResponse.self)
            XCTFail("expected failure")
        } catch let e as APIError {
            if case .unreachable = e {} else { XCTFail("got \(e)") }
        } catch {
            XCTFail("got \(error)")
        }
    }

    func testRefreshOn401() async throws {
        StubProtocol.unauthorizedOnce = true
        let store = tokens()
        let api = APIClient(baseURL: base, tokens: store, tunnel: nil, factory: factory)
        _ = try await api.send(APIRequest("GET", "v1/me"), MeResponse.self)
        XCTAssertEqual(store.load()?.accessToken, "new")
        XCTAssertTrue(StubProtocol.served.contains("direct /v1/auth/refresh"))
    }

    func testServerErrorIsDecoded() async {
        let api = APIClient(baseURL: base, tokens: tokens(), tunnel: nil, factory: factory)
        do {
            try await api.sendNoContent(APIRequest("GET", "v1/missing"))
            XCTFail("expected failure")
        } catch let e as APIError {
            XCTAssertEqual(e, .server(status: 404, code: "not_found", message: "не найдено"))
        } catch {
            XCTFail("got \(error)")
        }
    }
}
