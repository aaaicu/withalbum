import Foundation

/// "내부망 격리 점검" (docs/HOME_NETWORK_ISOLATION.md): from this Mac, try
/// one TCP connection to each address the user typed (a home camera, Home
/// Assistant, the router's admin page) and report whether it is blocked.
/// Only the typed targets are tried, once each, with a short timeout: no
/// scanning, no name lookups, nothing stored or sent anywhere.
public enum IsolationCheck {
    public static let maxTargets = 20
    public static let timeout: TimeInterval = 2

    public struct Target: Equatable, Hashable, Sendable {
        public var label: String
        public var ip: IPAddress
        public var port: Int
        public var address: String { ListenAddress.join(ip, port: port) }
    }

    public enum TargetError: Error, Equatable, Sendable {
        case format       // not IP:port
        case notAnIP      // a host name (not resolved on purpose)
        case port
        case notPrivate   // loopback, public, multicast …: only home network addresses
        case tooMany
    }

    /// Private home-network addresses: RFC 1918 and fc00::/7 (as the
    /// server's LAN rule), plus IPv4 link-local 169.254/16 (devices without
    /// DHCP). This Mac's own loopback and every public address are refused.
    public static func isHomeNetwork(_ ip: IPAddress) -> Bool {
        if ip.isPrivate { return true }
        return ip.isIPv4 && ip.bytes[0] == 169 && ip.bytes[1] == 254
    }

    /// One line: "[label] IP:port" (label optional, IPv6 in brackets).
    public static func parseLine(_ line: String) -> Result<Target, TargetError> {
        let parts = line.trimmingCharacters(in: .whitespaces).split(separator: " ", omittingEmptySubsequences: true)
        guard let addr = parts.last.map(String.init) else { return .failure(.format) }
        let label = parts.dropLast().joined(separator: " ")
        guard let (host, portText) = ListenAddress.splitHostPort(addr), !host.isEmpty else { return .failure(.format) }
        guard portText.allSatisfy(\.isASCIIDigit), let port = Int(portText), (1...65535).contains(port) else { return .failure(.port) }
        guard let ip = IPAddress(host) else { return .failure(.notAnIP) }
        guard isHomeNetwork(ip) else { return .failure(.notPrivate) }
        return .success(Target(label: label.isEmpty ? addr : label, ip: ip, port: port))
    }

    /// Every non-empty line of text, in order, without duplicates.
    public static func parse(_ text: String) -> (targets: [Target], errors: [(line: String, error: TargetError)]) {
        var targets: [Target] = [], errors: [(String, TargetError)] = [], seen = Set<String>()
        for raw in text.split(whereSeparator: \.isNewline) {
            let line = raw.trimmingCharacters(in: .whitespaces)
            if line.isEmpty || line.hasPrefix("#") { continue }
            switch parseLine(line) {
            case .success(let t):
                guard seen.insert(t.address).inserted else { continue }
                if targets.count >= maxTargets { errors.append((line, .tooMany)); continue }
                targets.append(t)
            case .failure(let e): errors.append((line, e))
            }
        }
        return (targets, errors)
    }

    public static func message(_ e: TargetError) -> String {
        switch e {
        case .format: return "IP:포트 형식으로 적어 주세요 (예: 홈캠 192.168.0.30:554)"
        case .notAnIP: return "이름이 아닌 IP 주소를 적어 주세요 (이름은 일부러 찾아보지 않아요)"
        case .port: return "포트는 1~65535 사이의 숫자여야 해요"
        case .notPrivate: return "집 안 사설 주소(192.168.x.x, 10.x.x.x, 172.16~31.x.x, 169.254.x.x, fd00::/8)만 점검해요"
        case .tooMany: return "한 번에 \(maxTargets)개까지만 점검해요"
        }
    }

    /// The result of one connection attempt.
    public enum Outcome: Equatable, Sendable {
        case open                  // connected: NOT isolated
        case refused               // the device answered (RST): the path is open, the port closed
        case timedOut              // no answer: blocked (or the device is off)
        case unreachable(Int32)    // no route / host down / denied by macOS: errno
        case failed(Int32)

