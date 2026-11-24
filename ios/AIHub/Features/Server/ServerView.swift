import AIHubKit
import SwiftUI
import UIKit

/// The Server tab: how the app reaches the server and this device's
/// OpenFlux channel.
struct ServerView: View {
    @Environment(AppModel.self) private var app
    @State private var linkInfo: TunnelService.LinkInfo?
    @State private var channels: [Channel] = []
    @State private var tunnelEnabled = true
    @State private var issued: IssuedChannel?
    @State private var importText = ""
    @State private var scanning = false
    @State private var showCaptcha = false
    @State private var message: String?
    @State private var busy = false
    @State private var latency: Duration?
    @State private var isAdmin = false

    var body: some View {
        @Bindable var app = app
        NavigationStack {
            Form {
                statusSection

                Section {
                    Picker("Режим", selection: $app.routeMode) {
                        Text("Авто").tag(RouteMode.auto)
                        Text("Только напрямую").tag(RouteMode.directOnly)
                        Text("Только туннель").tag(RouteMode.tunnelOnly)
                    }
                    .pickerStyle(.segmented)
                } header: {
                    Text("Как подключаться")
                } footer: {
                    Text("«Авто»: сначала напрямую, а если не выходит (белые списки) — через туннель OpenFlux.")
                }

                channelSection
                importSection

                if let linkInfo, linkInfo.transports.count > 1 {
                    Section("Приоритет транспортов") {
                        NavigationLink("Порядок: \(linkInfo.transports.map(AppModel.transportName).joined(separator: " → "))") {
                            PriorityView(transports: linkInfo.transports)
                        }
                    }
                }

                Section("Дополнительно") {
                    NavigationLink("DNS-over-TLS") { DoTView() }
                    NavigationLink("Логи ядра") { CoreLogView() }
                    LabeledContent("Адрес сервера") {
                        Text(app.serverURLString).font(.footnote).lineLimit(1).truncationMode(.middle)
                    }
                    Button("Изменить адрес сервера", role: .destructive) { app.serverURLString = "" }
                }

                if isAdmin {
                    Section("Администрирование") {
                        NavigationLink("Пул документов") { CarrierPoolView() }
                        NavigationLink("Все каналы") { AllChannelsView() }
                        NavigationLink("Приглашение") { InviteView() }
                    }
                }

                if let message {
                    Section { Text(message).foregroundStyle(.secondary).font(.footnote) }
                }
            }
            .navigationTitle("Сервер")
            .refreshable { await reload() }
            .task { await reload() }
            .sheet(isPresented: $scanning) {
                QRScannerSheet { code in
                    importText = code
                    scanning = false
                    Task { await importLink() }
                }
            }
            .sheet(item: $issued) { iss in IssuedChannelSheet(issued: iss) }
            .sheet(isPresented: $showCaptcha) {
                if let c = app.captcha { CaptchaView(captcha: c) }
            }
        }
    }

    // MARK: sections

    private var statusSection: some View {
        Section("Состояние") {
            LabeledContent("Связь") {
                Label { Text(app.statusText) } icon: { Image(systemName: app.statusSymbol) }
            }
            LabeledContent("Туннель") {
                Text(app.tunnelStats.running ? (app.tunnelStats.connected ? String(localized: "подключён") : String(localized: "подключается")) : String(localized: "выключен"))
            }
            if app.tunnelStats.running {
                LabeledContent("Транспорт", value: AppModel.transportName(app.tunnelStats.transport))
                LabeledContent("Отправлено", value: ByteCountFormatter.string(fromByteCount: app.tunnelStats.bytesSent, countStyle: .binary))
                LabeledContent("Получено", value: ByteCountFormatter.string(fromByteCount: app.tunnelStats.bytesReceived, countStyle: .binary))
            }
            LabeledContent("Задержка") {
                if let latency {
                    Text(latency.formatted(.units(allowed: [.milliseconds], width: .narrow)))
                } else {
                    Text("—")
                }
            }
            if app.captcha != nil {
                Button("Пройти проверку", systemImage: "checkmark.shield") { showCaptcha = true }
                    .foregroundStyle(.orange)
            }
            HStack {
                Button(app.tunnelStats.running ? "Остановить туннель" : "Запустить туннель") {
                    Task { await toggleTunnel() }
                }
                .disabled(linkInfo == nil)
                Spacer()
                Button("Проверить") { Task { await measure() } }
            }
            .buttonStyle(.borderless)
        }
    }

