import AIHubKit
import PhotosUI
import SwiftUI
import UIKit

/// One bubble in the chat.
struct ChatItem: Identifiable, Equatable {
    let id = UUID()
    var role: String
    var text: String
    var localImages: [UIImage] = []
    var remoteImages: [AttachmentInfo] = []
    var streaming = false
    var stopped = false
    var error: String?
}

/// A photo picked for the next message: downscaled JPEG, ready to send.
struct PendingImage: Identifiable {
    let id = UUID()
    var preview: UIImage
    var jpeg: Data

    /// Longest side 2048 px, JPEG 0.8: what a vision model needs, and small
    /// enough for the slow tunnel carriers.
    static func make(from data: Data) -> PendingImage? {
        guard let image = UIImage(data: data) else { return nil }
        let maxSide: CGFloat = 2048
        let size = image.size
        let scale = min(1, maxSide / max(size.width, size.height))
        let target = CGSize(width: (size.width * scale).rounded(), height: (size.height * scale).rounded())
        let format = UIGraphicsImageRendererFormat.default()
        format.scale = 1
        let resized = UIGraphicsImageRenderer(size: target, format: format).image { _ in
            image.draw(in: CGRect(origin: .zero, size: target))
        }
        guard let jpeg = resized.jpegData(compressionQuality: 0.8) else { return nil }
        return PendingImage(preview: resized, jpeg: jpeg)
    }
}

struct ChatView: View {
    @Environment(AppModel.self) private var app
    @State private var conversationID: Int64?
    @State private var current: AIModel
    @State private var items: [ChatItem] = []
    @State private var input = ""
    @State private var pending: [PendingImage] = []
    @State private var photoItems: [PhotosPickerItem] = []
    @State private var streamTask: Task<Void, Never>?
    @State private var loadError: String?
    @FocusState private var inputFocused: Bool

    init(conversationID: Int64?, initialModel: AIModel) {
        _conversationID = State(initialValue: conversationID)
        _current = State(initialValue: initialModel)
    }

    private var isStreaming: Bool { streamTask != nil }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(spacing: 12) {
                    if let loadError {
                        Text(loadError).foregroundStyle(.red).font(.footnote)
                    }
                    ForEach(items) { item in
                        MessageBubble(item: item, onRetry: item.id == items.last?.id ? retry : nil)
                            .id(item.id)
                    }
                    Color.clear.frame(height: 1).id("bottom")
                }
                .padding()
            }
            .defaultScrollAnchor(.bottom)
            .scrollDismissesKeyboard(.interactively)
            .onChange(of: items.last?.text) { _, _ in
                proxy.scrollTo("bottom", anchor: .bottom)
            }
            .safeAreaInset(edge: .bottom) { composer }
        }
        .navigationTitle(current.displayName)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) { modelMenu }
        }
        .task { await loadHistory() }
        .onChange(of: photoItems) { _, picked in
            Task { await addPhotos(picked) }
        }
        .onDisappear { streamTask?.cancel() }
    }

    // MARK: subviews

    private var modelMenu: some View {
        Menu {
            ForEach(app.connections) { c in
                let ms = app.models(of: c.provider)
                if !ms.isEmpty {
                    Section(c.name) {
                        ForEach(ms) { m in
                            Button {
                                current = m
                            } label: {
                                if m.key == current.key { Label(m.displayName, systemImage: "checkmark") } else { Text(m.displayName) }
                            }
                        }
                    }
                }
            }
        } label: {
            Image(systemName: "cpu")
        }
        .accessibilityLabel(Text("Выбрать модель"))
    }

    private var composer: some View {
        VStack(spacing: 8) {
            if !pending.isEmpty {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack {
                        ForEach(pending) { p in
                            Image(uiImage: p.preview)
                                .resizable().scaledToFill()
                                .frame(width: 56, height: 56)
                                .clipShape(.rect(cornerRadius: 10))
                                .overlay(alignment: .topTrailing) {
                                    Button {
                                        pending.removeAll { $0.id == p.id }
                                    } label: {
                                        Image(systemName: "xmark.circle.fill").symbolRenderingMode(.hierarchical)
                                    }
                                    .accessibilityLabel(Text("Убрать изображение"))
                                }
                        }
                    }
                    .padding(.horizontal)
                }
            }
            HStack(alignment: .bottom, spacing: 8) {
                PhotosPicker(selection: $photoItems, maxSelectionCount: 4, matching: .images) {
                    Image(systemName: "photo.badge.plus")
                        .font(.title3)
                        .frame(width: 40, height: 40)
                }
                .disabled(current.vision == false)
                .accessibilityLabel(Text("Прикрепить фото"))

                TextField("Сообщение", text: $input, axis: .vertical)
                    .lineLimit(1...6)
                    .focused($inputFocused)
                    .padding(.horizontal, 12)
                    .padding(.vertical, 9)

                Button {
                    if isStreaming { stop() } else { send() }
                } label: {
                    Image(systemName: isStreaming ? "stop.fill" : "arrow.up")
                        .font(.headline)
                        .frame(width: 40, height: 40)
                }
                .buttonStyle(.glassProminent)
                .buttonBorderShape(.circle)
                .disabled(!isStreaming && input.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && pending.isEmpty)
                .accessibilityLabel(Text(isStreaming ? "Остановить" : "Отправить"))
            }
            .padding(6)
            .glassEffect(.regular, in: .rect(cornerRadius: 26))
            .padding(.horizontal)
            .padding(.bottom, 6)
        }
    }

    // MARK: actions

    private func loadHistory() async {
        guard let id = conversationID, items.isEmpty else { return }
        do {
            let d = try await app.api.send(APIRequest("GET", "v1/conversations/\(id)"), ConversationDetail.self)
            items = d.messages.map {
                ChatItem(role: $0.role, text: $0.content, remoteImages: $0.attachments ?? [],
                         stopped: $0.status == "stopped", error: $0.status == "error" ? String(localized: "Ответ прервался с ошибкой") : nil)
            }
            current = app.model(provider: d.conversation.provider, id: d.conversation.model)
        } catch {
            loadError = error.localizedDescription
        }
    }

    private func addPhotos(_ picked: [PhotosPickerItem]) async {
        for item in picked {
            if let data = try? await item.loadTransferable(type: Data.self), let img = PendingImage.make(from: data) {
                pending.append(img)
            }
        }
        photoItems = []
    }

    private func send() {
        let text = input.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty || !pending.isEmpty else { return }
        let images = pending
        input = ""
        pending = []
        run(text: text, images: images)
    }

    private func run(text: String, images: [PendingImage]) {
        items.append(ChatItem(role: "user", text: text, localImages: images.map(\.preview)))
        items.append(ChatItem(role: "assistant", text: "", streaming: true))
        let attachments = images.map { AttachmentInput(mediaType: "image/jpeg", data: $0.jpeg.base64EncodedString()) }
        let system = app.systemPrompt.trimmingCharacters(in: .whitespacesAndNewlines)
        let req = ChatRequest(conversationId: conversationID, provider: current.provider, model: current.id,
                              messages: [ChatMessageInput(role: "user", content: text, attachments: attachments.isEmpty ? nil : attachments)],
                              system: conversationID == nil && !system.isEmpty ? system : nil)
        let api = app.api
        streamTask = Task {
            do {
                let stream = await api.chat(req)
                for try await ev in stream {
                    switch ev {
                    case .start(let cid, _): conversationID = cid
                    case .delta(let t): updateLast { $0.text += t }
                    case .usage: break
                    case .done(_, _, let stopped): updateLast { $0.streaming = false; $0.stopped = stopped }
                    case .error(_, let message): updateLast { $0.streaming = false; $0.error = message }
                    }
                }
                updateLast { $0.streaming = false }
            } catch is CancellationError {
                updateLast { $0.streaming = false; $0.stopped = true }
            } catch {
                updateLast { $0.streaming = false; $0.error = error.localizedDescription }
            }
            streamTask = nil
        }
    }

    private func updateLast(_ f: (inout ChatItem) -> Void) {
        guard !items.isEmpty else { return }
        f(&items[items.count - 1])
    }

    private func stop() {
        streamTask?.cancel()
        streamTask = nil
        updateLast { $0.streaming = false; $0.stopped = true }
    }

    /// Asks again: the last user message is sent once more.
    private func retry() {
        guard !isStreaming, let lastUser = items.last(where: { $0.role == "user" }) else { return }
        if items.last?.role == "assistant" { items.removeLast() }
        if items.last?.role == "user" { items.removeLast() }
        let images = lastUser.localImages.compactMap { img in img.jpegData(compressionQuality: 0.8).map { PendingImage(preview: img, jpeg: $0) } }
        run(text: lastUser.text, images: images)
    }
}

