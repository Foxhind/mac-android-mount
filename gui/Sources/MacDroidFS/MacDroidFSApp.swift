import AppKit
import SwiftUI

@main
struct MacDroidFSApp: App {
    @NSApplicationDelegateAdaptor private var appDelegate: AppDelegate
    @State private var settings = AppSettings.shared

    static let mainWindowID = "main"

    var body: some Scene {
        Window("MacDroidFS", id: Self.mainWindowID) {
            MainView()
                .environment(appDelegate.daemon)
        }
        .defaultSize(width: 720, height: 460)
        // With the Dock icon hidden the app starts in the menu bar only.
        // Window restoration would reopen the window regardless, so it is off.
        .defaultLaunchBehavior(settings.dockIconVisible ? .presented : .suppressed)
        .restorationBehavior(.disabled)

        Settings {
            SettingsView()
                .environment(settings)
        }

        MenuBarExtra("MacDroidFS", systemImage: "externaldrive.connected.to.line.below",
                     isInserted: $settings.showMenuBarIcon) {
            MenuBarContent()
                .environment(appDelegate.daemon)
        }
    }
}

/// Owns the daemon and handles the app life cycle around it.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let daemon = DaemonController()

    func applicationWillFinishLaunching(_ notification: Notification) {
        AppSettings.shared.applyDockPolicy()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        daemon.start()
    }

    /// The app keeps serving the volumes with every window closed.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }

    /// Quitting stops the daemon first, so every volume is unmounted cleanly.
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        Task {
            await daemon.stop()
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }
}
