import AIHubKit
import Foundation
import Observation
import SwiftUI
import UIKit

enum AppTab: Hashable {
    case home, server, settings
}

enum ThemeChoice: String, CaseIterable, Identifiable {
    case system, light, dark
    var id: String { rawValue }
    var colorScheme: ColorScheme? {
        switch self {
        case .system: nil
        case .light: .light
        case .dark: .dark
        }
    }
    var title: LocalizedStringKey {
        switch self {
        case .system: "Как в системе"
        case .light: "Светлая"
        case .dark: "Тёмная"
        }
    }
}

/// What the capsule above the tab bar says.
enum ConnectionStatus: Equatable {
    case unknown
    case direct
    case tunnel(transport: String)
    case connecting
    case needsCheck
    case offline(String)
}

/// App-wide state: settings, the signed-in user, the API client and the
/// tunnel. Lives on the main actor; network work happens in the actors it
/// owns.
@MainActor
@Observable
final class AppModel {
    // Settings (UserDefaults; secrets are in the Keychain).
    var serverURLString: String {
        didSet { UserDefaults.standard.set(serverURLString, forKey: "server.url"); reconfigure() }
    }
    var routeMode: RouteMode {
        didSet { UserDefaults.standard.set(routeMode.rawValue, forKey: "route.mode"); reconfigure() }
    }
    var defaultModelKey: String {
        didSet { UserDefaults.standard.set(defaultModelKey, forKey: "chat.defaultModel") }
    }
    var systemPrompt: String {
        didSet { UserDefaults.standard.set(systemPrompt, forKey: "chat.system") }
    }
    var theme: ThemeChoice {
        didSet { UserDefaults.standard.set(theme.rawValue, forKey: "ui.theme") }
    }

    var tab: AppTab = .home
    var user: User?
    var signedIn = false
    var status: ConnectionStatus = .unknown
    var tunnelStats = TunnelService.Stats.idle
    var captcha: TunnelService.Captcha?
    var connections: [Connection] = []
    var models: [AIModel] = []
    var modelErrors: [ProviderError] = []

    let api: APIClient
    let tunnel = TunnelService()
    let deviceID: String
    private var pollTask: Task<Void, Never>?

    var serverURL: URL? {
        let s = serverURLString.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let u = URL(string: s), u.scheme == "https" || u.scheme == "http", u.host() != nil else { return nil }
        return u
    }

    var isConfigured: Bool { serverURL != nil }

    init() {
        let d = UserDefaults.standard
        serverURLString = d.string(forKey: "server.url") ?? ""
        routeMode = RouteMode(rawValue: d.string(forKey: "route.mode") ?? "") ?? .auto
        defaultModelKey = d.string(forKey: "chat.defaultModel") ?? ""
        systemPrompt = d.string(forKey: "chat.system") ?? ""
        theme = ThemeChoice(rawValue: d.string(forKey: "ui.theme") ?? "") ?? .system
        if let id = d.string(forKey: "device.id") {
            deviceID = id
        } else {
            let id = UUID().uuidString
            d.set(id, forKey: "device.id")
            deviceID = id
        }
        let tokens = KeychainTokenStore()
        api = APIClient(baseURL: nil, tokens: tokens, tunnel: tunnel, socksPort: TunnelService.socksPort)
        signedIn = tokens.load() != nil
        let url = serverURL
        let mode = routeMode
        Task { [api] in
            await api.configure(baseURL: url, mode: mode)
            await api.setCallbacks(
                onRouteChange: { [weak self] r in Task { @MainActor in self?.routeChanged(r) } },
                onSignedOut: { [weak self] in Task { @MainActor in self?.didSignOut() } })
        }
    }

    private func reconfigure() {
        let url = serverURL
        let mode = routeMode
        Task { [api] in await api.configure(baseURL: url, mode: mode) }
    }

    var deviceName: String { UIDevice.current.name }

    // MARK: lifecycle

    func becameActive() {
        Task { [api] in await api.resetRoute() }
        startPolling()
        if signedIn { Task { await refreshCatalog() } }
    }

    /// The core runs in the app's process: stop it in the background and
    /// reconnect when the app is back.
    func wentToBackground() {
        pollTask?.cancel()
        pollTask = nil
        Task { [tunnel] in await tunnel.stop() }
    }

    private func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.poll()
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }

    private func poll() async {
        let stats = await tunnel.stats()
        let pending = await tunnel.captcha()
        let route = await api.lastRoute
        tunnelStats = stats
        captcha = pending
        if pending != nil {
            status = .needsCheck
        } else if route == .tunnel {
            status = stats.connected ? .tunnel(transport: stats.transport) : .connecting
        } else if route == .direct {
            status = .direct
        } else if stats.running && !stats.connected {
            status = .connecting
        }
    }

    private func routeChanged(_ r: Route) {
        switch r {
        case .direct: status = .direct
        case .tunnel: status = tunnelStats.connected ? .tunnel(transport: tunnelStats.transport) : .connecting
        }
    }

    // MARK: account

    func didSignIn(_ resp: AuthResponse) async {
        await api.setTokens(resp.tokens)
        user = resp.user
        signedIn = true
        await refreshCatalog()
    }

    func signOut() async {
        try? await api.sendNoContent(APIRequest("POST", "v1/auth/logout"))
        await api.setTokens(nil)
        didSignOut()
    }

    private func didSignOut() {
        signedIn = false
        user = nil
        connections = []
        models = []
    }

    func loadMe() async {
        if let me = try? await api.send(APIRequest("GET", "v1/me"), MeResponse.self) { user = me.user }
    }

    // MARK: providers

    func refreshCatalog() async {
        if let c = try? await api.send(APIRequest("GET", "v1/connections"), ConnectionsResponse.self) {
            connections = c.connections
        }
        if let m = try? await api.send(APIRequest("GET", "v1/models"), ModelsResponse.self) {
            models = m.models
            modelErrors = m.errors
        }
        if user == nil { await loadMe() }
    }

    func models(of provider: String) -> [AIModel] { models.filter { $0.provider == provider } }

    var defaultModel: AIModel? {
        models.first { $0.key == defaultModelKey } ?? models.first
    }

    func model(provider: String, id: String) -> AIModel {
        models.first { $0.provider == provider && $0.id == id } ?? AIModel(provider: provider, id: id, displayName: id)
    }

    // MARK: status text

    var statusText: LocalizedStringKey {
        switch status {
        case .unknown: "Проверка связи…"
        case .direct: "Напрямую"
        case .tunnel(let t): "Через \(Self.transportName(t))"
        case .connecting: "Подключение…"
        case .needsCheck: "Нужна проверка"
        case .offline: "Нет связи"
        }
    }

    var statusSymbol: String {
        switch status {
        case .unknown, .connecting: "arrow.triangle.2.circlepath"
        case .direct: "bolt.horizontal.circle.fill"
        case .tunnel: "lock.shield.fill"
        case .needsCheck: "exclamationmark.shield.fill"
        case .offline: "wifi.slash"
        }
    }

    static func transportName(_ t: String) -> String {
        switch t {
        case "vyandex", "yandex": String(localized: "Яндекс Документ")
        case "mailru": String(localized: "Mail.ru")
        case "cupsonline": String(localized: "cups.online")
        case "direct": String(localized: "прямой канал")
        case "": String(localized: "туннель")
        default: t
        }
    }
}

struct ConnectionsResponse: Decodable, Sendable {
    var connections: [Connection]
}
