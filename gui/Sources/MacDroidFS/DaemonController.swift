import AppKit
import Foundation
import Observation

/// Runs the bundled macdroidfs daemon as a child process, collects its log and
/// keeps the device list current through the control socket.
@MainActor
@Observable
final class DaemonController {
    enum RunState: Equatable {
        case stopped
        case starting
        case running
        case failed(String)
    }

    private(set) var runState: RunState = .stopped
    private(set) var devices: [Device] = []
    private(set) var log = ""
    /// The last command that failed, shown until the next one succeeds.
    private(set) var commandError: String?

    @ObservationIgnored private var process: Process?
    @ObservationIgnored private var lifeline: Pipe?
    @ObservationIgnored private var pollTask: Task<Void, Never>?
    @ObservationIgnored private var stopping = false
    @ObservationIgnored private let client: ControlClient

    /// Keeps the log view responsive; older lines are dropped first.
    private static let maxLogLength = 200_000
    private static let pollInterval: Duration = .seconds(1)
    /// The daemon unmounts every volume on SIGTERM; give it this long.
    private static let shutdownTimeout: Duration = .seconds(20)

    init() {
        // The per-user temporary directory is private (mode 0700) and short
        // enough for a socket path.
        let socket = FileManager.default.temporaryDirectory
            .appendingPathComponent("macdroidfs-control.sock").path
        client = ControlClient(socketPath: socket)
    }

    /// Starts the daemon if it is not running.
    func start() {
        guard process == nil else { return }
        guard let binary = Bundle.main.url(forAuxiliaryExecutable: "macdroidfsd") else {
            runState = .failed(String(localized: "The macdroidfsd daemon is missing from the app bundle."))
            return
        }
        let proc = Process()
        proc.executableURL = binary
        proc.arguments = ["-control", client.socketPath, "-stdin-lifeline"]

        // The daemon shuts down when this pipe closes, which also happens
        // when the app crashes.
        let lifeline = Pipe()
        proc.standardInput = lifeline

        let output = Pipe()
        proc.standardOutput = output
        proc.standardError = output
        output.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty else {
                handle.readabilityHandler = nil
                return
            }
            let text = String(decoding: data, as: UTF8.self)
            Task { @MainActor in self?.appendLog(text) }
        }
        let id = ObjectIdentifier(proc)
        proc.terminationHandler = { [weak self] p in
            let status = p.terminationStatus
            let signaled = p.terminationReason == .uncaughtSignal
            Task { @MainActor in self?.exited(id, status: status, signaled: signaled) }
        }

        do {
            try proc.run()
        } catch {
            runState = .failed(error.localizedDescription)
            return
        }
        process = proc
        self.lifeline = lifeline
        stopping = false
        runState = .starting
        commandError = nil
        startPolling()
    }

    /// Stops the daemon and waits until it has unmounted everything.
    func stop() async {
        guard let proc = process else { return }
        stopping = true
        pollTask?.cancel()
        proc.terminate() // SIGTERM: a clean shutdown
        let deadline = ContinuousClock.now + Self.shutdownTimeout
        while proc.isRunning, ContinuousClock.now < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
        if proc.isRunning {
            kill(proc.processIdentifier, SIGKILL)
        }
    }

    /// Restarts a daemon that failed or was stopped.
    func restart() {
        Task {
            await stop()
            process = nil
            start()
        }
    }

    func eject(_ device: Device) { send("eject", to: device) }

    func mount(_ device: Device) { send("mount", to: device) }

    /// Opens the device's volume in Finder.
    func open(_ device: Device) {
        NSWorkspace.shared.open(URL(fileURLWithPath: device.point))
    }

    func clearLog() { log = "" }

    // MARK: - Internals

    private func send(_ command: String, to device: Device) {
        let client = client
        let id = device.id
        let name = device.displayName
        Task {
            do {
                try await Task.detached { try client.send(command, to: id) }.value
                commandError = nil
                await refresh()
            } catch {
                let reason = String(describing: error)
                commandError = command == "eject"
                    ? String(localized: "Could not eject \(name): \(reason)")
                    : String(localized: "Could not mount \(name): \(reason)")
            }
        }
    }

    private func startPolling() {
        pollTask?.cancel()
        pollTask = Task {
            while !Task.isCancelled {
                await refresh()
                try? await Task.sleep(for: Self.pollInterval)
            }
        }
    }

    private func refresh() async {
        let client = client
        do {
            let list = try await Task.detached { try client.devices() }.value
            guard !stopping else { return }
            devices = list
            if runState == .starting { runState = .running }
        } catch {
            // Expected while the daemon starts; a dead daemon is reported
            // by exited(_:status:signaled:).
        }
    }

    private func appendLog(_ text: String) {
        log += text
        if log.count > Self.maxLogLength {
            log = String(log.suffix(Self.maxLogLength / 2))
        }
    }

    private func exited(_ id: ObjectIdentifier, status: Int32, signaled: Bool) {
        guard let proc = process, ObjectIdentifier(proc) == id else { return }
        pollTask?.cancel()
        devices = []
        if stopping {
            runState = .stopped
        } else {
            process = nil
            lifeline = nil
            runState = .failed(signaled
                ? String(localized: "macdroidfs was killed by signal \(status). See the log for details.")
                : String(localized: "macdroidfs exited with status \(status). See the log for details."))
        }
    }
}
