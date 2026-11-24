import AIHubKit
import SwiftUI
import UIKit

/// First launch: the server address and, for networks with whitelists,
/// the channel link (openflux://) to reach it through.
struct OnboardingView: View {
    @Environment(AppModel.self) private var model
    @State private var address = ""
    @State private var link = ""
    @State private var scanning = false
    @State private var message: String?
    @State private var busy = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("https://сервер:порт", text: $address)
                        .keyboardType(.URL)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                } header: {
                    Text("Адрес сервера AIHub")
                } footer: {
                    Text("Тот, что выдал install.sh, например https://1-2-3-4.sslip.io:8443")
                }

                Section {
                    TextField("openflux://v1/…", text: $link, axis: .vertical)
                        .lineLimit(1...4)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .font(.footnote.monospaced())
                    HStack {
                        Button("Вставить", systemImage: "doc.on.clipboard") {
                            link = UIPasteboard.general.string ?? ""
                        }
                        Spacer()
                        Button("Сканировать QR", systemImage: "qrcode.viewfinder") { scanning = true }
                    }
                    .buttonStyle(.borderless)
                } header: {
                    Text("Ссылка канала (необязательно)")
                } footer: {
                    Text("Нужна, если мобильный интернет пускает только по «белым спискам». Ссылку даёт администратор сервера; в ней ключ канала, она хранится в Связке ключей.")
                }

                if let message {
                    Section { Text(message).foregroundStyle(.red) }
                }

                Section {
                    Button {
                        Task { await connect() }
                    } label: {
                        HStack {
                            Text("Продолжить")
                            if busy { Spacer(); ProgressView() }
                        }
                    }
                    .disabled(busy || address.isEmpty)
                }
            }
            .navigationTitle("AIHub")
            .sheet(isPresented: $scanning) {
                QRScannerSheet { code in
                    link = code
                    scanning = false
                }
            }
        }
    }

    private func connect() async {
        busy = true
        defer { busy = false }
        message = nil
        var a = address.trimmingCharacters(in: .whitespacesAndNewlines)
        if !a.contains("://") { a = "https://" + a }
        guard let url = URL(string: a), url.host() != nil else {
            message = String(localized: "Неверный адрес")
            return
        }
        if !link.isEmpty {
            do { _ = try await model.tunnel.importLink(link) } catch {
                message = error.localizedDescription
                return
            }
        }
        await model.api.configure(baseURL: url, mode: model.routeMode)
        do {
            let ready = try await model.api.send(APIRequest("GET", "readyz", auth: false), ReadyStatus.self)
            guard ready.status == "ready" else { throw APIError.unreachable(ready.status) }
            model.serverURLString = url.absoluteString
        } catch {
            message = error.localizedDescription
        }
    }
}
