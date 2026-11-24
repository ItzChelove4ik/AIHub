import AIHubKit
import SwiftUI
import UIKit

struct SettingsView: View {
    @Environment(AppModel.self) private var app
    @State private var addingKey = false
    @State private var openRouterURL: URL?
    @State private var error: String?

    var body: some View {
        @Bindable var app = app
        NavigationStack {
            Form {
                Section("Аккаунт") {
                    LabeledContent("Email", value: app.user?.email ?? "—")
                    if app.user?.isAdmin == true { LabeledContent("Роль", value: String(localized: "администратор")) }
                    NavigationLink("Устройства") { DevicesView() }
                    Button("Выйти", role: .destructive) { Task { await app.signOut() } }
                }

                Section {
                    ForEach(app.connections) { c in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(c.name)
                            Text(c.kind == "oauth" ? "OAuth · …\(c.last4)" : "API-ключ · …\(c.last4)")
                                .font(.caption.monospaced()).foregroundStyle(.secondary)
                        }
                    }
                    .onDelete { idx in
                        let ids = idx.map { app.connections[$0].id }
                        Task {
                            for id in ids { try? await app.api.sendNoContent(APIRequest("DELETE", "v1/connections/\(id)")) }
                            await app.refreshCatalog()
                        }
                    }
                    Button("Добавить API-ключ", systemImage: "key") { addingKey = true }
                    Button("Войти через OpenRouter", systemImage: "point.3.connected.trianglepath.dotted") {
                        Task { await startOpenRouter() }
                    }
                } header: {
                    Text("Подключения ИИ")
                } footer: {
                    Text("Ключи хранятся на вашем сервере в зашифрованном виде; приложение видит только последние 4 символа. Вход через подписки ChatGPT Plus / Claude Pro не поддерживается: провайдеры это запрещают.")
                }
                if let error { Section { Text(error).foregroundStyle(.red) } }

                Section("Чат") {
                    Picker("Модель по умолчанию", selection: $app.defaultModelKey) {
                        Text("Первая доступная").tag("")
                        ForEach(app.models) { m in Text("\(m.provider) · \(m.displayName)").tag(m.key) }
                    }
                    .pickerStyle(.navigationLink)
                    NavigationLink("Системный промт") { SystemPromptView() }
                }

                Section("API AIHub") {
                    NavigationLink("API-ключи AIHub") { APIKeysView() }
                }

                Section("Оформление") {
                    Picker("Тема", selection: $app.theme) {
                        ForEach(ThemeChoice.allCases) { t in Text(t.title).tag(t) }
                    }
                    Button("Язык приложения") {
                        if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                    }
                }

                Section {
                    NavigationLink("О приложении") { AboutView() }
                }
            }
            .navigationTitle("Настройки")
            .sheet(isPresented: $addingKey) { AddKeySheet() }
            .sheet(item: $openRouterURL) { url in OpenRouterSignInView(url: url) }
            .task { await app.refreshCatalog() }
        }
    }

    private struct StartResponse: Decodable, Sendable { var url: URL }

    private func startOpenRouter() async {
        do {
            openRouterURL = try await app.api.send(APIRequest("GET", "v1/oauth/openrouter/start"), StartResponse.self).url
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

extension URL: @retroactive Identifiable {
    public var id: String { absoluteString }
}

/// Provider → key → check (the server lists the models with it).
struct AddKeySheet: View {
    @Environment(AppModel.self) private var app
    @Environment(\.dismiss) private var dismiss
    @State private var specs: [ProviderSpec] = []
    @State private var provider = "openai"
    @State private var key = ""
    @State private var label = ""
    @State private var baseURL = ""
    @State private var busy = false
    @State private var error: String?

    private struct SpecsResponse: Decodable, Sendable { var providers: [ProviderSpec] }
    private struct Body: Encodable { var provider: String; var apiKey: String; var label: String?; var baseUrl: String? }

    var body: some View {
        NavigationStack {
            Form {
                Picker("Провайдер", selection: $provider) {
                    ForEach(specs) { s in Text(s.name).tag(s.id) }
                }
                SecureField("API-ключ", text: $key)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                if provider == "custom" {
                    TextField("Название (метка)", text: $label)
                    TextField("https://api.example.com/v1", text: $baseURL)
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                }
                if let error { Text(error).foregroundStyle(.red) }
                Button {
                    Task { await save() }
                } label: {
                    HStack {
                        Text("Проверить и сохранить")
                        if busy { Spacer(); ProgressView() }
                    }
                }
                .disabled(busy || key.isEmpty || (provider == "custom" && (label.isEmpty || baseURL.isEmpty)))
            }
            .navigationTitle("API-ключ")
            .toolbar { Button("Отмена") { dismiss() } }
            .task {
                if let r = try? await app.api.send(APIRequest("GET", "v1/providers"), SpecsResponse.self) {
                    specs = r.providers.sorted { $0.custom == true ? false : ($1.custom == true ? true : $0.name < $1.name) }
                }
            }
        }
    }

    private func save() async {
        busy = true
        defer { busy = false }
        let body = Body(provider: provider, apiKey: key.trimmingCharacters(in: .whitespacesAndNewlines),
                        label: provider == "custom" ? label : nil, baseUrl: provider == "custom" ? baseURL : nil)
        do {
            _ = try await app.api.send(APIRequest.json("POST", "v1/connections", body), Connection.self)
            key = ""
            await app.refreshCatalog()
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// OpenRouter's OAuth page in a web view (through the tunnel when needed);
/// the server finishes the PKCE exchange on its callback page.
struct OpenRouterSignInView: View {
    @Environment(AppModel.self) private var app
    @Environment(\.dismiss) private var dismiss
    var url: URL
    @State private var state = WebViewState()
    @State private var route: WebRoute?
    @State private var result: String?

    var body: some View {
        NavigationStack {
            Group {
                if let route {
                    ProxiedWebView(url: url, storeID: nil, route: route, state: state) { webView, current in
                        guard current.path().hasSuffix("/v1/oauth/openrouter/callback") else { return }
                        webView.evaluateJavaScript("document.getElementById('aihub-oauth')?.dataset.status") { value, _ in
                            MainActor.assumeIsolated {
                                result = value as? String
                                if result == "ok" {
                                    Task {
                                        await app.refreshCatalog()
                                        dismiss()
                                    }
                                }
                            }
                        }
                    }
                } else {
                    ProgressView()
                }
            }
            .navigationTitle("OpenRouter")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { Button("Закрыть") { dismiss() } }
        }
        .task {
            let last = await app.api.lastRoute
            if app.routeMode == .tunnelOnly || (app.routeMode == .auto && last == .tunnel), await app.tunnel.ensureConnected() {
                route = .socks(port: TunnelService.socksPort)
            } else {
                route = .direct
            }
        }
    }
}

struct DevicesView: View {
    @Environment(AppModel.self) private var app
    @State private var devices: [Device] = []
    @State private var error: String?

    private struct Response: Decodable, Sendable { var devices: [Device] }

    var body: some View {
        List {
            if let error { Text(error).foregroundStyle(.red) }
            ForEach(devices) { d in
                VStack(alignment: .leading) {
                    HStack {
                        Text(d.name.isEmpty ? d.deviceId : d.name)
                        if d.current { Text("это устройство").font(.caption).foregroundStyle(.tint) }
                    }
                    Text(d.revokedAt != nil ? String(localized: "отозвано") : "\(d.lastSeenAt.formatted(.relative(presentation: .named)))")
                        .font(.caption).foregroundStyle(.secondary)
                }
                .swipeActions {
                    if !d.current && d.revokedAt == nil {
                        Button("Отозвать", role: .destructive) {
                            Task {
                                try? await app.api.sendNoContent(APIRequest("DELETE", "v1/devices/\(d.id)"))
                                await load()
                            }
                        }
                    }
                }
            }
        }
        .navigationTitle("Устройства")
        .task { await load() }
    }

    private func load() async {
        do { devices = try await app.api.send(APIRequest("GET", "v1/devices"), Response.self).devices } catch { self.error = error.localizedDescription }
    }
}

struct SystemPromptView: View {
    @Environment(AppModel.self) private var app

    var body: some View {
        @Bindable var app = app
        Form {
            Section {
                TextEditor(text: $app.systemPrompt).frame(minHeight: 200)
            } footer: {
                Text("Добавляется к каждому новому чату.")
            }
        }
        .navigationTitle("Системный промт")
    }
}

/// AIHub's own keys for other programs (OpenAI-compatible API).
struct APIKeysView: View {
    @Environment(AppModel.self) private var app
    @State private var keys: [APIKeyInfo] = []
    @State private var name = ""
    @State private var created: String?
    @State private var error: String?

    private struct Response: Decodable, Sendable { var keys: [APIKeyInfo] }
    private struct Body: Encodable { var name: String }

    var body: some View {
        Form {
            Section {
                TextField("Название ключа", text: $name)
                Button("Создать ключ") { Task { await create() } }
            }
            if let created {
                Section {
                    Text(created).font(.footnote.monospaced()).textSelection(.enabled)
                    Button("Скопировать", systemImage: "doc.on.doc") { UIPasteboard.general.string = created }
                } header: {
                    Text("Новый ключ")
                } footer: {
                    Text("Показывается один раз: сохраните его сейчас.")
                }
            }
            if let error { Section { Text(error).foregroundStyle(.red) } }
            Section("Ключи") {
                ForEach(keys) { k in
                    VStack(alignment: .leading) {
                        Text(k.name.isEmpty ? k.prefix : k.name)
                        Text("\(k.prefix)… · \(k.scopes.joined(separator: ", "))\(k.revokedAt != nil ? " · отозван" : "")")
                            .font(.caption.monospaced()).foregroundStyle(.secondary)
                    }
                    .swipeActions {
                        if k.revokedAt == nil {
                            Button("Отозвать", role: .destructive) {
                                Task {
                                    try? await app.api.sendNoContent(APIRequest("DELETE", "v1/apikeys/\(k.id)"))
                                    await load()
                                }
                            }
                        }
                    }
                }
            }
            Section("Пример") {
                Text(curlExample).font(.caption2.monospaced()).textSelection(.enabled)
            }
        }
        .navigationTitle("API-ключи AIHub")
        .task { await load() }
    }

    private var curlExample: String {
        let base = app.serverURLString.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let model = app.defaultModel.map { "\($0.provider)/\($0.id)" } ?? "anthropic/<model>"
        return """
        curl \(base)/openai/v1/chat/completions \\
          -H "Authorization: Bearer ahk_live_…" \\
          -H "Content-Type: application/json" \\
          -d '{"model":"\(model)","messages":[{"role":"user","content":"Привет"}]}'
        """
    }

    private func load() async {
        do { keys = try await app.api.send(APIRequest("GET", "v1/apikeys"), Response.self).keys } catch { self.error = error.localizedDescription }
    }

    private func create() async {
        do {
            let r = try await app.api.send(APIRequest.json("POST", "v1/apikeys", Body(name: name)), CreatedAPIKey.self)
            created = r.key
            name = ""
            await load()
        } catch { self.error = error.localizedDescription }
    }
}

struct AboutView: View {
    var body: some View {
        Form {
            LabeledContent("Версия", value: "\(Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "") (\(Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? ""))")
            Section("Лицензии") {
                Text("AIHub распространяется под GNU GPL-3.0.")
                Text("Ядро туннеля — OpenFlux (форк meepo161/OpenFlux), GNU GPL-3.0.")
            }
            Section {
                Link("Исходники AIHub", destination: URL(string: "https://github.com/ItzChelove4ik/AIHub")!)
                Link("OpenFlux", destination: URL(string: "https://github.com/meepo161/OpenFlux")!)
            }
        }
        .navigationTitle("О приложении")
    }
}