    private var channelSection: some View {
        Section {
            if let linkInfo {
                LabeledContent("Канал на устройстве", value: linkInfo.name)
                Text(linkInfo.transports.map(AppModel.transportName).joined(separator: ", "))
                    .font(.footnote).foregroundStyle(.secondary)
            } else {
                Text("На этом iPhone нет канала").foregroundStyle(.secondary)
            }
            ForEach(channels) { c in
                VStack(alignment: .leading) {
                    Text(c.channel).font(.body.monospaced())
                    Text("\(c.transports.map(AppModel.transportName).joined(separator: ", ")) · \(c.state)")
                        .font(.caption).foregroundStyle(.secondary)
                }
                .swipeActions {
                    Button("Удалить", role: .destructive) { Task { await delete(c) } }
                    Button("Пересоздать") { Task { await rotate(c) } }.tint(.orange)
                }
            }
            if tunnelEnabled {
                Button(channels.isEmpty ? "Создать канал" : "Пересоздать канал", systemImage: "plus.circle") {
                    Task {
                        if channels.isEmpty { await create() } else { await rotate(channels[0]) }
                    }
                }
                .disabled(busy)
            } else {
                Text("Управление каналами на сервере выключено").font(.footnote).foregroundStyle(.secondary)
            }
        } header: {
            Text("Мой канал")
        } footer: {
            Text("Один канал — одно подключённое устройство. Ссылка с ключом показывается один раз и хранится в Связке ключей.")
        }
    }

    private var importSection: some View {
        Section("Импорт ссылки") {
            TextField("openflux://v1/…", text: $importText, axis: .vertical)
                .lineLimit(1...4)
                .font(.footnote.monospaced())
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            HStack {
                Button("Вставить", systemImage: "doc.on.clipboard") { importText = UIPasteboard.general.string ?? "" }
                Spacer()
                Button("QR", systemImage: "qrcode.viewfinder") { scanning = true }
                Spacer()
                Button("Импорт") { Task { await importLink() } }.disabled(importText.isEmpty)
            }
            .buttonStyle(.borderless)
            if linkInfo != nil {
                Button("Забыть канал на этом iPhone", role: .destructive) {
                    Task {
                        await app.tunnel.forgetLink()
                        linkInfo = nil
                    }
                }
            }
        }
    }

    // MARK: actions

    private func reload() async {
        linkInfo = await app.tunnel.linkInfo()
        isAdmin = app.user?.isAdmin ?? false
        do {
            let r = try await app.api.send(APIRequest("GET", "v1/tunnel/channels"), ChannelsResponse.self)
            channels = r.channels.filter { $0.deviceId != nil }
            tunnelEnabled = r.enabled
        } catch {
            message = error.localizedDescription
        }
        await measure()
    }

    private func measure() async {
        let clock = ContinuousClock()
        let start = clock.now
        if (try? await app.api.raw(APIRequest("GET", "healthz", auth: false))) != nil {
            latency = clock.now - start
        } else {
            latency = nil
        }
    }

    private func toggleTunnel() async {
        if app.tunnelStats.running {
            await app.tunnel.stop()
        } else {
            do { try await app.tunnel.start() } catch { message = error.localizedDescription }
        }
    }

    private func store(_ iss: IssuedChannel) async {
        do {
            linkInfo = try await app.tunnel.importLink(iss.link)
            issued = iss
        } catch {
            message = error.localizedDescription
        }
    }

    private func create() async {
        busy = true
        defer { busy = false }
        do {
            let iss = try await app.api.send(APIRequest("POST", "v1/tunnel/channels"), IssuedChannel.self)
            await store(iss)
            await reload()
        } catch {
            message = error.localizedDescription
        }
    }

    private func rotate(_ c: Channel) async {
        busy = true
        defer { busy = false }
        do {
            let iss = try await app.api.send(APIRequest("POST", "v1/tunnel/channels/\(c.id)/rotate"), IssuedChannel.self)
            await store(iss)
            await reload()
        } catch {
            message = error.localizedDescription
        }
    }

