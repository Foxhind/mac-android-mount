// swift-tools-version: 6.0

// The MacDroidFS app: a window and a menu bar item in front of the macdroidfs
// daemon. `make app` builds it and assembles bin/MacDroidFS.app.
import PackageDescription

let package = Package(
    name: "MacDroidFS",
    platforms: [.macOS(.v15)],
    targets: [
        .executableTarget(name: "MacDroidFS", path: "Sources/MacDroidFS")
    ]
)
