import Foundation
import Network

/// How requests reach the server.
public enum RouteMode: String, Codable, Sendable, CaseIterable, Identifiable {
    /// Direct first; through the tunnel when direct does not get through.
    case auto
    case directOnly
    case tunnelOnly
    public var id: String { rawValue }
}

/// The route a request actually took.
public enum Route: String, Sendable {
    case direct
    case tunnel
}

/// What the API client needs from the in-app OpenFlux core.
public protocol TunnelGate: Sendable {
    /// Starts the tunnel if needed and waits until it carries traffic.
    /// Returns false when there is no channel or it does not connect.
    func ensureConnected() async -> Bool
}

public enum APIError: Error, LocalizedError, Sendable, Equatable {
    case notConfigured
    case unreachable(String)
    case server(status: Int, code: String, message: String)
    case decoding(String)
    case unauthorized

    public var errorDescription: String? {
        switch self {
        case .notConfigured: return String(localized: "Не указан адрес сервера")
        case .unreachable(let m): return String(localized: "Сервер недоступен: \(m)")
        case .server(_, _, let m): return m
        case .decoding(let m): return String(localized: "Неожиданный ответ сервера: \(m)")
        case .unauthorized: return String(localized: "Нужно войти заново")
        }
    }

    public var code: String? {
        if case .server(_, let c, _) = self { return c }
        return nil
    }
}

/// URLSessions for both routes. Tests inject configurations with stub
/// URLProtocols.
public struct SessionFactory: Sendable {
    public var direct: @Sendable () -> URLSessionConfiguration
    public var tunnel: @Sendable (_ socksPort: UInt16) -> URLSessionConfiguration

    public init(direct: @escaping @Sendable () -> URLSessionConfiguration,
                tunnel: @escaping @Sendable (UInt16) -> URLSessionConfiguration) {
        self.direct = direct
        self.tunnel = tunnel
    }

    public static let live = SessionFactory(
        direct: {
            let c = URLSessionConfiguration.ephemeral
            c.timeoutIntervalForRequest = 60
            c.waitsForConnectivity = false
            return c
        },
        tunnel: { port in
            let c = URLSessionConfiguration.ephemeral
            c.timeoutIntervalForRequest = 120 // document carriers are slow
            c.waitsForConnectivity = false
            c.proxyConfigurations = [
                ProxyConfiguration(socksv5Proxy: .hostPort(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: port) ?? 1080)),
            ]
            return c
        })
}

/// Persists the auth tokens (Keychain).
public protocol TokenStore: Sendable {
    func load() -> Tokens?
    func save(_ tokens: Tokens?)
}

public struct KeychainTokenStore: TokenStore {
    let keychain: Keychain
    public init(keychain: Keychain = Keychain()) { self.keychain = keychain }
    public func load() -> Tokens? { try? keychain.codable(Tokens.self, for: KeychainKeys.tokens) }
    public func save(_ tokens: Tokens?) {
        if let tokens { try? keychain.setCodable(tokens, for: KeychainKeys.tokens) } else { try? keychain.remove(KeychainKeys.tokens) }
    }
}

public struct APIRequest: Sendable {
    public var method: String
    public var path: String
    public var query: [URLQueryItem]
    public var body: Data?
    public var auth: Bool

    public init(_ method: String, _ path: String, query: [URLQueryItem] = [], body: Data? = nil, auth: Bool = true) {
        self.method = method
        self.path = path
        self.query = query
        self.body = body
        self.auth = auth
    }

    public static func json<T: Encodable>(_ method: String, _ path: String, _ value: T, auth: Bool = true) -> APIRequest {
        APIRequest(method, path, body: try? JSONCoding.encoder.encode(value), auth: auth)
    }
}

