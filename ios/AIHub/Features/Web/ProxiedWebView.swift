import Network
import SwiftUI
import WebKit

/// How a web view reaches the internet: directly, through the in-app
/// tunnel's SOCKS5 proxy (works under whitelists), or through the exit's
/// HTTP proxy for its captcha.
enum WebRoute: Equatable {
    case direct
    case socks(port: UInt16)
    case httpProxy(host: String, port: UInt16)

    var proxyConfigurations: [ProxyConfiguration] {
        switch self {
        case .direct:
            return []
        case .socks(let port):
            return [ProxyConfiguration(socksv5Proxy: .hostPort(host: "127.0.0.1", port: NWEndpoint.Port(rawValue: port) ?? 1080))]
        case .httpProxy(let host, let port):
            return [ProxyConfiguration(httpCONNECTProxy: .hostPort(host: NWEndpoint.Host(host), port: NWEndpoint.Port(rawValue: port) ?? 80), tlsOptions: nil)]
        }
    }

    /// "127.0.0.1:12345" → .httpProxy
    static func http(_ hostPort: String) -> WebRoute? {
        guard let colon = hostPort.lastIndex(of: ":"), let port = UInt16(hostPort[hostPort.index(after: colon)...]) else { return nil }
        return .httpProxy(host: String(hostPort[..<colon]), port: port)
    }
}

/// Observable state of a web view for the surrounding SwiftUI.
@MainActor
@Observable
final class WebViewState {
    var title = ""
    var url: URL?
    var loading = false
    var canGoBack = false
    var progress = 0.0
    weak var webView: WKWebView?

    func back() { webView?.goBack() }
    func reload() { webView?.reload() }
}

/// WKWebView with a persistent per-site data store and a proxy route.
/// ASWebAuthenticationSession is not used: it would bypass the tunnel.
struct ProxiedWebView: UIViewRepresentable {
    var url: URL
    /// Fixed per site so sign-ins persist and sites do not share cookies;
    /// nil = an ephemeral store.
    var storeID: UUID?
    var route: WebRoute
    var state: WebViewState
    var onNavigate: ((WKWebView, URL) -> Void)?

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    func makeUIView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        let store = storeID.map { WKWebsiteDataStore(forIdentifier: $0) } ?? .nonPersistent()
        store.proxyConfigurations = route.proxyConfigurations
        config.websiteDataStore = store
        config.allowsInlineMediaPlayback = true
        let view = WKWebView(frame: .zero, configuration: config)
        view.navigationDelegate = context.coordinator
        view.uiDelegate = context.coordinator
        view.allowsBackForwardNavigationGestures = true
        context.coordinator.observe(view)
        state.webView = view
        view.load(URLRequest(url: url))
        return view
    }

    /// The route is fixed when the view is made; a new route means a new
    /// view (the callers give it a new identity).
    func updateUIView(_ view: WKWebView, context: Context) {
        context.coordinator.parent = self
    }

    @MainActor
    final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate {
        var parent: ProxiedWebView
        private var observations: [NSKeyValueObservation] = []

        init(_ parent: ProxiedWebView) { self.parent = parent }

        func observe(_ view: WKWebView) {
            let state = parent.state
            observations = [
                view.observe(\.estimatedProgress) { v, _ in MainActor.assumeIsolated { state.progress = v.estimatedProgress } },
                view.observe(\.isLoading) { v, _ in MainActor.assumeIsolated { state.loading = v.isLoading } },
                view.observe(\.canGoBack) { v, _ in MainActor.assumeIsolated { state.canGoBack = v.canGoBack } },
                view.observe(\.title) { v, _ in MainActor.assumeIsolated { state.title = v.title ?? "" } },
                view.observe(\.url) { v, _ in MainActor.assumeIsolated { state.url = v.url } },
            ]
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            if let url = webView.url { parent.onNavigate?(webView, url) }
        }

        /// target=_blank links open in the same view.
        func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                     for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
            if navigationAction.targetFrame == nil { webView.load(navigationAction.request) }
            return nil
        }
    }
}
