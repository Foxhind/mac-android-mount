import SwiftUI

/// One device in the main window: what it is, where it stands, and what can
/// be done with its volume.
struct DeviceRow: View {
    @Environment(DaemonController.self) private var daemon
    let device: Device

    var body: some View {
        HStack(alignment: .firstTextBaseline) {
            Image(systemName: "smartphone")
                .font(.title2)
                .foregroundStyle(.secondary)
            VStack(alignment: .leading) {
                HStack(alignment: .firstTextBaseline) {
                    Text(device.displayName)
                        .font(.headline)
                    Text(device.transportName)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    StatusBadge(state: device.state)
                }
                Text(device.point)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                if let error = device.error {
                    Text(error)
                        .font(.callout)
                        .foregroundStyle(.red)
                }
            }
            Spacer()
            actions
        }
    }

    @ViewBuilder
    private var actions: some View {
        if device.canOpen {
            Button("Open in Finder") { daemon.open(device) }
        }
        if device.canEject {
            Button("Eject") { daemon.eject(device) }
        } else if device.canMount {
            Button("Mount") { daemon.mount(device) }
        }
    }
}

/// A device's state as a coloured symbol and a word.
struct StatusBadge: View {
    let state: DeviceState

    var body: some View {
        Label {
            Text(state.title)
        } icon: {
            Image(systemName: symbol)
        }
        .font(.caption)
        .foregroundStyle(color)
    }

    private var symbol: String {
        switch state {
        case .mounted: "checkmark.circle.fill"
        case .offline: "exclamationmark.triangle.fill"
        case .connecting: "arrow.triangle.2.circlepath"
        case .ejected: "eject.circle"
        case .disconnected, .unknown: "minus.circle"
        }
    }

    private var color: Color {
        switch state {
        case .mounted: .green
        case .offline: .orange
        case .connecting, .ejected, .disconnected, .unknown: .secondary
        }
    }
}
