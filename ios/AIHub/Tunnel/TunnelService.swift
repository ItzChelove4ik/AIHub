import AIHubKit
import Foundation

/// The in-app OpenFlux client: a local SOCKS5 proxy on 127.0.0.1:1080 that
/// reaches the server through the channel's carriers (Yandex/Mail.ru
/// documents, cups.online, direct). No Network Extension: the core runs in
/// the app's process, so it stops when the app goes to the background.
actor TunnelService: TunnelGate {
    struct Stats: Decodable, Sendable, Equatable {
        var running: Bool
        var connected: Bool
        var bytesSent: Int64
        var bytesReceived: Int64
        var uptimeSec: Int64
        var mode: String
        var transport: String

        static let idle = Stats(running: false, connected: false, bytesSent: 0, bytesReceived: 0, uptimeSec: 0, mode: "", transport: "")
    }

    struct Captcha: Sendable, Equatable {
        var url: URL
        /// Pages the exit needs passed load through this HTTP proxy.
        var remoteProxy: String?
        var reason: String
    }

    struct LinkInfo: Sendable, Equatable {
        var name: String
        /// Carrier types in priority order, e.g. ["vyandex", "cupsonline", "direct"].
        var transports: [String]
    }

    enum Failure: Error, LocalizedError, Equatable {
        case noLink
        case badLink(String)
        case start(Int32)

        var errorDescription: String? {
            switch self {
            case .noLink: return String(localized: "Нет канала: создайте его или импортируйте ссылку openflux://")
            case .badLink(let code): return String(localized: "Ссылка не подходит (\(code))")
            case .start(let rc):
                switch rc {
                case 1: return String(localized: "Туннель уже запущен")
                case 4: return String(localized: "Порт 1080 занят")
                case 6: return String(localized: "В ссылке нет ключа шифрования")
                default: return String(localized: "Ядро не запустилось (код \(rc))")
                }
            }
        }
    }

    static let socksPort: UInt16 = 1080
    private let keychain: Keychain
    private let defaults: UserDefaults
    private var logBuffer = ""

    init(keychain: Keychain = Keychain(), defaults: UserDefaults = .standard) {
        self.keychain = keychain
        self.defaults = defaults
        // Cookies of document carriers persist across launches.
        if let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first {
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            _ = OpenFluxCore.setCookieStore(dir.appending(path: "openflux-cookies.json").path)
        }
    }

    // MARK: link

    var hasLink: Bool { (try? keychain.string(for: KeychainKeys.channelLink)) != nil }

    private struct Decoded: Decodable {
        struct Config: Decodable {
            var name: String?
            var secret: String?
            var transports: [T]?
            struct T: Decodable { var type: String; var priority: Int? }
        }
        var config: Config?
        var session: String?
        var error: String?
        var code: String?
    }

    private func decode(_ link: String) throws -> Decoded {
        let raw = OpenFluxCore.shareDecode(link.trimmingCharacters(in: .whitespacesAndNewlines))
        guard let d = try? JSONDecoder().decode(Decoded.self, from: Data(raw.utf8)) else { throw Failure.badLink("parse") }
        if d.config == nil || d.session == nil { throw Failure.badLink(d.code ?? d.error ?? "invalid") }
        return d
    }

    /// Checks and stores a link (the channel key is inside: Keychain only).
    func importLink(_ link: String) throws -> LinkInfo {
        let d = try decode(link)
        try keychain.setString(link.trimmingCharacters(in: .whitespacesAndNewlines), for: KeychainKeys.channelLink)
        stop()
        return info(d)
    }

    func linkInfo() -> LinkInfo? {
        guard let link = try? keychain.string(for: KeychainKeys.channelLink), let d = try? decode(link) else { return nil }
        return info(d)
    }

    private func info(_ d: Decoded) -> LinkInfo {
        let ts = (d.config?.transports ?? []).sorted { ($0.priority ?? 0) > ($1.priority ?? 0) }.map(\.type)
        return LinkInfo(name: d.config?.name ?? "AIHub", transports: ts)
    }

    func forgetLink() {
        stop()
        try? keychain.remove(KeychainKeys.channelLink)
    }

    // MARK: settings

    /// Carrier types in the order the user wants them tried (overrides the
    /// link's priorities); empty = the link's own order.
    var priorityOrder: [String] {
        get { defaults.stringArray(forKey: "tunnel.priorities") ?? [] }
    }

    func setPriorityOrder(_ order: [String]) {
        defaults.set(order, forKey: "tunnel.priorities")
        if OpenFluxCore.isRunning { stop() }
    }

    func setDoTResolver(_ spec: String) {
        defaults.set(spec, forKey: "tunnel.dot")
        OpenFluxCore.setDoTResolver(spec)
    }

    var dotResolver: String { defaults.string(forKey: "tunnel.dot") ?? "" }

    func setDebugLevel(_ n: Int) {
        defaults.set(n, forKey: "tunnel.debug")
        OpenFluxCore.setDebugLevel(n)
    }

    var debugLevel: Int { defaults.integer(forKey: "tunnel.debug") }

    /// The session JSON with the user's priority order applied.
    private func applyPriorities(_ session: String) -> String {
        let order = priorityOrder
        guard !order.isEmpty,
              var obj = try? JSONSerialization.jsonObject(with: Data(session.utf8)) as? [String: Any],
              var ts = obj["transports"] as? [[String: Any]] else { return session }
        for i in ts.indices {
            if let type = ts[i]["type"] as? String, let pos = order.firstIndex(of: type) {
                ts[i]["priority"] = 100 - pos * 10
            }
        }
        obj["transports"] = ts
        guard let data = try? JSONSerialization.data(withJSONObject: obj) else { return session }
        return String(decoding: data, as: UTF8.self)
    }

    // MARK: running

    func start() throws {
        if OpenFluxCore.isRunning { return }
        guard let link = try? keychain.string(for: KeychainKeys.channelLink) else { throw Failure.noLink }
        let d = try decode(link)
        guard let secret = d.config?.secret, let session = d.session else { throw Failure.badLink("no_secret") }
        OpenFluxCore.setDebugLevel(debugLevel)
        OpenFluxCore.setDoTResolver(dotResolver)
        let rc = OpenFluxCore.startSession(specs: applyPriorities(session), secret: secret,
                                           socksAddr: "127.0.0.1:\(TunnelService.socksPort)")
        guard rc == 0 else { throw Failure.start(rc) }
    }

    func stop() {
        if OpenFluxCore.isRunning { OpenFluxCore.stop() }
    }

    func restart() throws {
        stop()
        try start()
    }

    /// TunnelGate: starts the core and waits up to `timeout` for a carrier.
    func ensureConnected() async -> Bool {
        await ensureConnected(timeout: 25)
    }

    func ensureConnected(timeout: TimeInterval) async -> Bool {
        guard hasLink else { return false }
        if !OpenFluxCore.isRunning {
            do { try start() } catch { return false }
        }
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if OpenFluxCore.isConnected { return true }
            try? await Task.sleep(for: .milliseconds(300))
            if Task.isCancelled { return false }
        }
        return OpenFluxCore.isConnected
    }

    func stats() -> Stats {
        guard let s = try? JSONDecoder().decode(Stats.self, from: Data(OpenFluxCore.statsJSON().utf8)) else { return .idle }
        return s
    }

    func captcha() -> Captcha? {
        let local = OpenFluxCore.captchaPending()
        let remote = OpenFluxCore.remoteCaptchaPending()
        let reason = OpenFluxCore.captchaReason()
        if let url = URL(string: remote), !remote.isEmpty {
            let proxy = OpenFluxCore.remoteCaptchaProxy()
            return Captcha(url: url, remoteProxy: proxy.isEmpty ? nil : proxy, reason: reason)
        }
        if let url = URL(string: local), !local.isEmpty {
            return Captcha(url: url, remoteProxy: nil, reason: reason)
        }
        return nil
    }

    /// Hands the cookies of a passed check to the carrier that asked.
    func submitCaptcha(cookieHeader: String) -> Bool { OpenFluxCore.applyCaptchaCookies(cookieHeader) > 0 }

    func cancelCaptcha() { OpenFluxCore.cancelCaptcha() }

    /// New core log lines since the last call, kept in a bounded buffer.
    func log() -> String {
        let new = OpenFluxCore.readLog()
        if !new.isEmpty {
            logBuffer += new
            if logBuffer.utf8.count > 200_000 { logBuffer = String(logBuffer.suffix(150_000)) }
        }
        return logBuffer
    }

    func clearLog() { logBuffer = "" }
}
