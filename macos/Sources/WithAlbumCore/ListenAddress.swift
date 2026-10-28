import Foundation

/// Listen address rules, kept identical to the server and the developer
/// install script:
///
///   - scripts/install-macos.sh --lan accepts `^[][0-9A-Fa-f.:]+:[0-9]+$`;
///   - server/listen.go scopeOf: host:port (IPv6 in brackets), "localhost"
///     or a loopback IP is loopback, an empty or unspecified host is the
///     wildcard, any other host must be an IP address and private
///     (RFC 1918 IPv4 or fc00::/7 IPv6, Go's net.IP.IsPrivate).
///
/// The server still makes the final decision (-allow-lan, -tls and an
/// administrator who can log in); the app never relaxes these rules.
public enum ListenScope: Equatable, Sendable {
    case loopback
    case privateLAN
    case wildcard
}

public enum ListenAddressError: Error, Equatable, Sendable {
    case format          // not ADDR:PORT as the install script expects
    case port            // missing, non-numeric or out of range
    case notAnIP         // a non-loopback host name
    case notPrivate      // a public (internet) address
}

/// An IP address parsed like Go's net.ParseIP (no zones, IPv4-mapped IPv6
/// is treated as IPv4).
public struct IPAddress: Equatable, Hashable, Sendable, CustomStringConvertible {
    /// 4 bytes for IPv4 (including IPv4-mapped IPv6), 16 for IPv6.
    public let bytes: [UInt8]

    public init?(_ text: String) {
        guard !text.isEmpty, !text.contains("%") else { return nil }
        if !text.contains(":") {
            // Go: four decimal octets 0-255, no leading zeros (inet_pton
            // would accept "010.0.0.1").
            let parts = text.split(separator: ".", omittingEmptySubsequences: false)
            guard parts.count == 4 else { return nil }
            var b: [UInt8] = []
            for p in parts {
                guard !p.isEmpty, p.count <= 3, p.allSatisfy(\.isASCIIDigit),
                      p.count == 1 || p.first != "0", let v = UInt8(p) else { return nil }
                b.append(v)
            }
            bytes = b
            return
        }
        var v6 = in6_addr()
        guard inet_pton(AF_INET6, text, &v6) == 1 else { return nil }
        let b = withUnsafeBytes(of: &v6) { Array($0) }
        if b[0..<10].allSatisfy({ $0 == 0 }) && b[10] == 0xff && b[11] == 0xff {
            bytes = Array(b[12..<16])
        } else {
            bytes = b
        }
    }

    public init?(bytes: [UInt8]) {
        guard bytes.count == 4 || bytes.count == 16 else { return nil }
        self.bytes = bytes
    }

    public var isIPv4: Bool { bytes.count == 4 }

    public var isLoopback: Bool {
        isIPv4 ? bytes[0] == 127 : bytes == [UInt8](repeating: 0, count: 15) + [1]
    }

    public var isUnspecified: Bool { bytes.allSatisfy { $0 == 0 } }

    /// Go net.IP.IsPrivate: 10/8, 172.16/12, 192.168/16, fc00::/7.
    public var isPrivate: Bool {
        if isIPv4 {
            return bytes[0] == 10
                || (bytes[0] == 172 && bytes[1] & 0xf0 == 16)
                || (bytes[0] == 192 && bytes[1] == 168)
        }
        return bytes[0] & 0xfe == 0xfc
    }

    public var description: String {
        var out = [CChar](repeating: 0, count: Int(INET6_ADDRSTRLEN))
        var b = bytes
        let family = isIPv4 ? AF_INET : AF_INET6
        _ = inet_ntop(family, &b, &out, socklen_t(out.count))
        return String(cString: out)
    }
}

public enum ListenAddress {
    public static let defaultPort = 8787
    public static let loopbackDefault = "127.0.0.1:8787"

