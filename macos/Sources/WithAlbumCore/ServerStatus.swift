import CryptoKit
import Foundation

/// GET /api/v1/health of a normally running server:
/// {"status":"ok","version":"…","storage":"ok|unavailable","integrity":"ok|warning"}.
/// In setup mode the server serves only the setup screen: /api/v1/health
/// is 404 and /setup is 200.
public struct Health: Equatable, Sendable {
    public var version: String
    public var storageOK: Bool
    public var integrityOK: Bool

    public static func parse(_ data: Data) -> Health? {
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              obj["status"] as? String == "ok",
              let storage = obj["storage"] as? String else { return nil }
        return Health(version: obj["version"] as? String ?? "",
                      storageOK: storage == "ok",
                      integrityOK: (obj["integrity"] as? String ?? "ok") == "ok")
    }
}

/// `launchctl print gui/<uid>/org.withalbum.server`, reduced to what the
/// menu needs.
public struct AgentRuntime: Equatable, Sendable {
    public var loaded: Bool
    public var running: Bool
    public var pid: Int?
    public var lastExitCode: String?
    public var path: String?

    public static let notLoaded = AgentRuntime(loaded: false, running: false)

    public init(loaded: Bool, running: Bool, pid: Int? = nil, lastExitCode: String? = nil, path: String? = nil) {
        self.loaded = loaded
        self.running = running
        self.pid = pid
        self.lastExitCode = lastExitCode
        self.path = path
    }

    /// Parses the output of a successful `launchctl print` (exit 0). Only
    /// the service's own top-level keys are read (one tab of indentation).
    public static func parsePrint(_ text: String) -> AgentRuntime {
        var r = AgentRuntime(loaded: true, running: false)
        for raw in text.split(separator: "\n", omittingEmptySubsequences: true) {
            guard raw.hasPrefix("\t"), !raw.hasPrefix("\t\t") else { continue }
            let line = raw.trimmingCharacters(in: .whitespaces)
            guard let eq = line.range(of: " = ") else { continue }
            let key = String(line[..<eq.lowerBound])
            let value = String(line[eq.upperBound...])
            switch key {
            case "state": r.running = value == "running"
            case "pid": r.pid = Int(value)
            case "last exit code": r.lastExitCode = value
            case "path": r.path = value
            default: break
            }
        }
        return r
    }
}

/// What the menu shows.
public enum ServerState: Equatable, Sendable {
    case notRegistered          // no agent and nothing answers
    case running(Health)        // storage ok (integrity may warn)
    case storageUnavailable(Health)
    case setupNeeded
    case starting               // agent running, not answering yet
    case failing                // agent loaded but the server keeps exiting
    case stopped                // agent plist exists, not loaded
    case unmanagedRunning       // something answers but no agent runs it

    /// Combines the agent state with the HTTP probe. `health` is the
    /// parsed health reply, `setupPage` whether /setup answered 200 (only
    /// probed when health was 404), `answered` whether anything answered.
    public static func combine(agentPlistExists: Bool, runtime: AgentRuntime,
                               health: Health?, setupPage: Bool, answered: Bool) -> ServerState {
        if let h = health {
            if !runtime.loaded { return .unmanagedRunning }
            return h.storageOK ? .running(h) : .storageUnavailable(h)
        }
        if setupPage { return runtime.loaded ? .setupNeeded : .unmanagedRunning }
        if answered && !runtime.loaded { return .unmanagedRunning }
        if runtime.loaded { return runtime.running ? .starting : .failing }
        return agentPlistExists ? .stopped : .notRegistered
    }

    public var title: String {
        switch self {
        case .notRegistered: return "서버가 아직 등록되지 않았어요"
        case .running(let h): return h.integrityOK ? "정상 실행 중" : "실행 중 · 무결성 경고"
        case .storageUnavailable: return "실행 중 · 저장 중단"
        case .setupNeeded: return "설치 필요 · 이 Mac에서 설치 화면을 여세요"
        case .starting: return "시작하는 중"
        case .failing: return "시작하지 못함 · 30초마다 다시 시도"
        case .stopped: return "멈춤"
        case .unmanagedRunning: return "실행 중 · 이 앱이 관리하지 않는 서버"
        }
    }

    public var detail: String? {
        switch self {
        case .storageUnavailable: return "저장장치를 연결하세요. 연결 전에는 사진을 받지 않아요."
        case .failing: return "저장장치 연결이나 LAN 조건을 확인하세요. 로그 폴더의 error.log에 이유가 있어요."
        case .running(let h) where !h.integrityOK: return "관리 화면에서 무결성 검사 결과를 확인하세요."
        case .unmanagedRunning: return "터미널이나 다른 방법으로 시작한 서버예요. 그 서버를 먼저 멈추세요."
        default: return nil
        }
    }

    public var version: String? {
        switch self {
        case .running(let h), .storageUnavailable(let h): return h.version
        default: return nil
        }
    }
}

