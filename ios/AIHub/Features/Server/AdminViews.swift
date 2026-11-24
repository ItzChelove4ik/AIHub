import AIHubKit
import SwiftUI
import UIKit

private struct CarriersResponse: Decodable, Sendable { var carriers: [Carrier] }

/// Admin: the pool of Yandex and Mail.ru documents channels get.
struct CarrierPoolView: View {
    @Environment(AppModel.self) private var app
    @State private var carriers: [Carrier] = []
    @State private var type = "vyandex"
    @State private var url = ""
    @State private var error: String?

    var body: some View {
        Form {
            Section {
                Picker("Тип", selection: $type) {
                    Text("Яндекс Документ").tag("vyandex")
                    Text("Mail.ru").tag("mailru")
                }
                TextField(type == "vyandex" ? "https://docs.yandex.ru/edit/d/…" : "https://cloud.mail.ru/public/…/…", text: $url)
                    .textInputAutocapitalization(.never)
                    .autocorrectionDisabled()
                    .font(.footnote.monospaced())
                Button("Добавить в пул") { Task { await add() } }.disabled(url.isEmpty)
            } footer: {
                Text("Комнаты cups.online создаются автоматически для каждого канала.")
            }
            if let error { Section { Text(error).foregroundStyle(.red) } }
            Section("Носители") {
                ForEach(carriers) { c in
                    HStack {
                        VStack(alignment: .leading) {
                            Text("\(AppModel.transportName(c.type)) \(c.hint)")
                            Text(statusTitle(c.status)).font(.caption).foregroundStyle(c.status == "broken" ? .red : .secondary)
                        }
                        Spacer()
                    }
                    .swipeActions {
                        if c.status != "assigned" {
                            Button("Удалить", role: .destructive) { Task { await delete(c) } }
                        }
                        Button(c.status == "broken" ? "Исправен" : "Сломан") {
                            Task { await mark(c, c.status == "broken" ? "free" : "broken") }
                        }
                        .tint(.orange)
                    }
                }
            }
        }
        .navigationTitle("Пул документов")
        .task { await load() }
        .refreshable { await load() }
    }

    private func statusTitle(_ s: String) -> String {
        switch s {
        case "free": String(localized: "свободен")
        case "assigned": String(localized: "выдан")
        case "broken": String(localized: "сломан")
        default: s
        }
    }

    private func load() async {
        do {
            carriers = try await app.api.send(APIRequest("GET", "v1/admin/tunnel/carriers"), CarriersResponse.self).carriers
        } catch { self.error = error.localizedDescription }
    }

    private struct AddBody: Encodable { var type: String; var url: String }
    private struct StatusBody: Encodable { var status: String }

    private func add() async {
        do {
            _ = try await app.api.send(APIRequest.json("POST", "v1/admin/tunnel/carriers", AddBody(type: type, url: url)), Carrier.self)
            url = ""
            error = nil
            await load()
        } catch { self.error = error.localizedDescription }
    }

    private func delete(_ c: Carrier) async {
        do { try await app.api.sendNoContent(APIRequest("DELETE", "v1/admin/tunnel/carriers/\(c.id)")); await load() } catch { self.error = error.localizedDescription }
    }

    private func mark(_ c: Carrier, _ status: String) async {
        do {
            try await app.api.sendNoContent(APIRequest.json("PATCH", "v1/admin/tunnel/carriers/\(c.id)", StatusBody(status: status)))
            await load()
        } catch { self.error = error.localizedDescription }
    }
}

/// Admin: every channel on the server and its state.
struct AllChannelsView: View {
    @Environment(AppModel.self) private var app
    @State private var channels: [Channel] = []
    @State private var error: String?

    var body: some View {
        List {
            if let error { Text(error).foregroundStyle(.red) }
            ForEach(channels) { c in
                VStack(alignment: .leading) {
                    Text(c.channel).font(.body.monospaced())
                    Text("\(c.invite ? String(localized: "приглашение") : "user \(c.userId.map(String.init) ?? "—")") · порт \(c.port) · \(c.state)")
                        .font(.caption).foregroundStyle(.secondary)
                }
                .swipeActions {
                    Button("Удалить", role: .destructive) {
                        Task {
                            try? await app.api.sendNoContent(APIRequest("DELETE", "v1/tunnel/channels/\(c.id)"))
                            await load()
                        }
                    }
                }
            }
        }
        .navigationTitle("Все каналы")
        .task { await load() }
        .refreshable { await load() }
    }

    private func load() async {
        do {
            channels = try await app.api.send(APIRequest("GET", "v1/admin/tunnel/channels"), ChannelsResponse.self).channels
        } catch { self.error = error.localizedDescription }
    }
}

/// Admin: a channel for someone without an account yet. They import the
/// link (it gets them through the whitelist) and register with the code.
struct InviteView: View {
    @Environment(AppModel.self) private var app
    @State private var issued: IssuedChannel?
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        Form {
            Section {
                Button("Создать приглашение") { Task { await create() } }.disabled(busy)
            } footer: {
                Text("Ссылку и код покажите один раз и передайте лично: в ссылке ключ канала.")
            }
            if let error { Section { Text(error).foregroundStyle(.red) } }
            if let issued {
                Section("Приглашение") {
                    if let img = UIImage(data: issued.qrPng) {
                        Image(uiImage: img).interpolation(.none).resizable().scaledToFit().frame(maxWidth: 240)
                            .frame(maxWidth: .infinity)
                    }
                    if let code = issued.inviteCode {
                        LabeledContent("Код", value: code).textSelection(.enabled)
                        Button("Копировать код", systemImage: "doc.on.doc") { UIPasteboard.general.string = code }
                    }
                    Button("Копировать ссылку", systemImage: "link") { UIPasteboard.general.string = issued.link }
                }
            }
        }
        .navigationTitle("Приглашение")
    }

    private func create() async {
        busy = true
        defer { busy = false }
        do {
            issued = try await app.api.send(APIRequest("POST", "v1/admin/tunnel/invite"), IssuedChannel.self)
            error = nil
        } catch { self.error = error.localizedDescription }
    }
}
