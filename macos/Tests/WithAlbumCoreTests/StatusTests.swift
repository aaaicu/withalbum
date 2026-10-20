import XCTest
@testable import WithAlbumCore

final class StatusTests: XCTestCase {
    func testHealth() {
        let ok = Health.parse(Data(#"{"integrity":"ok","status":"ok","storage":"ok","version":"0.1.0"}"#.utf8))
        XCTAssertEqual(ok, Health(version: "0.1.0", storageOK: true, integrityOK: true))
        let down = Health.parse(Data(#"{"integrity":"warning","status":"ok","storage":"unavailable","version":"dev"}"#.utf8))
        XCTAssertEqual(down, Health(version: "dev", storageOK: false, integrityOK: false))
        XCTAssertNil(Health.parse(Data(#"{"error":"not_found"}"#.utf8)))
        XCTAssertNil(Health.parse(Data("<html>".utf8)))
    }

    func testCombine() {
        let h = Health(version: "1", storageOK: true, integrityOK: true)
        let bad = Health(version: "1", storageOK: false, integrityOK: true)
        let running = AgentRuntime(loaded: true, running: true, pid: 10)
        let exited = AgentRuntime(loaded: true, running: false, lastExitCode: "1")
        func c(_ p: Bool, _ r: AgentRuntime, _ h: Health?, setup: Bool = false, answered: Bool = false) -> ServerState {
            ServerState.combine(agentPlistExists: p, runtime: r, health: h, setupPage: setup, answered: answered || h != nil || setup)
        }
        XCTAssertEqual(c(true, running, h), .running(h))
        XCTAssertEqual(c(true, running, bad), .storageUnavailable(bad))
        XCTAssertEqual(c(true, running, nil, setup: true), .setupNeeded)
        XCTAssertEqual(c(true, running, nil), .starting)
        XCTAssertEqual(c(true, exited, nil), .failing)
        XCTAssertEqual(c(true, .notLoaded, nil), .stopped)
        XCTAssertEqual(c(false, .notLoaded, nil), .notRegistered)
        XCTAssertEqual(c(false, .notLoaded, h), .unmanagedRunning)
        XCTAssertEqual(c(true, .notLoaded, h), .unmanagedRunning)
        XCTAssertEqual(c(false, .notLoaded, nil, setup: true), .unmanagedRunning)
        XCTAssertEqual(c(false, .notLoaded, nil, answered: true), .unmanagedRunning)
    }

    func testLaunchctlPrint() {
        let text = """
        gui/501/org.withalbum.server = {
        \tactive count = 1
        \tpath = /Users/test/Library/LaunchAgents/org.withalbum.server.plist
        \ttype = LaunchAgent
        \tstate = running

        \tprogram = /Applications/WithAlbum Server.app/Contents/Helpers/withalbum-server
        \tenvironment = {
        \t\tstate = not-this-one
        \t}
        \tpid = 4242
        \tlast exit code = (never exited)
        }
        """
        let r = AgentRuntime.parsePrint(text)
        XCTAssertTrue(r.loaded)
        XCTAssertTrue(r.running)
        XCTAssertEqual(r.pid, 4242)
        XCTAssertEqual(r.lastExitCode, "(never exited)")
        XCTAssertEqual(r.path, "/Users/test/Library/LaunchAgents/org.withalbum.server.plist")
        let waiting = AgentRuntime.parsePrint("x = {\n\tstate = spawn scheduled\n\tlast exit code = 1\n}")
        XCTAssertFalse(waiting.running)
        XCTAssertEqual(waiting.lastExitCode, "1")
    }

    func testSetupCodeFromLog() throws {
        let log = """
        2026/10/20 09:00:00 SETUP: open http://127.0.0.1:8787/setup on this Mac and enter the setup code  AAAA-BBBB-CCCC
        2026/10/20 09:00:00 SETUP: the code works once, for 30 minutes (until 09:30), and is disabled after 5 wrong entries; restart the server for a new one.
        2026/10/20 10:15:07 setup: no registered storage yet; starting setup mode (x)
        2026/10/20 10:15:07 EXPERIMENTAL / SETUP MODE, LOOPBACK ONLY, PLAIN HTTP: listen=127.0.0.1:8787 storage=/withalbum-data
        2026/10/20 10:15:07 SETUP: open http://127.0.0.1:8787/setup on this Mac and enter the setup code  7KQ2-MNPX-9RTZ
        2026/10/20 10:15:07 SETUP: the code works once, for 30 minutes (until 10:45), and is disabled after 5 wrong entries; restart the server for a new one.
        """
        let c = try XCTUnwrap(SetupCode.lastIn(log))
        XCTAssertEqual(c.code, "7KQ2-MNPX-9RTZ")
        XCTAssertEqual(c.url, "http://127.0.0.1:8787/setup")
        XCTAssertTrue(c.isLoopback)
        let comps = Calendar.current.dateComponents([.hour, .minute], from: try XCTUnwrap(c.expires))
        XCTAssertEqual(comps.hour, 10)
        XCTAssertEqual(comps.minute, 45)
        XCTAssertNil(SetupCode.lastIn("2026/10/20 10:00:00 listening\n"))
        XCTAssertFalse(SetupCode(url: "http://192.168.0.2:8787/setup", code: "x", loggedAt: nil).isLoopback)
    }

    func testLastProblem() {
        let log = """
        2026/10/20 10:00:00 EXPERIMENTAL / LOOPBACK ONLY, PLAIN HTTP: listen=127.0.0.1:8787 root=/x volume_id=y
        2026/10/20 10:05:00 SECURITY: refusing non-loopback address "192.168.0.10:8787": it needs an active server administrator who can log in
        """
        XCTAssertEqual(LogReason.lastProblem(log)?.contains("SECURITY: refusing"), true)
        XCTAssertNil(LogReason.lastProblem(log + "\n2026/10/20 10:06:00 EXPERIMENTAL / LAN, TLS ONLY: listen=…"))
        XCTAssertNil(LogReason.lastProblem("2026/10/20 10:00:00 TLS: created a new key pair in /x (mode 0600).\n"
            + "2026/10/20 10:00:00 EXPERIMENTAL / LOOPBACK ONLY, TLS: listen=127.0.0.1:8787 root=/x volume_id=y"))
        let missing = "2026/10/20 10:00:00 open storage: root missing\n"
        XCTAssertEqual(LogReason.lastProblem("2026/10/20 09:00:00 EXPERIMENTAL / LOOPBACK ONLY, PLAIN HTTP: x\n" + missing),
                       "2026/10/20 10:00:00 open storage: root missing")
    }

    func testTLSPairing() throws {
        let json = """
        {"cert_sha256":"\(String(repeating: "ab", count: 32))","not_after":"2027-10-20T00:00:00Z",
         "pairing":[{"payload":"withalbum://pair?u=https%3A%2F%2F192.168.0.10%3A8787&p=x","url":"https://192.168.0.10:8787"}],
         "spki_sha256":"\(String(repeating: "0F", count: 32))","spki_sha256_base64":"Dw=="}
        """
        let p = try XCTUnwrap(TLSPairing.parse(Data(json.utf8)))
        XCTAssertEqual(p.spkiSHA256, String(repeating: "0f", count: 32))
        XCTAssertEqual(p.pairing.first?.url, "https://192.168.0.10:8787")
        XCTAssertEqual(TLSPairing.colons("0a1b2c"), "0A:1B:2C")
        XCTAssertNil(TLSPairing.parse(Data(#"{"spki_sha256":"short"}"#.utf8)))
    }

    func testSPKIPin() throws {
        // openssl ec -pubout -outform DER | shasum -a 256 for this P-256 key.
        let x963 = try XCTUnwrap(Data(hex: "0479477e05f056c1c78d32f62821ff50b4b7126d1a8b228593d72718fcc9f654e0df9427e700b7e6c4230e0d04d0182cb4a0714a437f0c8204970c6d7a2b228607"))
        XCTAssertEqual(SPKIPin.sha256Hex(p256X963: x963), "3bd78f068ae3510481abce9a156e717e9046c0ce70dcde0a4a38db006e4a9464")
        XCTAssertNil(SPKIPin.sha256Hex(p256X963: Data([4, 1, 2])))
    }

    func testConfigFile() {
        XCTAssertEqual(ServerConfigFile.storageRoot(Data(#"{"format":1,"storage_root":"/Volumes/T7/WithAlbum","volume_id":"v"}"#.utf8)),
                       "/Volumes/T7/WithAlbum")
        XCTAssertNil(ServerConfigFile.storageRoot(Data(#"{"storage_root":"relative"}"#.utf8)))
    }

    /// Dashboard wording rules (TestDashboardDesignRules): no "업로드 완료"
    /// and no "완료" anywhere in the app's UI text.
    func testWordingRules() throws {
        let sources = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("Sources")
        let files = try XCTUnwrap(FileManager.default.enumerator(at: sources, includingPropertiesForKeys: nil))
            .compactMap { $0 as? URL }.filter { $0.pathExtension == "swift" }
        XCTAssertGreaterThan(files.count, 3)
        for f in files {
            let text = try String(contentsOf: f, encoding: .utf8)
            XCTAssertFalse(text.contains("완료"), f.lastPathComponent)
        }
    }
}

extension Data {
    init?(hex: String) {
        var out = Data()
        var i = hex.startIndex
        while i < hex.endIndex {
            let j = hex.index(i, offsetBy: 2)
            guard let b = UInt8(hex[i..<j], radix: 16) else { return nil }
            out.append(b)
            i = j
        }
        self = out
    }
}