/// The one-time setup code the server writes to its error log in setup
/// mode (internal/httpapi SetupBanner):
///   2026/10/20 09:30:00 SETUP: open http://127.0.0.1:8787/setup on this Mac and enter the setup code  ABCD-EFGH-JKLM
/// The app reads it from the local log only, to show and copy it.
public struct SetupCode: Equatable, Sendable {
    public var url: String
    public var code: String
    public var loggedAt: Date?
    public var expires: Date? { loggedAt?.addingTimeInterval(30 * 60) }

    /// The last code in the log text (the newest server start).
    public static func lastIn(_ log: String) -> SetupCode? {
        let pattern = #"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})?[^\n]*?SETUP: open (http://\S+) on this Mac and enter the setup code\s+([A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4})\s*$"#
        guard let re = try? NSRegularExpression(pattern: pattern, options: [.anchorsMatchLines]) else { return nil }
        let ns = log as NSString
        guard let m = re.matches(in: log, range: NSRange(location: 0, length: ns.length)).last else { return nil }
        var at: Date?
        if m.range(at: 1).location != NSNotFound {
            let f = DateFormatter()
            f.locale = Locale(identifier: "en_US_POSIX")
            f.timeZone = .current // Go's log uses local time
            f.dateFormat = "yyyy/MM/dd HH:mm:ss"
            at = f.date(from: ns.substring(with: m.range(at: 1)))
        }
        return SetupCode(url: ns.substring(with: m.range(at: 2)), code: ns.substring(with: m.range(at: 3)), loggedAt: at)
    }

    /// Only a loopback setup URL is shown (the server never serves setup
    /// elsewhere).
    public var isLoopback: Bool {
        guard let u = URL(string: url), let host = u.host else { return false }
        return host == "127.0.0.1" || host == "localhost" || host == "::1"
    }
}

/// The last line of the error log that explains a refusal to start.
public enum LogReason {
    public static func lastProblem(_ log: String) -> String? {
        let keys = ["SECURITY:", "SETUP:", "storage", "config file", "TLS:", "RECOVERY:", "address already in use"]
        for line in log.split(separator: "\n").reversed() {
            // A later successful start (or setup mode) ends the search.
            if line.contains("EXPERIMENTAL /") || line.contains("SETUP: open") || line.contains("SETUP: the code works") {
                return nil
            }
            if line.contains("TLS: created") || line.contains("TLS: re-issued") { continue }
            if keys.contains(where: { line.contains($0) }) { return String(line.prefix(400)) }
        }
        return nil
    }
}

/// `withalbum-server tls -json`.
public struct TLSPairing: Equatable, Sendable {
    public struct Entry: Equatable, Sendable {
        public var url: String
        public var payload: String
    }
    public var spkiSHA256: String
    public var certSHA256: String
    public var notAfter: String
    public var pairing: [Entry]

    public static func parse(_ data: Data) -> TLSPairing? {
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let spki = obj["spki_sha256"] as? String, isHex64(spki) else { return nil }
        let entries = (obj["pairing"] as? [[String: Any]] ?? []).compactMap { e -> Entry? in
            guard let u = e["url"] as? String, let p = e["payload"] as? String else { return nil }
            return Entry(url: u, payload: p)
        }
        return TLSPairing(spkiSHA256: spki.lowercased(), certSHA256: obj["cert_sha256"] as? String ?? "",
                          notAfter: obj["not_after"] as? String ?? "", pairing: entries)
    }

    /// AA:BB:… as the server prints it.
    public static func colons(_ hex: String) -> String {
        stride(from: 0, to: hex.count, by: 2).map { i -> String in
            let s = hex.index(hex.startIndex, offsetBy: i)
            return String(hex[s..<hex.index(s, offsetBy: min(2, hex.count - i))]).uppercased()
        }.joined(separator: ":")
    }

    static func isHex64(_ s: String) -> Bool {
        s.count == 64 && s.allSatisfy { $0.isHexDigit }
    }
}

/// The server's TLS key is ECDSA P-256 (internal/tlsid). Its pin is the
/// SHA-256 of the SubjectPublicKeyInfo DER.
public enum SPKIPin {
    /// `x963` is the uncompressed point 04‖X‖Y that
    /// SecKeyCopyExternalRepresentation returns for a P-256 public key.
    public static func sha256Hex(p256X963 x963: Data) -> String? {
        guard let key = try? P256.Signing.PublicKey(x963Representation: x963) else { return nil }
        return SHA256.hash(data: key.derRepresentation).map { String(format: "%02x", $0) }.joined()
    }
}

/// ~/Library/Application Support/WithAlbum/server.json written by the
/// setup screen: {"format":1,"storage_root":"/…","volume_id":"…"}.
public enum ServerConfigFile {
    public static func storageRoot(_ data: Data) -> String? {
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let root = obj["storage_root"] as? String, root.hasPrefix("/") else { return nil }
        return root
    }
}
