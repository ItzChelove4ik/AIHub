import AIHubKit
import SwiftUI

@main
struct AIHubApp: App {
    @State private var model = AppModel()
    @Environment(\.scenePhase) private var scenePhase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(model)
                .preferredColorScheme(model.theme.colorScheme)
        }
        .onChange(of: scenePhase) { _, phase in
            switch phase {
            case .active: model.becameActive()
            case .background: model.wentToBackground()
            default: break
            }
        }
    }
}

struct RootView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Group {
            if !model.isConfigured {
                OnboardingView()
            } else if !model.signedIn {
                AuthView()
            } else {
                MainTabs()
            }
        }
        .animation(.default, value: model.signedIn)
        .animation(.default, value: model.isConfigured)
    }
}

/// Root: the system TabView on the iOS 26 SDK gives the floating Liquid
/// Glass tab bar; it shrinks while a chat scrolls, and the connection
/// capsule sits above it.
struct MainTabs: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        @Bindable var model = model
        TabView(selection: $model.tab) {
            Tab("Главная", systemImage: "sparkles", value: AppTab.home) {
                HomeView()
            }
            Tab("Сервер", systemImage: "server.rack", value: AppTab.server) {
                ServerView()
            }
            Tab("Настройки", systemImage: "gearshape", value: AppTab.settings) {
                SettingsView()
            }
        }
        .tabBarMinimizeBehavior(.onScrollDown)
        .tabViewBottomAccessory {
            ConnectionPill()
        }
    }
}

/// "Напрямую / Через Яндекс Документ / Подключение… / Нужна проверка";
/// opens the Server tab.
struct ConnectionPill: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        Button {
            model.tab = .server
        } label: {
            HStack(spacing: 8) {
                Image(systemName: model.statusSymbol)
                    .symbolEffect(.pulse, isActive: model.status == .connecting || model.status == .unknown)
                    .foregroundStyle(model.status == .needsCheck ? .orange : .accentColor)
                Text(model.statusText)
                    .font(.subheadline.weight(.medium))
                    .lineLimit(1)
                Spacer(minLength: 0)
                if case .tunnel = model.status {
                    Text(ByteCountFormatter.string(fromByteCount: model.tunnelStats.bytesReceived, countStyle: .binary))
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, 16)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(Text("Соединение с сервером"))
        .accessibilityValue(Text(model.statusText))
        .accessibilityHint(Text("Открывает вкладку «Сервер»"))
    }
}