        /// Whether this target counts as blocked from this Mac.
        public var blocked: Bool {
            switch self {
            case .timedOut, .unreachable: return true
            case .open, .refused, .failed: return false
            }
        }

        public var title: String {
            switch self {
            case .open: return "열림"
            case .refused: return "길 열림(포트 닫힘)"
            case .timedOut: return "막힘"
            case .unreachable: return "막힘(경로 없음)"
            case .failed: return "확인 못 함"
            }
        }

        public var detail: String {
            switch self {
            case .open: return "이 Mac에서 접속돼요. 격리가 안 된 상태예요."
            case .refused: return "기기가 응답했어요(포트는 닫힘). 네트워크 길은 열려 있으니 다른 포트는 접속될 수 있어요."
            case .timedOut: return "\(Int(IsolationCheck.timeout))초 안에 응답이 없어요. 차단되었거나 기기가 꺼져 있어요."
            case .unreachable(let e):
                return "경로가 없어요(\(String(cString: strerror(e)))). 차단된 것일 수도, macOS 로컬 네트워크 권한이 꺼져 있는 것일 수도 있어요."
            case .failed(let e): return "접속을 시도하지 못했어요(\(String(cString: strerror(e))))."
            }
        }
    }

    /// One TCP connect to ip:port with a timeout (non-blocking connect +
    /// poll). Nothing is sent; the socket is closed right away.
    public static func probe(_ ip: IPAddress, port: Int, timeout: TimeInterval = IsolationCheck.timeout) -> Outcome {
        let fd = socket(ip.isIPv4 ? AF_INET : AF_INET6, SOCK_STREAM, IPPROTO_TCP)
        if fd < 0 { return .failed(errno) }
        defer { close(fd) }
        var on: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        let flags = fcntl(fd, F_GETFL, 0)
        _ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)
        var rc: Int32
        if ip.isIPv4 {
            var sa = sockaddr_in()
            sa.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
            sa.sin_family = sa_family_t(AF_INET)
            sa.sin_port = in_port_t(UInt16(port).bigEndian)
            withUnsafeMutableBytes(of: &sa.sin_addr) { $0.copyBytes(from: ip.bytes) }
            rc = withUnsafePointer(to: &sa) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        } else {
            var sa = sockaddr_in6()
            sa.sin6_len = UInt8(MemoryLayout<sockaddr_in6>.size)
            sa.sin6_family = sa_family_t(AF_INET6)
            sa.sin6_port = in_port_t(UInt16(port).bigEndian)
            withUnsafeMutableBytes(of: &sa.sin6_addr) { $0.copyBytes(from: ip.bytes) }
            rc = withUnsafePointer(to: &sa) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in6>.size)) } }
        }
        if rc == 0 { return .open }
        if errno != EINPROGRESS { return classify(errno) }
        var p = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
        let n = poll(&p, 1, Int32(timeout * 1000))
        if n == 0 { return .timedOut }
        if n < 0 { return .failed(errno) }
        var err: Int32 = 0
        var len = socklen_t(MemoryLayout<Int32>.size)
        if getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &len) != 0 { return .failed(errno) }
        return err == 0 ? .open : classify(err)
    }

    static func classify(_ e: Int32) -> Outcome {
        switch e {
        case ECONNREFUSED: return .refused
        case ETIMEDOUT: return .timedOut
        case EHOSTUNREACH, ENETUNREACH, EHOSTDOWN, ENETDOWN, EACCES, EPERM: return .unreachable(e)
        default: return .failed(e)
        }
    }

    /// The overall verdict for a finished check.
    public enum Verdict: Equatable, Sendable {
        case isolated          // every target blocked
        case notIsolated       // at least one target open or answering
        case inconclusive      // a target could not be tried
    }

    public static func verdict(_ outcomes: [Outcome]) -> Verdict? {
        guard !outcomes.isEmpty else { return nil }
        if outcomes.contains(where: { if case .failed = $0 { return true } else { return false } }) { return .inconclusive }
        return outcomes.allSatisfy(\.blocked) ? .isolated : .notIsolated
    }
}
