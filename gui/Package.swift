// swift-tools-version: 6.0

// The DroidFS app: a window and a menu bar item in front of the droidfs
// daemon. `make app` builds it and assembles bin/DroidFS.app.
import PackageDescription

let package = Package(
    name: "DroidFS",
    platforms: [.macOS(.v15)],
    targets: [
        .executableTarget(name: "DroidFS", path: "Sources/DroidFS")
    ]
)
