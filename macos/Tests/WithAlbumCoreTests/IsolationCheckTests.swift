import XCTest
@testable import WithAlbumCore

final class IsolationCheckTests: XCTestCase {
    func testTargets() {
        guard case .success(let t) = IsolationCheck.parseLine("홈캠 거실 192.168.0.30:554") else { return XCTFail() }
        XCTAssertEqual(t.label, "홈캠 거실")
        XCTAssertEqual(t.address, "192.168.0.30:554")
        guard case .success(let ha) = IsolationCheck.parseLine("[fd00::8]:8123") else { return XCTFail() }
        XCTAssertEqual(ha.label, "[fd00::8]:8123")
        for (line, want) in [("router 10.0.0.1:80", nil), ("169.254.10.2:80", nil),
                             ("cam 8.8.8.8:443", IsolationCheck.TargetError.notPrivate), ("127.0.0.1:8787", .notPrivate),
                             ("[::1]:80", .notPrivate), ("224.0.0.1:80", .notPrivate), ("0.0.0.0:80", .notPrivate),
                             ("homeassistant.local:8123", .notAnIP), ("192.168.0.30", .format), ("192.168.0.30:0", .port),
                             ("192.168.0.30:70000", .port), ("192.168.0.30:8a", .port), ("", .format), ("[fe80::1%en0]:80", .notAnIP),
                             ("172.32.0.1:80", .notPrivate), ("172.31.0.1:80", nil)] as [(String, IsolationCheck.TargetError?)] {
            switch IsolationCheck.parseLine(line) {
            case .success: XCTAssertNil(want, line)
            case .failure(let e): XCTAssertEqual(e, want, line)
            }
        }
    }

    func testParseText() {
        let text = "# 점검할 기기\n홈캠 192.168.0.30:554\n\nHA 192.168.0.40:8123\n홈캠 다시 192.168.0.30:554\nbad 1.1.1.1:53\n"
        let r = IsolationCheck.parse(text)
        XCTAssertEqual(r.targets.map(\.address), ["192.168.0.30:554", "192.168.0.40:8123"])
        XCTAssertEqual(r.errors.count, 1)
        XCTAssertEqual(r.errors.first?.error, .notPrivate)
        let many = (1...25).map { "192.168.1.\($0):80" }.joined(separator: "\n")
        let m = IsolationCheck.parse(many)
        XCTAssertEqual(m.targets.count, IsolationCheck.maxTargets)
        XCTAssertEqual(m.errors.count, 5)
        XCTAssertTrue(m.errors.allSatisfy { $0.error == .tooMany })
    }

    /// The probe itself, against this Mac's loopback (the validation above
    /// keeps users from choosing loopback; the probe does not care).
    func testProbe() throws {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        XCTAssertGreaterThanOrEqual(fd, 0)
        defer { close(fd) }
        var sa = sockaddr_in()
        sa.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        sa.sin_family = sa_family_t(AF_INET)
        sa.sin_addr.s_addr = inet_addr("127.0.0.1")
        sa.sin_port = 0
        _ = withUnsafePointer(to: &sa) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        XCTAssertEqual(Darwin.listen(fd, 4), 0)
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &sa) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) } }
        let port = Int(UInt16(bigEndian: sa.sin_port))
        let lo = try XCTUnwrap(IPAddress("127.0.0.1"))
        XCTAssertEqual(IsolationCheck.probe(lo, port: port, timeout: 1), .open)
        close(fd)
        XCTAssertEqual(IsolationCheck.probe(lo, port: port, timeout: 1), .refused)
        // A TEST-NET-1 address nobody answers for: blocked (timeout or no route).
        let start = Date()
        let none = IsolationCheck.probe(try XCTUnwrap(IPAddress("192.0.2.1")), port: 9, timeout: 0.5)
        XCTAssertTrue(none.blocked, "\(none)")
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
    }

    func testVerdict() {
        XCTAssertNil(IsolationCheck.verdict([]))
        XCTAssertEqual(IsolationCheck.verdict([.timedOut, .unreachable(EHOSTUNREACH)]), .isolated)
        XCTAssertEqual(IsolationCheck.verdict([.timedOut, .refused]), .notIsolated)
        XCTAssertEqual(IsolationCheck.verdict([.open]), .notIsolated)
        XCTAssertEqual(IsolationCheck.verdict([.timedOut, .failed(EMFILE)]), .inconclusive)
        XCTAssertEqual(IsolationCheck.Outcome.open.title, "열림")
        XCTAssertEqual(IsolationCheck.Outcome.timedOut.title, "막힘")
    }
}
