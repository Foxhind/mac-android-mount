import ServiceManagement
import SwiftUI

/// The Settings window (⌘,).
struct SettingsView: View {
    @Environment(AppSettings.self) private var settings

    var body: some View {
        @Bindable var settings = settings
        Form {
            Section {
                Toggle("Launch at login", isOn: $settings.launchAtLogin)
                if settings.launchAtLoginStatus == .requiresApproval {
                    HStack {
                        Text("Allow MacDroidFS in System Settings to finish turning this on.")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                        Button("Open Login Items") { SMAppService.openSystemSettingsLoginItems() }
                    }
                }
                if let error = settings.launchAtLoginError {
                    Text(error)
                        .font(.callout)
                        .foregroundStyle(.red)
                }
            }
            Section {
                Toggle("Show icon in menu bar", isOn: $settings.showMenuBarIcon)
                Toggle("Show icon in Dock", isOn: Binding(
                    get: { settings.dockIconVisible },
                    set: { settings.showDockIcon = $0 }
                ))
                .disabled(!settings.canHideDockIcon)
            } footer: {
                if !settings.canHideDockIcon {
                    Text("The Dock icon can be hidden only while the menu bar icon is shown.")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .formStyle(.grouped)
        .frame(width: 440)
        .fixedSize(horizontal: false, vertical: true)
        .onAppear { settings.refreshLaunchAtLogin() }
    }
}
