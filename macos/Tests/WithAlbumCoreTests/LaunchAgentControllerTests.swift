import XCTest
@testable import WithAlbumCore

/// The controller in dry-run mode against a temporary home: real files,
/// no launchctl calls (they are only logged).
final class LaunchAgentControllerTests: XCTestCase {
    var home: String!
    var ctl: LaunchAgentController!
    let helper = "/Applications/WithAlbum Server.app/Contents/Helpers/withalbum-server"

    override func setUpWithError() throws {
        home = FileManager.default.temporaryDirectory.appendingPathComponent("wa-home-\(UUID())").path
        try FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
        ctl = LaunchAgentController(location: AgentLocation(home: home), dryRun: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(atPath: home)
    }

    private func config() -> AgentConfig {
        AgentConfig(program: helper, logDirectory: ctl.location.logDirectory)
    }

    func testInstallWritesLoginPlistAndPrivateLogDirectory() throws {
        XCTAssertNil(ctl.installed())
        try ctl.install(config(), loginStart: true, helperPath: helper, takeOver: false)
        let inst = try XCTUnwrap(ctl.installed())
        XCTAssertEqual(inst.path, home + "/Library/LaunchAgents/org.withalbum.server.plist")
        XCTAssertTrue(inst.loginStart)
        XCTAssertEqual(inst.agent?.owner(helperPath: helper), .thisApp)
        let logMode = try FileManager.default.attributesOfItem(atPath: ctl.location.logDirectory)[.posixPermissions] as? Int
        XCTAssertEqual(logMode, 0o700)
        let plistMode = try FileManager.default.attributesOfItem(atPath: inst.path)[.posixPermissions] as? Int
        XCTAssertEqual(plistMode, 0o644)
    }

    func testLoginToggleMovesThePlist() throws {
        try ctl.install(config(), loginStart: true, helperPath: helper, takeOver: false)
        try ctl.setLoginStart(false)
        let off = try XCTUnwrap(ctl.installed())
        XCTAssertFalse(off.loginStart)
        XCTAssertEqual(off.path, home + "/Library/Application Support/WithAlbum/ServerApp/org.withalbum.server.plist")
        XCTAssertFalse(FileManager.default.fileExists(atPath: ctl.location.loginPlist))
        XCTAssertEqual(off.agent?.owner(helperPath: helper), .thisApp)
        try ctl.setLoginStart(true)
        XCTAssertTrue(try XCTUnwrap(ctl.installed()).loginStart)
        XCTAssertFalse(FileManager.default.fileExists(atPath: ctl.location.manualPlist))
    }

    func testNeverOverwritesAnotherAgentWithoutTakeOver() throws {
        let script = AgentConfig(program: home + "/.local/share/withalbum-server/withalbum-server",
                                 environment: ["WITHALBUM_ROOT": "/Volumes/T7/WithAlbum"], logDirectory: ctl.location.logDirectory)
        try FileManager.default.createDirectory(atPath: home + "/Library/LaunchAgents", withIntermediateDirectories: true)
        try script.plistData().write(to: URL(fileURLWithPath: ctl.location.loginPlist))
        XCTAssertThrowsError(try ctl.install(config(), loginStart: true, helperPath: helper, takeOver: false))
        XCTAssertThrowsError(try ctl.unregister(helperPath: helper))
        XCTAssertEqual(ctl.installed()?.agent?.owner(helperPath: helper), .installScript)

        let (c, _) = try XCTUnwrap(ctl.installed()?.agent).takeOver(helperPath: helper, logDirectory: ctl.location.logDirectory)
        try ctl.install(c, loginStart: true, helperPath: helper, takeOver: true)
        let now = try XCTUnwrap(ctl.installed()?.agent)
        XCTAssertEqual(now.owner(helperPath: helper), .thisApp)
        XCTAssertEqual(now.environment["WITHALBUM_ROOT"], "/Volumes/T7/WithAlbum")
    }

    func testUnreadablePlistIsNotOverwritten() throws {
        try FileManager.default.createDirectory(atPath: home + "/Library/LaunchAgents", withIntermediateDirectories: true)
        try Data("garbage".utf8).write(to: URL(fileURLWithPath: ctl.location.loginPlist))
        XCTAssertNil(ctl.installed()?.agent)
        XCTAssertThrowsError(try ctl.install(config(), loginStart: true, helperPath: helper, takeOver: false))
        XCTAssertEqual(FileManager.default.contents(atPath: ctl.location.loginPlist), Data("garbage".utf8))
    }

    func testUnregisterKeepsLogs() throws {
        try ctl.install(config(), loginStart: false, helperPath: helper, takeOver: false)
        try Data("log".utf8).write(to: URL(fileURLWithPath: ctl.location.logDirectory + "/error.log"))
        try ctl.unregister(helperPath: helper)
        XCTAssertNil(ctl.installed())
        XCTAssertTrue(FileManager.default.fileExists(atPath: ctl.location.logDirectory + "/error.log"))
    }

    func testInvalidConfigWritesNothing() {
        var c = config()
        c.lan = true
        c.listen = "203.0.113.1:8787"
        XCTAssertThrowsError(try ctl.install(c, loginStart: true, helperPath: helper, takeOver: false))
        XCTAssertNil(ctl.installed())
    }
}
