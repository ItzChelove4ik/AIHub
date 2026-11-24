import AIHubKit
import SwiftUI
import UIKit

/// A provider's own website inside AIHub (see docs/web-mode.md). The app
/// does not read the site's cookies or script the page: the session and
/// the chats stay on this iPhone. Photos go through the site's own upload
/// button (camera or library).
struct WebSite: Identifiable, Hashable {
    let id: String
    let title: String
    let url: URL
    /// Fixed data-store id: the sign-in persists, sites stay separate.
    let storeID: UUID
    var host: String { url.host() ?? "" }

    static let all: [WebSite] = [
        WebSite(id: "chatgpt", title: "ChatGPT", url: URL(string: "https://chatgpt.com/")!,
                storeID: UUID(uuidString: "5B0C8B1E-6F2A-4C55-9C3E-6A0F0B1C0A01")!),
        WebSite(id: "claude", title: "Claude", url: URL(string: "https://claude.ai/")!,
                storeID: UUID(uuidString: "5B0C8B1E-6F2A-4C55-9C3E-6A0F0B1C0A02")!),
        WebSite(id: "gemini", title: "Gemini", url: URL(string: "https://gemini.google.com/")!,
                storeID: UUID(uuidString: "5B0C8B1E-6F2A-4C55-9C3E-6A0F0B1C0A03")!),
    ]
}

struct WebModeView: View {
    @Environment(AppModel.self) private var app
    var site: WebSite
    @State private var state = WebViewState()
    @State private var route: WebRoute?

    var body: some View {
        Group {
            if let route {
                ProxiedWebView(url: site.url, storeID: site.storeID, route: route, state: state)
                    .ignoresSafeArea(edges: .bottom)
            } else {
                ProgressView("Подключение…")
            }
        }
        .navigationTitle(site.title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItemGroup(placement: .topBarTrailing) {
                Button("Назад", systemImage: "chevron.backward") { state.back() }.disabled(!state.canGoBack)
                Button("Обновить", systemImage: "arrow.clockwise") { state.reload() }
                Menu {
                    Button("Открыть в Safari", systemImage: "safari") {
                        UIApplication.shared.open(state.url ?? site.url)
                    }
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
            }
        }
        .overlay(alignment: .top) {
            if state.loading { ProgressView(value: state.progress).progressViewStyle(.linear) }
        }
        .task { route = await chooseRoute() }
    }

    /// Through the tunnel when the app's requests go through it (whitelists),
    /// else directly.
    private func chooseRoute() async -> WebRoute {
        let mode = app.routeMode
        let last = await app.api.lastRoute
        if mode == .tunnelOnly || (mode == .auto && last == .tunnel) {
            if await app.tunnel.ensureConnected() { return .socks(port: TunnelService.socksPort) }
        }
        return .direct
    }
}
