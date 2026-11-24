import SwiftUI
import WebKit

/// A carrier asks for a check (Yandex SmartCaptcha or a login): the page
/// opens here, through the exit's proxy when it is the exit's check, and
/// after it is passed its cookies go back to the core.
struct CaptchaView: View {
    @Environment(AppModel.self) private var app
    @Environment(\.dismiss) private var dismiss
    var captcha: TunnelService.Captcha
    @State private var state = WebViewState()
    @State private var message: String?

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                if let message {
                    Text(message).font(.footnote).padding(8)
                }
                ProxiedWebView(url: captcha.url, storeID: nil, route: route, state: state)
            }
            .navigationTitle(captcha.reason == "login" ? "Вход для транспорта" : "Проверка")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Отмена") {
                        Task { await app.tunnel.cancelCaptcha() }
                        dismiss()
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Готово") { Task { await submit() } }
                }
            }
        }
    }

    private var route: WebRoute {
        if let p = captcha.remoteProxy, let r = WebRoute.http(p) { return r }
        return .direct
    }

    /// Cookies of the check's site as a Cookie header.
    private func submit() async {
        guard let store = state.webView?.configuration.websiteDataStore.httpCookieStore else { return }
        let host = captcha.url.host() ?? ""
        let cookies = await store.allCookies().filter { c in
            let d = c.domain.hasPrefix(".") ? String(c.domain.dropFirst()) : c.domain
            return host == d || host.hasSuffix("." + d) || d.hasSuffix("." + host)
        }
        let header = cookies.map { "\($0.name)=\($0.value)" }.joined(separator: "; ")
        if await app.tunnel.submitCaptcha(cookieHeader: header) {
            dismiss()
        } else {
            message = String(localized: "Проверка ещё не пройдена — завершите её на странице и нажмите «Готово».")
        }
    }
}
