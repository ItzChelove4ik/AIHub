import AIHubKit
import SwiftUI

/// Sign in or create an AIHub account (email + password). "Sign in with
/// Apple" needs a paid developer account and is not offered.
struct AuthView: View {
    @Environment(AppModel.self) private var model
    @State private var registering = false
    @State private var email = ""
    @State private var password = ""
    @State private var invite = ""
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Picker("Режим", selection: $registering) {
                    Text("Вход").tag(false)
                    Text("Регистрация").tag(true)
                }
                .pickerStyle(.segmented)
                .listRowBackground(Color.clear)

                Section {
                    TextField("Email", text: $email)
                        .keyboardType(.emailAddress)
                        .textContentType(.username)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    SecureField("Пароль", text: $password)
                        .textContentType(registering ? .newPassword : .password)
                    if registering {
                        TextField("Код приглашения (если есть)", text: $invite)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                    }
                } footer: {
                    if registering {
                        Text("Пароль не короче 10 символов. Если сервер принимает только приглашённых, введите код от администратора.")
                    }
                }

                if let error {
                    Section { Text(error).foregroundStyle(.red) }
                }

                Section {
                    Button {
                        Task { await submit() }
                    } label: {
                        HStack {
                            Text(registering ? "Создать аккаунт" : "Войти")
                            if busy { Spacer(); ProgressView() }
                        }
                    }
                    .disabled(busy || email.isEmpty || password.isEmpty)
                }

                Section {
                    Button("Изменить адрес сервера", role: .destructive) {
                        model.serverURLString = ""
                    }
                } footer: {
                    Text(model.serverURLString)
                }
            }
            .navigationTitle(registering ? "Регистрация" : "Вход")
        }
    }

    private struct Body: Encodable {
        var email: String
        var password: String
        var deviceId: String
        var deviceName: String
        var inviteCode: String?
    }

    private func submit() async {
        busy = true
        defer { busy = false }
        error = nil
        let body = Body(email: email, password: password, deviceId: model.deviceID, deviceName: model.deviceName,
                        inviteCode: registering && !invite.isEmpty ? invite : nil)
        let path = registering ? "v1/auth/register" : "v1/auth/login"
        do {
            let resp = try await model.api.send(APIRequest.json("POST", path, body, auth: false), AuthResponse.self)
            password = ""
            await model.didSignIn(resp)
        } catch {
            self.error = error.localizedDescription
        }
    }
}