    private func delete(_ c: Channel) async {
        do {
            try await app.api.sendNoContent(APIRequest("DELETE", "v1/tunnel/channels/\(c.id)"))
            await app.tunnel.forgetLink()
            await reload()
        } catch {
            message = error.localizedDescription
        }
    }

    private func importLink() async {
        do {
            linkInfo = try await app.tunnel.importLink(importText)
            importText = ""
            message = String(localized: "Ссылка сохранена")
        } catch {
            message = error.localizedDescription
        }
    }
}

/// Shown once after a channel is issued: the link is already stored on this
/// iPhone; the QR lets another device of the same person import it.
struct IssuedChannelSheet: View {
    var issued: IssuedChannel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            VStack(spacing: 16) {
                if let img = UIImage(data: issued.qrPng) {
                    Image(uiImage: img).interpolation(.none).resizable().scaledToFit().frame(maxWidth: 260)
                        .accessibilityLabel(Text("QR-код ссылки канала"))
                }
                Text(issued.channel).font(.headline.monospaced())
                if let code = issued.inviteCode {
                    LabeledContent("Код приглашения", value: code).textSelection(.enabled)
                }
                Text("Ссылка уже сохранена на этом iPhone. В ней ключ канала: не пересылайте её чужим.")
                    .font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
                Button("Скопировать ссылку", systemImage: "doc.on.doc") { UIPasteboard.general.string = issued.link }
                    .buttonStyle(.glass)
            }
            .padding()
            .navigationTitle("Канал создан")
            .toolbar { Button("Готово") { dismiss() } }
        }
    }
}

/// The user's carrier order; the first is tried first.
struct PriorityView: View {
    @Environment(AppModel.self) private var app
    @State var transports: [String]

    var body: some View {
        List {
            ForEach(transports, id: \.self) { t in Text(AppModel.transportName(t)) }
                .onMove { transports.move(fromOffsets: $0, toOffset: $1) }
        }
        .environment(\.editMode, .constant(.active))
        .navigationTitle("Приоритет")
        .toolbar {
            Button("Сбросить") {
                Task { await app.tunnel.setPriorityOrder([]) }
            }
        }
        .onChange(of: transports) { _, order in
            Task { await app.tunnel.setPriorityOrder(order) }
        }
    }
}

struct DoTView: View {
    @Environment(AppModel.self) private var app
    @State private var spec = ""

    var body: some View {
        Form {
            Section {
                TextField("77.88.8.8@common.dot.dns.yandex.net", text: $spec)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .font(.footnote.monospaced())
                Button("Сохранить") { Task { await app.tunnel.setDoTResolver(spec) } }
            } footer: {
                Text("Серверы через «;», формат адрес[:порт]@SNI. Пусто — по умолчанию (Яндекс, Google, Cloudflare). Применяется при следующем запуске туннеля.")
            }
        }
        .navigationTitle("DNS-over-TLS")
        .task { spec = await app.tunnel.dotResolver }
    }
}

struct CoreLogView: View {
    @Environment(AppModel.self) private var app
    @State private var text = ""
    @State private var level = 0

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                Text(text.isEmpty ? String(localized: "Лог пуст") : text)
                    .font(.caption2.monospaced())
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
                Color.clear.frame(height: 1).id("end")
            }
            .onChange(of: text) { _, _ in proxy.scrollTo("end") }
        }
        .navigationTitle("Логи ядра")
        .toolbar {
            Menu {
                Picker("Подробность", selection: $level) {
                    Text("Обычно").tag(0)
                    Text("Отладка").tag(1)
                    Text("Подробно").tag(2)
                }
                Button("Копировать", systemImage: "doc.on.doc") { UIPasteboard.general.string = text }
                Button("Очистить", systemImage: "trash") { Task { await app.tunnel.clearLog(); text = "" } }
            } label: { Image(systemName: "ellipsis.circle") }
        }
        .onChange(of: level) { _, n in Task { await app.tunnel.setDebugLevel(n) } }
        .task {
            level = await app.tunnel.debugLevel
            while !Task.isCancelled {
                text = await app.tunnel.log()
                try? await Task.sleep(for: .seconds(1))
            }
        }
    }
}
