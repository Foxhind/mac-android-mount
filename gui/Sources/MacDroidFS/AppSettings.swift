import AppKit
import Observation
import ServiceManagement

/// The app's preferences. The Dock icon can only be hidden while the menu
/// bar icon is shown, so the app always stays reachable.
@MainActor
@Observable
final class AppSettings {
    static let shared = AppSettings()

    private enum Key {
        static let showMenuBarIcon = "showMenuBarIcon"
        static let showDockIcon = "showDockIcon"
    }

    var showMenuBarIcon: Bool {
        didSet {
            UserDefaults.standard.set(showMenuBarIcon, forKey: Key.showMenuBarIcon)
            applyDockPolicy()
        }
    }

    /// What the user chose; see dockIconVisible for what is in effect.
    var showDockIcon: Bool {
        didSet {
            UserDefaults.standard.set(showDockIcon, forKey: Key.showDockIcon)
            applyDockPolicy()
        }
    }

    /// Whether the Dock icon is shown: always, while the menu bar icon is off.
    var dockIconVisible: Bool { showDockIcon || !showMenuBarIcon }

    /// Whether the user may switch the Dock icon off right now.
    var canHideDockIcon: Bool { showMenuBarIcon }

    private(set) var launchAtLoginStatus: SMAppService.Status = SMAppService.mainApp.status
    private(set) var launchAtLoginError: String?

    private init() {
        UserDefaults.standard.register(defaults: [
            Key.showMenuBarIcon: true,
            Key.showDockIcon: true,
        ])
        showMenuBarIcon = UserDefaults.standard.bool(forKey: Key.showMenuBarIcon)
        showDockIcon = UserDefaults.standard.bool(forKey: Key.showDockIcon)
    }

    /// Shows or hides the Dock icon to match the settings.
    func applyDockPolicy() {
        let policy: NSApplication.ActivationPolicy = dockIconVisible ? .regular : .accessory
        guard NSApp.activationPolicy() != policy else { return }
        NSApp.setActivationPolicy(policy)
        // Changing the policy deactivates the app; keep the open windows in front.
        NSApp.activate()
    }

    var launchAtLogin: Bool {
        get { launchAtLoginStatus == .enabled || launchAtLoginStatus == .requiresApproval }
        set {
            do {
                if newValue {
                    try SMAppService.mainApp.register()
                } else {
                    try SMAppService.mainApp.unregister()
                }
                launchAtLoginError = nil
            } catch {
                launchAtLoginError = error.localizedDescription
            }
            refreshLaunchAtLogin()
        }
    }

    /// Re-reads the login item status, which the user can change in System Settings.
    func refreshLaunchAtLogin() {
        launchAtLoginStatus = SMAppService.mainApp.status
    }
}
