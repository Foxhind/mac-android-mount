import AppKit
import SwiftUI

/// The main window: devices and the daemon log, chosen in the sidebar.
struct MainView: View {
    enum Pane: Hashable, CaseIterable {
        case devices
        case log

        var title: LocalizedStringKey {
            switch self {
            case .devices: "Devices"
            case .log: "Log"
            }
        }

        var systemImage: String {
            switch self {
            case .devices: "externaldrive"
            case .log: "text.alignleft"
            }
        }
    }

    @State private var pane: Pane? = .devices

    var body: some View {
        NavigationSplitView {
            List(Pane.allCases, id: \.self, selection: $pane) { pane in
                Label(pane.title, systemImage: pane.systemImage)
            }
            .navigationSplitViewColumnWidth(min: 150, ideal: 170)
        } detail: {
            switch pane ?? .devices {
            case .devices: DevicesView()
            case .log: LogView()
            }
        }
    }
}

/// The device list, or what stands in its place.
struct DevicesView: View {
    @Environment(DaemonController.self) private var daemon

    var body: some View {
        content
            .navigationTitle("Devices")
    }

    @ViewBuilder
    private var content: some View {
        switch daemon.runState {
        case .failed(let message):
            ContentUnavailableView {
                Label("droidfs Is Not Running", systemImage: "exclamationmark.triangle")
            } description: {
                Text(message)
            } actions: {
                Button("Restart") { daemon.restart() }
            }
        case .starting, .stopped:
            ProgressView("Starting…")
        case .running where daemon.devices.isEmpty:
            ContentUnavailableView {
                Label("No Devices", systemImage: "cable.connector")
            } description: {
                Text("Connect an Android device with USB debugging enabled, or choose “File transfer” on it.")
            }
        case .running:
            List(daemon.devices) { device in
                DeviceRow(device: device)
            }
            .safeAreaInset(edge: .bottom) {
                if let error = daemon.commandError {
                    Text(error)
                        .font(.callout)
                        .foregroundStyle(.red)
                        .padding()
                }
            }
        }
    }
}

/// The daemon's output, newest at the bottom.
struct LogView: View {
    @Environment(DaemonController.self) private var daemon

    private let bottomID = "bottom"

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                Text(daemon.log.isEmpty ? String(localized: "No messages yet.") : daemon.log)
                    .font(.system(.body, design: .monospaced))
                    .foregroundStyle(daemon.log.isEmpty ? .secondary : .primary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
                Color.clear.frame(height: 1).id(bottomID)
            }
            .onChange(of: daemon.log) {
                proxy.scrollTo(bottomID, anchor: .bottom)
            }
            .onAppear {
                proxy.scrollTo(bottomID, anchor: .bottom)
            }
        }
        .navigationTitle("Log")
        .toolbar {
            Button("Copy", systemImage: "doc.on.doc") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(daemon.log, forType: .string)
            }
            .disabled(daemon.log.isEmpty)
            Button("Clear", systemImage: "trash") { daemon.clearLog() }
                .disabled(daemon.log.isEmpty)
        }
    }
}
