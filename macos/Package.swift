// swift-tools-version:5.9
// WithAlbum Server menu bar app (macOS 13+). Build the .app and DMG with
// scripts/build-macos-app.sh; run the unit tests with `swift test` here.
import PackageDescription

let package = Package(
    name: "WithAlbumServerApp",
    platforms: [.macOS(.v13)],
    products: [
        .executable(name: "WithAlbumServer", targets: ["WithAlbumServerApp"]),
    ],
    targets: [
        // Pure logic (no UI, no launchctl): plist, LAN address rules, status
        // and log parsing. Everything here is unit tested.
        .target(
            name: "WithAlbumCore",
            swiftSettings: [.enableUpcomingFeature("ConciseMagicFile")]
        ),
        .executableTarget(
            name: "WithAlbumServerApp",
            dependencies: ["WithAlbumCore"],
            swiftSettings: [.enableUpcomingFeature("ConciseMagicFile")]
        ),
        .testTarget(
            name: "WithAlbumCoreTests",
            dependencies: ["WithAlbumCore"]
        ),
    ]
)
