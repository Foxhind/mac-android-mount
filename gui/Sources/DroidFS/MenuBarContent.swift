import AppKit
import SwiftUI

/// The menu of the menu bar icon.
struct MenuBarContent: View {
    @Environment(DaemonController.self) private var daemon
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        switch daemon.runState {
        case .failed:
            Text("droidfs Is Not Running")
            Button("Restart") { daemon.restart() }
        case .starting, .stopped:
            Text("Starting…")
        case .running where daemon.devices.isEmpty:
            Text("No Devices")
        case .running:
            ForEach(daemon.devices) { device in
                Menu("\(device.displayName) — \(String(localized: device.state.title))") {
                    Button("Open in Finder") { daemon.open(device) }
                        .disabled(!device.canOpen)
                    if device.canMount {
                        Button("Mount") { daemon.mount(device) }
                    } else {
                        Button("Eject") { daemon.eject(device) }
                            .disabled(!device.canEject)
                    }
                }
            }
        }
        Divider()
        Button("Open DroidFS…") {
            NSApp.activate()
            openWindow(id: DroidFSApp.mainWindowID)
        }
        Button("Settings…") {
            NSApp.activate()
            openSettings()
        }
        .keyboardShortcut(",")
        Divider()
        Button("Quit DroidFS") { NSApp.terminate(nil) }
            .keyboardShortcut("q")
    }
}
