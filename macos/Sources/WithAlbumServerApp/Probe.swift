import Foundation
import Security
import WithAlbumCore

/// Accepts exactly one server key: the P-256 SPKI SHA-256 that
/// `withalbum-server tls -json` read from this Mac's storage. Plain HTTP
/// (loopback) needs no trust decision.
final class PinnedTrust: NSObject, URLSessionDelegate {
    let pin: String?
    init(pin: String?) { self.pin = pin }

    static func leafPin(_ trust: SecTrust) -> String? {
        guard let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate], let leaf = chain.first,
              let key = SecCertificateCopyKey(leaf),
              let raw = SecKeyCopyExternalRepresentation(key, nil) as Data? else { return nil }
        return SPKIPin.sha256Hex(p256X963: raw)
    }

    static func decide(_ challenge: URLAuthenticationChallenge, pin: String?) -> (URLSession.AuthChallengeDisposition, URLCredential?) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust else {
            return (.performDefaultHandling, nil)
        }
        if let pin, leafPin(trust) == pin { return (.useCredential, URLCredential(trust: trust)) }
        return (.cancelAuthenticationChallenge, nil)
    }

    func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge,
                    completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
        let (d, c) = Self.decide(challenge, pin: pin)
        completionHandler(d, c)
    }
}

/// Probes the local server: /api/v1/health, and /setup when health is 404
/// (setup mode serves only the setup screen). Never uses a proxy.
enum Probe {
    struct Result {
        var health: Health?
        var setupPage = false
        var answered = false
    }

    static func run(base: URL, pin: String?) async -> Result {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.connectionProxyDictionary = [:]
        cfg.timeoutIntervalForRequest = 3
        cfg.timeoutIntervalForResource = 5
        cfg.requestCachePolicy = .reloadIgnoringLocalCacheData
        cfg.urlCache = nil
        cfg.httpCookieStorage = nil
        let session = URLSession(configuration: cfg, delegate: PinnedTrust(pin: pin), delegateQueue: nil)
        defer { session.finishTasksAndInvalidate() }
        var out = Result()
        guard let (data, resp) = try? await session.data(from: base.appendingPathComponent("api/v1/health")),
              let http = resp as? HTTPURLResponse else { return out }
        out.answered = true
        if http.statusCode == 200 {
            out.health = Health.parse(data)
            return out
        }
        if http.statusCode == 404, base.scheme == "http",
           let (_, r2) = try? await session.data(from: base.appendingPathComponent("setup")),
           let h2 = r2 as? HTTPURLResponse, h2.statusCode == 200 {
            out.setupPage = true
        }
        return out
    }
}

/// The IP addresses of this Mac's up, non-loopback interfaces.
enum Interfaces {
    static func addresses() -> [IPAddress] {
        var head: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&head) == 0, let first = head else { return [] }
        defer { freeifaddrs(head) }
        var out: [IPAddress] = []
        for p in sequence(first: first, next: { $0.pointee.ifa_next }) {
            let flags = Int32(p.pointee.ifa_flags)
            guard flags & IFF_UP != 0, flags & IFF_RUNNING != 0, flags & IFF_LOOPBACK == 0,
                  let sa = p.pointee.ifa_addr else { continue }
            switch Int32(sa.pointee.sa_family) {
            case AF_INET:
                var a = sa.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { $0.pointee.sin_addr }
                if let ip = IPAddress(bytes: withUnsafeBytes(of: &a) { Array($0) }) { out.append(ip) }
            case AF_INET6:
                var a = sa.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) { $0.pointee.sin6_addr }
                if let ip = IPAddress(bytes: withUnsafeBytes(of: &a) { Array($0) }) { out.append(ip) }
            default: continue
            }
        }
        return out
    }
}
