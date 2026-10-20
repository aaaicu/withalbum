import XCTest
@testable import WithAlbumCore

final class ListenAddressTests: XCTestCase {
    func testScriptPattern() {
        // scripts/install-macos.sh: ^[][0-9A-Fa-f.:]+:[0-9]+$
        for ok in ["192.168.0.10:8787", "[fd00::10]:8787", "10.0.0.1:1", "0.0.0.0:8787", "[::]:8787"] {
            XCTAssertTrue(ListenAddress.scriptAccepts(ok), ok)
        }
        for bad in ["mac.local:8787", "192.168.0.10", "192.168.0.10:", "192.168.0.10:87a", " 192.168.0.10:8787",
                    "192.168.0.10:8787\n", "fe80::1%en0:8787", "http://192.168.0.10:8787"] {
            XCTAssertFalse(ListenAddress.scriptAccepts(bad), bad)
        }
    }

    func testServerScope() {
        // server/listen.go scopeOf
        XCTAssertEqual(try ListenAddress.scope("127.0.0.1:8787").get(), .loopback)
        XCTAssertEqual(try ListenAddress.scope("localhost:8787").get(), .loopback)
        XCTAssertEqual(try ListenAddress.scope("[::1]:8787").get(), .loopback)
        XCTAssertEqual(try ListenAddress.scope("127.8.9.10:8787").get(), .loopback)
        XCTAssertEqual(try ListenAddress.scope(":8787").get(), .wildcard)
        XCTAssertEqual(try ListenAddress.scope("0.0.0.0:8787").get(), .wildcard)
        XCTAssertEqual(try ListenAddress.scope("[::]:8787").get(), .wildcard)
        for p in ["10.1.2.3:8787", "172.16.0.1:8787", "172.31.255.255:8787", "192.168.0.10:8787",
                  "[fd00::10]:8787", "[fc00::1]:8787", "[::ffff:192.168.1.2]:8787"] {
            XCTAssertEqual(try ListenAddress.scope(p).get(), .privateLAN, p)
        }
        for pub in ["8.8.8.8:8787", "172.32.0.1:8787", "172.15.255.255:8787", "192.169.0.1:8787", "100.64.0.1:8787",
                    "169.254.1.1:8787", "[2001:db8::1]:8787", "[fe80::1]:8787", "[fe00::1]:8787"] {
            XCTAssertEqual(ListenAddress.scope(pub), .failure(.notPrivate), pub)
        }
        XCTAssertEqual(ListenAddress.scope("mac.local:8787"), .failure(.notAnIP))
        XCTAssertEqual(ListenAddress.scope("010.0.0.1:8787"), .failure(.notAnIP)) // Go rejects leading zeros
        XCTAssertEqual(ListenAddress.scope("[fe80::1%en0]:8787"), .failure(.notAnIP))
        XCTAssertEqual(ListenAddress.scope("192.168.0.10"), .failure(.format))
        XCTAssertEqual(ListenAddress.scope("fd00::1:8787"), .failure(.format)) // IPv6 needs brackets
        XCTAssertEqual(ListenAddress.scope("192.168.0.10:"), .failure(.port))
        XCTAssertEqual(ListenAddress.scope("192.168.0.10:70000"), .failure(.port))
        XCTAssertEqual(ListenAddress.scope("192.168.0.10:0"), .failure(.port))
    }

    /// The same file server/listen_shared_test.go checks against scopeOf.
    func testSharedFixtureWithServer() throws {
        let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Fixtures/listen_scope_cases.json")
        let obj = try JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any]
        let cases = try XCTUnwrap(obj?["cases"] as? [[String: String]])
        XCTAssertGreaterThan(cases.count, 20)
        for c in cases {
            let addr = try XCTUnwrap(c["addr"])
            let got: String
            switch ListenAddress.scope(addr) {
            case .success(.loopback): got = "loopback"
            case .success(.privateLAN): got = "private"
            case .success(.wildcard): got = "wildcard"
            case .failure: got = "refused"
            }
            XCTAssertEqual(got, c["scope"], addr)
        }
    }

    func testValidateLAN() {
        XCTAssertEqual(try ListenAddress.validateLAN("192.168.0.10:8787").get(), .privateLAN)
        XCTAssertEqual(try ListenAddress.validateLAN("[fd12:3456::1]:8787").get(), .privateLAN)
        XCTAssertEqual(try ListenAddress.validateLAN("0.0.0.0:8787").get(), .wildcard)
        XCTAssertEqual(ListenAddress.validateLAN("127.0.0.1:8787"), .failure(.notPrivate))
        XCTAssertEqual(ListenAddress.validateLAN("localhost:8787"), .failure(.format)) // the script refuses names
        XCTAssertEqual(ListenAddress.validateLAN("203.0.113.5:8787"), .failure(.notPrivate))
        // Not accepted by the script even though the server would parse it.
        XCTAssertEqual(ListenAddress.validateLAN(":8787"), .failure(.format))
        XCTAssertEqual(ListenAddress.validateLAN("192.168.0.10:8787\n"), .failure(.format))
    }

    func testCandidates() {
        let ips = ["fe80::1", "192.168.0.10", "8.8.8.8", "fd00::5", "10.0.0.2", "192.168.0.10", "100.64.0.1"]
            .compactMap(IPAddress.init)
        XCTAssertEqual(ListenAddress.lanCandidates(ips).map(\.description), ["192.168.0.10", "10.0.0.2", "fd00::5"])
        XCTAssertEqual(ListenAddress.join(IPAddress("fd00::5")!, port: 8787), "[fd00::5]:8787")
        XCTAssertEqual(ListenAddress.join(IPAddress("10.0.0.2")!, port: 9000), "10.0.0.2:9000")
        for c in ListenAddress.lanCandidates(ips) {
            XCTAssertEqual(try ListenAddress.validateLAN(ListenAddress.join(c, port: 8787)).get(), .privateLAN)
        }
    }
}
