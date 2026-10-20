import XCTest
@testable import WithAlbumCore

final class AgentPlistTests: XCTestCase {
    let helper = "/Applications/WithAlbum Server.app/Contents/Helpers/withalbum-server"
    let logs = "/Users/test/Library/Logs/WithAlbum"

    private func dict(_ c: AgentConfig) throws -> [String: Any] {
        let data = try c.plistData()
        return try XCTUnwrap(PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any])
    }

    func testLoopbackPlistMatchesInstallScript() throws {
        let d = try dict(AgentConfig(program: helper, logDirectory: logs))
        XCTAssertEqual(d["Label"] as? String, "org.withalbum.server")
        XCTAssertEqual(d["ProgramArguments"] as? [String], [helper])
        XCTAssertEqual(d["EnvironmentVariables"] as? [String: String], ["WITHALBUM_LISTEN": "127.0.0.1:8787"])
        XCTAssertEqual(d["RunAtLoad"] as? Bool, true)
        XCTAssertEqual(d["KeepAlive"] as? Bool, true)
        XCTAssertEqual(d["ThrottleInterval"] as? Int, 30)
        XCTAssertEqual(d["StandardOutPath"] as? String, logs + "/server.log")
        XCTAssertEqual(d["StandardErrorPath"] as? String, logs + "/error.log")
        XCTAssertEqual(Set(d.keys), ["Label", "ProgramArguments", "EnvironmentVariables", "RunAtLoad", "KeepAlive",
                                     "ThrottleInterval", "StandardOutPath", "StandardErrorPath"])
    }

    func testLANPlist() throws {
        let c = AgentConfig(program: helper, lan: true, listen: "192.168.0.10:8787",
                            environment: ["WITHALBUM_ROOT": "/Volumes/T7/WithAlbum"], logDirectory: logs)
        let d = try dict(c)
        XCTAssertEqual(d["ProgramArguments"] as? [String], [helper, "-tls", "-allow-lan"])
        XCTAssertEqual(d["EnvironmentVariables"] as? [String: String],
                       ["WITHALBUM_LISTEN": "192.168.0.10:8787", "WITHALBUM_ROOT": "/Volumes/T7/WithAlbum"])
    }

    func testEscapingRoundTrips() throws {
        let odd = "/Volumes/A&B <C> \"D\" 'E' 사진/WithAlbum"
        let c = AgentConfig(program: "/Users/x/My & <Apps>/WithAlbum Server.app/Contents/Helpers/withalbum-server",
                            environment: ["WITHALBUM_ROOT": odd, "WITHALBUM_CONFIG": "/tmp/a&b.json"], logDirectory: logs)
        let data = try c.plistData()
        let xml = String(decoding: data, as: UTF8.self)
        XCTAssertTrue(xml.contains("A&amp;B &lt;C&gt;"), xml)
        XCTAssertFalse(xml.contains("<C>"))
        let back = try XCTUnwrap(ExistingAgent.parse(data))
        XCTAssertEqual(back.environment["WITHALBUM_ROOT"], odd)
        XCTAssertEqual(back.program, c.program)
        // plutil agrees that it is a valid plist.
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("wa-\(UUID()).plist")
        try data.write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/plutil")
        p.arguments = ["-lint", "-s", url.path]
        try p.run()
        p.waitUntilExit()
        XCTAssertEqual(p.terminationStatus, 0)
    }

    func testRefusesWhatTheScriptRefuses() {
        XCTAssertThrowsError(try AgentConfig(program: helper, environment: ["WITHALBUM_ROOT": "/a\nb"], logDirectory: logs).plistData())
        XCTAssertThrowsError(try AgentConfig(program: helper, environment: ["WITHALBUM_CONFIG": "/a\rb"], logDirectory: logs).plistData())
        XCTAssertThrowsError(try AgentConfig(program: "withalbum-server", logDirectory: logs).plistData())
        XCTAssertThrowsError(try AgentConfig(program: helper, environment: ["WITHALBUM_ROOT": "rel/dir"], logDirectory: logs).plistData())
        // Plain HTTP is loopback only; LAN needs a private address.
        XCTAssertThrowsError(try AgentConfig(program: helper, lan: false, listen: "192.168.0.10:8787", logDirectory: logs).plistData())
        XCTAssertThrowsError(try AgentConfig(program: helper, lan: true, listen: "8.8.8.8:8787", logDirectory: logs).plistData())
        XCTAssertThrowsError(try AgentConfig(program: helper, lan: true, listen: "127.0.0.1:8787", logDirectory: logs).plistData())
        XCTAssertThrowsError(try AgentConfig(program: helper, lan: true, listen: "mac.local:8787", logDirectory: logs).plistData())
    }

    /// The exact text scripts/install-macos.sh writes for
    /// `--lan 192.168.0.10:8787` with WITHALBUM_CONFIG set.
    let scriptPlist = """
    <?xml version="1.0" encoding="UTF-8"?>
    <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
    <plist version="1.0">
    <dict>
      <key>Label</key><string>org.withalbum.server</string>
      <key>ProgramArguments</key><array><string>/Users/test/.local/share/withalbum-server/withalbum-server</string><string>-tls</string><string>-allow-lan</string></array>
      <key>EnvironmentVariables</key><dict>
        <key>WITHALBUM_LISTEN</key><string>192.168.0.10:8787</string>
        <key>WITHALBUM_CONFIG</key><string>/Users/test/cfg &amp; more/server.json</string>
      </dict>
      <key>RunAtLoad</key><true/>
      <key>KeepAlive</key><true/>
      <key>ThrottleInterval</key><integer>30</integer>
      <key>StandardOutPath</key><string>/Users/test/Library/Logs/WithAlbum/server.log</string>
      <key>StandardErrorPath</key><string>/Users/test/Library/Logs/WithAlbum/error.log</string>
    </dict>
    </plist>
    """

    func testDetectsAndTakesOverScriptAgent() throws {
        let a = try XCTUnwrap(ExistingAgent.parse(Data(scriptPlist.utf8)))
        XCTAssertEqual(a.owner(helperPath: helper), .installScript)
        XCTAssertTrue(a.lan)
        XCTAssertEqual(a.listen, "192.168.0.10:8787")
        let (c, dropped) = a.takeOver(helperPath: helper, logDirectory: logs)
        XCTAssertFalse(dropped)
        XCTAssertEqual(c.arguments, [helper, "-tls", "-allow-lan"])
        XCTAssertEqual(c.fullEnvironment, ["WITHALBUM_LISTEN": "192.168.0.10:8787",
                                           "WITHALBUM_CONFIG": "/Users/test/cfg & more/server.json"])
        let mine = try XCTUnwrap(ExistingAgent.parse(try c.plistData()))
        XCTAssertEqual(mine.owner(helperPath: helper), .thisApp)
        XCTAssertEqual(mine.owner(helperPath: "/Users/x/Downloads/WithAlbum Server.app/Contents/Helpers/withalbum-server"), .otherAppCopy)
    }

    func testTakeOverFailsClosedOnBadLANAddress() throws {
        let bad = scriptPlist.replacingOccurrences(of: "192.168.0.10:8787", with: "203.0.113.9:8787")
        let a = try XCTUnwrap(ExistingAgent.parse(Data(bad.utf8)))
        let (c, dropped) = a.takeOver(helperPath: helper, logDirectory: logs)
        XCTAssertTrue(dropped)
        XCTAssertFalse(c.lan)
        XCTAssertEqual(c.listen, "127.0.0.1:8787")
        XCTAssertEqual(c.arguments, [helper])
        XCTAssertNoThrow(try c.plistData())
    }

    func testTakeOverKeepsLoopbackScriptAgent() throws {
        let plain = scriptPlist
            .replacingOccurrences(of: "<string>-tls</string><string>-allow-lan</string>", with: "")
            .replacingOccurrences(of: "192.168.0.10:8787", with: "127.0.0.1:8787")
            .replacingOccurrences(of: "<key>WITHALBUM_CONFIG</key>", with: "<key>WITHALBUM_ROOT</key>")
        let a = try XCTUnwrap(ExistingAgent.parse(Data(plain.utf8)))
        XCTAssertFalse(a.lan)
        let (c, _) = a.takeOver(helperPath: helper, logDirectory: logs)
        XCTAssertEqual(c.storageRoot, "/Users/test/cfg & more/server.json")
        XCTAssertEqual(c.arguments, [helper])
    }

    func testUnreadablePlist() {
        XCTAssertNil(ExistingAgent.parse(Data("not a plist".utf8)))
        XCTAssertNil(ExistingAgent.parse(Data("<plist><dict></dict></plist>".utf8)))
    }

    func testBundlePaths() {
        XCTAssertEqual(AppBundle.helperPath(bundlePath: "/Applications/WithAlbum Server.app"), helper)
        XCTAssertTrue(AppBundle.canRegister(bundlePath: "/Applications/WithAlbum Server.app"))
        XCTAssertFalse(AppBundle.canRegister(bundlePath: "/Volumes/WithAlbum Server/WithAlbum Server.app"))
        XCTAssertFalse(AppBundle.canRegister(bundlePath: "/private/var/folders/x/AppTranslocation/ABC/d/WithAlbum Server.app"))
        let loc = AgentLocation(home: "/Users/test")
        XCTAssertEqual(loc.loginPlist, "/Users/test/Library/LaunchAgents/org.withalbum.server.plist")
        XCTAssertEqual(loc.logDirectory, "/Users/test/Library/Logs/WithAlbum")
        XCTAssertEqual(loc.defaultConfigFile, "/Users/test/Library/Application Support/WithAlbum/server.json")
    }
}
