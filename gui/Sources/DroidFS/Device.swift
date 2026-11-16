import Foundation

/// One device as reported by the daemon's control API.
struct Device: Decodable, Identifiable, Equatable, Sendable {
    let id: String
    let transport: String
    let model: String
    let serial: String
    let point: String
    let state: DeviceState
    let error: String?

    /// The name shown to the user.
    var displayName: String { model.isEmpty ? serial : model }

    /// "ADB" or "MTP".
    var transportName: String { transport.uppercased() }

    /// Whether the volume is (still) mounted and can be opened.
    var canOpen: Bool { state == .mounted || state == .offline }

    /// Whether the volume can be ejected now.
    var canEject: Bool { state == .mounted || state == .offline || state == .connecting }

    /// Whether the volume was ejected and can be mounted again.
    var canMount: Bool { state == .ejected }
}

/// Where a device stands; mirrors manager.State in the daemon.
enum DeviceState: String, Decodable, Sendable {
    case mounted
    case offline
    case connecting
    case ejected
    case disconnected
    case unknown

    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = DeviceState(rawValue: raw) ?? .unknown
    }

    var title: LocalizedStringResource {
        switch self {
        case .mounted: "Mounted"
        case .offline: "Connection Lost"
        case .connecting: "Connecting"
        case .ejected: "Ejected"
        case .disconnected: "Disconnected"
        case .unknown: "Unknown"
        }
    }
}