/// The AIHub API client. In auto mode it probes the direct route with a
/// short timeout and falls back to the tunnel's SOCKS5 proxy; a route that
/// worked is kept for a while so not every request pays for the probe.
public actor APIClient {
    public private(set) var baseURL: URL?
    public private(set) var mode: RouteMode
    public private(set) var lastRoute: Route?
    public var onRouteChange: (@Sendable (Route) -> Void)?
    public var onSignedOut: (@Sendable () -> Void)?

    private let tokens: TokenStore
    private let tunnel: TunnelGate?
    private let factory: SessionFactory
    private let socksPort: UInt16
    private var directSession: URLSession
    private var tunnelSession: URLSession
    private var probeSession: URLSession
    private var chosen: (route: Route, until: Date)?
    private var refreshing: Task<Bool, Never>?
    private let probeTimeout: TimeInterval
    private let routeTTL: TimeInterval

    public init(baseURL: URL?, mode: RouteMode = .auto, tokens: TokenStore, tunnel: TunnelGate?,
                factory: SessionFactory = .live, socksPort: UInt16 = 1080,
                probeTimeout: TimeInterval = 4, routeTTL: TimeInterval = 120) {
        self.baseURL = baseURL
        self.mode = mode
        self.tokens = tokens
        self.tunnel = tunnel
        self.factory = factory
        self.socksPort = socksPort
        self.probeTimeout = probeTimeout
        self.routeTTL = routeTTL
        directSession = URLSession(configuration: factory.direct())
        tunnelSession = URLSession(configuration: factory.tunnel(socksPort))
        let probe = factory.direct()
        probe.timeoutIntervalForRequest = probeTimeout
        probe.timeoutIntervalForResource = probeTimeout
        probeSession = URLSession(configuration: probe)
    }

    public func configure(baseURL: URL?, mode: RouteMode) {
        self.baseURL = baseURL
        self.mode = mode
        chosen = nil
    }

    public func setCallbacks(onRouteChange: (@Sendable (Route) -> Void)?, onSignedOut: (@Sendable () -> Void)?) {
        self.onRouteChange = onRouteChange
        self.onSignedOut = onSignedOut
    }

    public var isSignedIn: Bool { tokens.load() != nil }

    public func setTokens(_ t: Tokens?) { tokens.save(t) }

    /// Forget the cached route (network changed, app came back).
    public func resetRoute() { chosen = nil }

    // MARK: routing

    private func isTransportFailure(_ error: Error) -> Bool {
        guard let u = error as? URLError else { return false }
        switch u.code {
        case .timedOut, .cannotConnectToHost, .cannotFindHost, .dnsLookupFailed, .networkConnectionLost,
             .secureConnectionFailed, .serverCertificateUntrusted, .cannotLoadFromNetwork, .notConnectedToInternet,
             .internationalRoamingOff, .dataNotAllowed, .resourceUnavailable, .badServerResponse:
            return true
        default:
            return false
        }
    }

    /// Picks the route for the next request.
    func route() async throws -> Route {
        guard let baseURL else { throw APIError.notConfigured }
        switch mode {
        case .directOnly: return .direct
        case .tunnelOnly:
            guard let tunnel, await tunnel.ensureConnected() else { throw APIError.unreachable(String(localized: "туннель не подключён")) }
            return .tunnel
        case .auto:
            if let c = chosen, c.until > Date() { return c.route }
            var probe = URLRequest(url: baseURL.appending(path: "healthz"))
            probe.timeoutInterval = probeTimeout
            if let (_, resp) = try? await probeSession.data(for: probe), (resp as? HTTPURLResponse)?.statusCode == 200 {
                remember(.direct)
                return .direct
            }
            if let tunnel, await tunnel.ensureConnected() {
                remember(.tunnel)
                return .tunnel
            }
            // Neither: try direct anyway so the user sees the real error.
            return .direct
        }
    }

    private func remember(_ r: Route) {
        chosen = (r, Date().addingTimeInterval(routeTTL))
        if lastRoute != r {
            lastRoute = r
            onRouteChange?(r)
        } else {
            lastRoute = r
        }
    }

    private func session(for r: Route) -> URLSession { r == .direct ? directSession : tunnelSession }

    private func urlRequest(_ r: APIRequest) throws -> URLRequest {
        guard let baseURL else { throw APIError.notConfigured }
        var comps = URLComponents(url: baseURL.appending(path: r.path), resolvingAgainstBaseURL: false)!
        if !r.query.isEmpty { comps.queryItems = r.query }
        var req = URLRequest(url: comps.url!)
        req.httpMethod = r.method
        if let body = r.body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        if r.auth, let t = tokens.load() {
            req.setValue("Bearer \(t.accessToken)", forHTTPHeaderField: "Authorization")
        }
        return req
    }

    /// Sends a request on the chosen route; a transport failure on the
    /// direct route in auto mode is retried once through the tunnel.
    private func perform(_ r: APIRequest) async throws -> (Data, HTTPURLResponse) {
        let first = try await route()
        do {
            return try await perform(r, on: first)
        } catch where mode == .auto && first == .direct && isTransportFailure(error) {
            chosen = nil
            if let tunnel, await tunnel.ensureConnected() {
                remember(.tunnel)
                return try await perform(r, on: .tunnel)
            }
            throw APIError.unreachable(error.localizedDescription)
        }
    }

    private func perform(_ r: APIRequest, on route: Route) async throws -> (Data, HTTPURLResponse) {
        let req = try urlRequest(r)
        let (data, resp) = try await session(for: route).data(for: req)
        guard let http = resp as? HTTPURLResponse else { throw APIError.unreachable("no HTTP response") }
        if lastRoute != route { remember(route) }
        return (data, http)
    }

    private func failure(_ data: Data, _ http: HTTPURLResponse) -> APIError {
        if let body = try? JSONCoding.decoder.decode(APIErrorBody.self, from: data) {
            return .server(status: http.statusCode, code: body.error.code, message: body.error.message)
        }
        return .server(status: http.statusCode, code: "http_\(http.statusCode)", message: HTTPURLResponse.localizedString(forStatusCode: http.statusCode))
    }

    /// Runs a request, refreshing the access token once on 401.
    public func raw(_ r: APIRequest) async throws -> Data {
        do {
            var (data, http) = try await perform(r)
            if http.statusCode == 401, r.auth, await refresh() {
                (data, http) = try await perform(r)
            }
            if http.statusCode == 401, r.auth {
                signedOut()
                throw APIError.unauthorized
            }
            guard (200..<300).contains(http.statusCode) else { throw failure(data, http) }
            return data
        } catch let e as APIError {
            throw e
        } catch is CancellationError {
            throw CancellationError()
        } catch let u as URLError where u.code == .cancelled {
            throw CancellationError()
        } catch {
            throw APIError.unreachable(error.localizedDescription)
        }
    }

    public func send<T: Decodable & Sendable>(_ r: APIRequest, _ type: T.Type = T.self) async throws -> T {
        let data = try await raw(r)
        do {
            return try JSONCoding.decoder.decode(T.self, from: data)
        } catch {
            throw APIError.decoding(String(describing: error))
        }
    }

    public func sendNoContent(_ r: APIRequest) async throws {
        _ = try await raw(r)
    }

    private func signedOut() {
        tokens.save(nil)
        onSignedOut?()
    }

    /// Rotates the refresh token; concurrent callers share one refresh.
    func refresh() async -> Bool {
        if let refreshing { return await refreshing.value }
        guard let current = tokens.load() else { return false }
        let task = Task<Bool, Never> {
            struct Body: Encodable { var refreshToken: String }
            let r = APIRequest.json("POST", "v1/auth/refresh", Body(refreshToken: current.refreshToken), auth: false)
            guard let (data, http) = try? await perform(r), http.statusCode == 200,
                  let t = try? JSONCoding.decoder.decode(Tokens.self, from: data) else { return false }
            tokens.save(t)
            return true
        }
        refreshing = task
        let ok = await task.value
        refreshing = nil
        return ok
    }

    // MARK: streaming

    /// POST /v1/chat as a stream of events. Cancelling the consuming task
    /// closes the connection, which stops the provider on the server.
    public func chat(_ body: ChatRequest) -> AsyncThrowingStream<ChatEvent, Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    try await self.streamChat(body, retryAuth: true, continuation)
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    private func streamChat(_ body: ChatRequest, retryAuth: Bool, _ c: AsyncThrowingStream<ChatEvent, Error>.Continuation) async throws {
        let r = APIRequest.json("POST", "v1/chat", body)
        let route = try await route()
        var req = try urlRequest(r)
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        req.timeoutInterval = 300
        let bytes: URLSession.AsyncBytes
        let resp: URLResponse
        do {
            (bytes, resp) = try await session(for: route).bytes(for: req)
        } catch where mode == .auto && route == .direct && isTransportFailure(error) {
            chosen = nil
            guard let tunnel, await tunnel.ensureConnected() else { throw APIError.unreachable(error.localizedDescription) }
            remember(.tunnel)
            (bytes, resp) = try await session(for: .tunnel).bytes(for: req)
        }
        guard let http = resp as? HTTPURLResponse else { throw APIError.unreachable("no HTTP response") }
        if http.statusCode == 401, retryAuth {
            guard await refresh() else {
                signedOut()
                throw APIError.unauthorized
            }
            return try await streamChat(body, retryAuth: false, c)
        }
        guard http.statusCode == 200 else {
            var data = Data()
            for try await b in bytes {
                data.append(b)
                if data.count > 64 << 10 { break }
            }
            throw failure(data, http)
        }
        var parser = SSEParser()
        for try await byte in bytes {
            try Task.checkCancellation()
            if let e = parser.push(byte), let ev = ChatEvent(e) {
                c.yield(ev)
            }
        }
        if let e = parser.finish(), let ev = ChatEvent(e) { c.yield(ev) }
    }
}