struct MessageBubble: View {
    var item: ChatItem
    var onRetry: (() -> Void)?

    var body: some View {
        let mine = item.role == "user"
        VStack(alignment: mine ? .trailing : .leading, spacing: 6) {
            if !item.localImages.isEmpty || !item.remoteImages.isEmpty {
                HStack {
                    ForEach(Array(item.localImages.enumerated()), id: \.offset) { _, img in
                        Image(uiImage: img).resizable().scaledToFill().frame(width: 120, height: 120).clipShape(.rect(cornerRadius: 12))
                    }
                    ForEach(item.remoteImages) { a in AuthorizedImage(attachment: a) }
                }
            }
            if mine {
                if !item.text.isEmpty {
                    Text(item.text)
                        .padding(12)
                        .background(.tint.opacity(0.18), in: .rect(cornerRadius: 18))
                        .textSelection(.enabled)
                }
            } else {
                if item.text.isEmpty && item.streaming {
                    ProgressView().padding(8)
                } else {
                    MarkdownView(text: item.text)
                }
                if let error = item.error {
                    Label(error, systemImage: "exclamationmark.triangle").font(.footnote).foregroundStyle(.red)
                }
                if item.stopped {
                    Text("Остановлено").font(.caption).foregroundStyle(.secondary)
                }
                if !item.streaming, let onRetry, item.error != nil || item.stopped || !item.text.isEmpty {
                    HStack(spacing: 16) {
                        Button("Повторить", systemImage: "arrow.clockwise", action: onRetry)
                        Button("Копировать", systemImage: "doc.on.doc") { UIPasteboard.general.string = item.text }
                    }
                    .font(.caption)
                    .labelStyle(.iconOnly)
                    .buttonStyle(.borderless)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: mine ? .trailing : .leading)
    }
}

/// Image of a stored message: fetched with the access token through the
/// API client (direct or tunnel).
struct AuthorizedImage: View {
    @Environment(AppModel.self) private var app
    var attachment: AttachmentInfo
    @State private var image: UIImage?

    var body: some View {
        Group {
            if let image {
                Image(uiImage: image).resizable().scaledToFill()
            } else {
                Rectangle().fill(.quaternary).overlay(ProgressView())
            }
        }
        .frame(width: 120, height: 120)
        .clipShape(.rect(cornerRadius: 12))
        .task {
            let path = attachment.url.hasPrefix("/") ? String(attachment.url.dropFirst()) : attachment.url
            if let data = try? await app.api.raw(APIRequest("GET", path)) { image = UIImage(data: data) }
        }
        .accessibilityLabel(Text("Изображение"))
    }
}