    /// The --lan pattern of scripts/install-macos.sh.
    public static func scriptAccepts(_ addr: String) -> Bool {
        addr.range(of: #"^[\]\[0-9A-Fa-f.:]+:[0-9]+$"#, options: .regularExpression) != nil
    }

    /// Go net.SplitHostPort for the forms the server accepts.
    public static func splitHostPort(_ addr: String) -> (host: String, port: String)? {
        if addr.hasPrefix("[") {
            guard let close = addr.firstIndex(of: "]") else { return nil }
            let host = String(addr[addr.index(after: addr.startIndex)..<close])
            let rest = addr[addr.index(after: close)...]
            guard rest.hasPrefix(":") else { return nil }
            let port = String(rest.dropFirst())
            if port.contains(":") || port.contains("[") || port.contains("]") { return nil }
            return (host, port)
        }
        guard let colon = addr.lastIndex(of: ":") else { return nil }
        let host = String(addr[..<colon])
        if host.contains(":") || host.contains("[") || host.contains("]") { return nil }
        let port = String(addr[addr.index(after: colon)...])
        if port.contains("[") || port.contains("]") { return nil }
        return (host, port)
    }

    /// The server's classification (server/listen.go scopeOf), with the
    /// port checked as a number 1-65535 as net.Listen would.
    public static func scope(_ addr: String) -> Result<ListenScope, ListenAddressError> {
        guard let (host, port) = splitHostPort(addr) else { return .failure(.format) }
        guard let p = Int(port), port.allSatisfy(\.isASCIIDigit), (1...65535).contains(p) else {
            return .failure(.port)
        }
        if host == "localhost" { return .success(.loopback) }
        if host.isEmpty { return .success(.wildcard) }
        guard let ip = IPAddress(host) else { return .failure(.notAnIP) }
        if ip.isLoopback { return .success(.loopback) }
        if ip.isUnspecified { return .success(.wildcard) }
        if ip.isPrivate { return .success(.privateLAN) }
        return .failure(.notPrivate)
    }

    /// An address the app may pass as WITHALBUM_LISTEN together with
    /// -tls -allow-lan: accepted by the install script's --lan pattern and
    /// by the server as a LAN address (private or, for an agent the app
    /// takes over, the wildcard it already had).
    public static func validateLAN(_ addr: String) -> Result<ListenScope, ListenAddressError> {
        guard scriptAccepts(addr) else {
            if case .failure(let e) = scope(addr), e == .notAnIP { return .failure(.notAnIP) }
            return .failure(.format)
        }
        switch scope(addr) {
        case .failure(let e): return .failure(e)
        case .success(.loopback): return .failure(.notPrivate)
        case .success(let s): return .success(s)
        }
    }

    /// host:port with IPv6 in brackets (Go net.JoinHostPort).
    public static func join(_ ip: IPAddress, port: Int) -> String {
        ip.isIPv4 ? "\(ip):\(port)" : "[\(ip)]:\(port)"
    }

    /// The private addresses of this Mac's interfaces a LAN listener may
    /// use, IPv4 first, without duplicates.
    public static func lanCandidates(_ addresses: [IPAddress]) -> [IPAddress] {
        var seen = Set<IPAddress>()
        let ok = addresses.filter { $0.isPrivate && seen.insert($0).inserted }
        return ok.filter(\.isIPv4) + ok.filter { !$0.isIPv4 }
    }

    /// Korean explanation of a validation error.
    public static func message(_ e: ListenAddressError) -> String {
        switch e {
        case .format: return "주소:포트 형식이 아니에요 (예: 192.168.0.10:8787, IPv6는 [fd00::10]:8787)"
        case .port: return "포트는 1~65535 사이의 숫자여야 해요"
        case .notAnIP: return "이름이 아닌 IP 주소를 입력하세요"
        case .notPrivate: return "이 Mac의 사설 LAN 주소만 쓸 수 있어요 (인터넷 공개는 지원하지 않아요)"
        }
    }
}

extension Character {
    var isASCIIDigit: Bool { ("0"..."9").contains(self) }
}
