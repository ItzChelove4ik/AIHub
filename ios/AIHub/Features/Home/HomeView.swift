import AIHubKit
import SwiftUI

/// Home: cards of the connected AIs, the providers' own websites (web
/// mode) and the chat history.
struct HomeView: View {
    @Environment(AppModel.self) private var model
    @State private var path = NavigationPath()

    private let columns = [GridItem(.adaptive(minimum: 150), spacing: 12)]

    var body: some View {
        NavigationStack(path: $path) {
            ScrollView {
                if model.connections.isEmpty {
                    emptyState
                } else {
                    VStack(alignment: .leading, spacing: 20) {
                        if let m = model.defaultModel {
                            NavigationLink(value: ChatRoute(conversationID: nil, model: m)) {
                                Label("Новый чат · \(m.displayName)", systemImage: "square.and.pencil")
                                    .font(.headline)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    .padding()
                                    .glassEffect(.regular.interactive(), in: .rect(cornerRadius: 20))
                            }
                            .buttonStyle(.plain)
                        }

                        Text("Подключённые ИИ").font(.title3.bold())
                        GlassEffectContainer(spacing: 12) {
                            LazyVGrid(columns: columns, spacing: 12) {
                                ForEach(model.connections) { c in
                                    NavigationLink(value: ProviderRoute(connection: c)) {
                                        ProviderCard(title: c.name, subtitle: "\(model.models(of: c.provider).count) моделей",
                                                     symbol: ProviderCard.symbol(for: c.type))
                                    }
                                    .buttonStyle(.plain)
                                }
                            }
                        }
                    }
                    .padding()
                }

                VStack(alignment: .leading, spacing: 12) {
                    Text("Сайты провайдеров").font(.title3.bold())
                    Text("Официальные сайты внутри AIHub: вход и переписка остаются на этом iPhone.")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                    LazyVGrid(columns: columns, spacing: 12) {
                        ForEach(WebSite.all) { site in
                            NavigationLink(value: site) {
                                ProviderCard(title: site.title, subtitle: site.host, symbol: "globe")
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
                .padding()
            }
            .navigationTitle("AIHub")
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    NavigationLink(value: HistoryRoute()) {
                        Image(systemName: "clock.arrow.circlepath")
                    }
                    .accessibilityLabel(Text("История чатов"))
                }
            }
            .refreshable { await model.refreshCatalog() }
            .navigationDestination(for: ProviderRoute.self) { r in ModelListView(connection: r.connection) }
            .navigationDestination(for: ChatRoute.self) { r in ChatView(conversationID: r.conversationID, initialModel: r.model) }
            .navigationDestination(for: HistoryRoute.self) { _ in ChatListView() }
            .navigationDestination(for: WebSite.self) { site in WebModeView(site: site) }
        }
        .task { await model.refreshCatalog() }
    }

    private var emptyState: some View {
        ContentUnavailableView {
            Label("Нет подключённых ИИ", systemImage: "sparkles")
        } description: {
            Text("Добавьте API-ключ OpenAI, Anthropic, Gemini или войдите через OpenRouter.")
        } actions: {
            Button("Подключить ИИ") { model.tab = .settings }
                .buttonStyle(.glassProminent)
        }
        .padding(.top, 40)
    }
}

struct ProviderRoute: Hashable { var connection: Connection }
struct ChatRoute: Hashable { var conversationID: Int64?; var model: AIModel }
struct HistoryRoute: Hashable {}

struct ProviderCard: View {
    var title: String
    var subtitle: String
    var symbol: String

    static func symbol(for type: String) -> String {
        switch type {
        case "openai": "circle.hexagongrid"
        case "anthropic": "asterisk"
        case "gemini": "sparkle"
        case "openrouter": "point.3.connected.trianglepath.dotted"
        case "custom": "server.rack"
        default: "brain"
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Image(systemName: symbol)
                .font(.title2)
                .foregroundStyle(.tint)
            Text(title).font(.headline).lineLimit(1)
            Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1)
        }
        .frame(maxWidth: .infinity, minHeight: 96, alignment: .leading)
        .padding()
        .glassEffect(.regular.interactive(), in: .rect(cornerRadius: 20))
        .accessibilityElement(children: .combine)
    }
}

/// The models of one provider; picking one starts a chat.
struct ModelListView: View {
    @Environment(AppModel.self) private var model
    var connection: Connection
    @State private var query = ""

    var body: some View {
        let all = model.models(of: connection.provider)
        let shown = query.isEmpty ? all : all.filter { $0.displayName.localizedCaseInsensitiveContains(query) || $0.id.localizedCaseInsensitiveContains(query) }
        List {
            if let err = model.modelErrors.first(where: { $0.provider == connection.provider }) {
                Text(err.error).foregroundStyle(.red).font(.footnote)
            }
            ForEach(shown) { m in
                NavigationLink(value: ChatRoute(conversationID: nil, model: m)) {
                    VStack(alignment: .leading, spacing: 2) {
                        HStack {
                            Text(m.displayName)
                            if m.vision == true {
                                Image(systemName: "photo").foregroundStyle(.secondary).accessibilityLabel(Text("принимает изображения"))
                            }
                        }
                        HStack {
                            Text(m.id).font(.caption.monospaced())
                            if let c = m.context { Text("· \(c / 1000)K").font(.caption) }
                        }
                        .foregroundStyle(.secondary)
                    }
                }
            }
        }
        .searchable(text: $query)
        .navigationTitle(connection.name)
        .overlay {
            if all.isEmpty { ContentUnavailableView("Нет моделей", systemImage: "tray", description: Text("Потяните вниз на главной, чтобы обновить список.")) }
        }
    }
}

/// Chat history: search, open, delete.
struct ChatListView: View {
    @Environment(AppModel.self) private var model
    @State private var items: [Conversation] = []
    @State private var query = ""
    @State private var error: String?

    var body: some View {
        List {
            if let error { Text(error).foregroundStyle(.red) }
            ForEach(items) { c in
                NavigationLink(value: ChatRoute(conversationID: c.id, model: model.model(provider: c.provider, id: c.model))) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(c.title.isEmpty ? String(localized: "Без названия") : c.title).lineLimit(1)
                        Text("\(c.model) · \(c.updatedAt.formatted(.relative(presentation: .named)))")
                            .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                }
            }
            .onDelete { idx in
                let ids = idx.map { items[$0].id }
                items.remove(atOffsets: idx)
                Task {
                    for id in ids { try? await model.api.sendNoContent(APIRequest("DELETE", "v1/conversations/\(id)")) }
                }
            }
        }
        .navigationTitle("История")
        .searchable(text: $query, prompt: Text("Поиск по чатам"))
        .task(id: query) {
            try? await Task.sleep(for: .milliseconds(300))
            await load()
        }
        .refreshable { await load() }
        .overlay {
            if items.isEmpty && error == nil { ContentUnavailableView("Чатов пока нет", systemImage: "bubble.left.and.bubble.right") }
        }
    }

    private func load() async {
        var q = [URLQueryItem(name: "limit", value: "100")]
        if !query.isEmpty { q.append(URLQueryItem(name: "q", value: query)) }
        do {
            items = try await model.api.send(APIRequest("GET", "v1/conversations", query: q), ConversationsResponse.self).conversations
            error = nil
        } catch is CancellationError {
        } catch {
            self.error = error.localizedDescription
        }
    }
}
