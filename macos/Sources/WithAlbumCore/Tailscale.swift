import Foundation

/// Tailscale Funnel helper logic (docs/FUNNEL.md): find the tailscale CLI,
/// read its status, build the funnel commands and explain failures. No
/// process is started here; the app runs the commands, and only when the
/// user presses a button.
///
/// The server's funnel listener is a loopback TLS port; Funnel must forward
/// raw TCP to it (`--tcp`), so the app's pinned TLS session with the
/// server's own key ends in the server, not in tailscaled.
public enum Funnel {
    /// WITHALBUM_FUNNEL_LISTEN the app configures (server default).
    public static let listen = "127.0.0.1:8788"
    /// Funnel's public ports.
    public static let ports = [443, 8443, 10000]
    public static let adminConsole = URL(string: "https://login.tailscale.com/admin")!
    public static let policyPage = URL(string: "https://login.tailscale.com/admin/acls/file")!
    public static let dnsPage = URL(string: "https://login.tailscale.com/admin/dns")!
    public static let docs = URL(string: "https://tailscale.com/docs/features/tailscale-funnel")!

    /// `tailscale funnel --bg --yes --tcp=<port> --proxy-protocol=2 tcp://<listen>`
    public static func enableArguments(port: Int = 443, target: String = listen) -> [String] {
        ["funnel", "--bg", "--yes", "--tcp=\(port)", "--proxy-protocol=2", "tcp://\(target)"]
    }

    /// The same forwarder, turned off (`... off`), leaving any other
    /// serve/funnel configuration of the machine alone.
    public static func disableArguments(port: Int = 443, target: String = listen) -> [String] {
        ["funnel", "--tcp=\(port)", "tcp://\(target)", "off"]
    }

    public static let statusArguments = ["status", "--json"]
    public static let funnelStatusArguments = ["funnel", "status", "--json"]

    /// https://<name>.ts.net[:port] from the machine's MagicDNS name
    /// (`Self.DNSName`, with or without the final dot). nil for anything
    /// that is not a ts.net name the server would accept.
    public static func publicURL(dnsName: String, port: Int = 443) -> String? {
        var host = dnsName.lowercased()
        if host.hasSuffix(".") { host.removeLast() }
        let labels = host.split(separator: ".", omittingEmptySubsequences: false)
        guard host.hasSuffix(".ts.net"), labels.count >= 4, host.count <= 253, ports.contains(port) else { return nil }
        for l in labels {
            guard !l.isEmpty, l.count <= 63, l.first != "-", l.last != "-",
                  l.allSatisfy({ ("a"..."z").contains($0) || $0.isASCIIDigit || $0 == "-" }) else { return nil }
        }
        return port == 443 ? "https://\(host)" : "https://\(host):\(port)"
    }

    /// Whether a WITHALBUM_FUNNEL_URL value is one the server accepts
    /// (the same rules as server/funnel.go parseFunnelURL, canonical form).
    public static func isValidURL(_ s: String) -> Bool {
        guard let u = URLComponents(string: s), u.scheme == "https", let host = u.host,
              u.user == nil, u.password == nil, u.query == nil, u.fragment == nil, u.path.isEmpty || u.path == "/" else { return false }
        return publicURL(dnsName: host, port: u.port ?? 443) == s
    }
}

/// Where the tailscale CLI is and which macOS variant it belongs to
/// (https://tailscale.com/docs/concepts/macos-variants).
public struct TailscaleCLI: Equatable, Sendable {
    public enum Variant: Equatable, Sendable {
        case appStore       // Mac App Store app (CLI = the app's executable)
        case standalone     // Standalone app from tailscale.com
        case openSource     // tailscaled + tailscale from source / Homebrew
    }

    public var path: String
    public var variant: Variant

    /// Tailscale's variant table lists Funnel only for the open source
    /// tailscaled; the GUI variants may refuse it. The app still lets the
    /// user try and shows the CLI's answer.
    public var funnelListedAsSupported: Bool { variant == .openSource }

    public var variantName: String {
        switch variant {
        case .appStore: return "Mac App Store 앱"
        case .standalone: return "Standalone 앱(tailscale.com 배포)"
        case .openSource: return "오픈소스 tailscaled(Homebrew 등)"
        }
    }

    public static let appExecutable = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
    public static let appReceipt = "/Applications/Tailscale.app/Contents/_MASReceipt/receipt"
    /// Where an open source build (Homebrew on Apple silicon / Intel, or
    /// `go install` to a system path) puts the CLI.
    public static let openSourcePaths = ["/opt/homebrew/bin/tailscale", "/usr/local/bin/tailscale"]

    /// Finds the CLI. An open source CLI is preferred (it supports Funnel);
    /// a /usr/local/bin/tailscale that is a link into Tailscale.app (the
    /// GUI apps can install one) counts as the app's variant.
    /// - Parameters:
    ///   - isExecutable: FileManager.isExecutableFile in the app.
    ///   - resolve: the target of a symbolic link, or the path itself.
    public static func detect(isExecutable: (String) -> Bool, fileExists: (String) -> Bool,
                              resolve: (String) -> String) -> TailscaleCLI? {
        let appVariant: Variant = fileExists(appReceipt) ? .appStore : .standalone
        for p in openSourcePaths where isExecutable(p) {
            if resolve(p).hasPrefix("/Applications/Tailscale.app/") { continue }
            return TailscaleCLI(path: p, variant: .openSource)
        }
        if isExecutable(appExecutable) { return TailscaleCLI(path: appExecutable, variant: appVariant) }
        return nil
    }
}

/// The parts of `tailscale status --json` the app needs.
public struct TailscaleStatus: Equatable, Sendable {
    public var backendState: String      // "Running", "NeedsLogin", "Stopped", …
    public var dnsName: String?          // Self.DNSName without the final dot
    public var magicDNS: Bool?           // CurrentTailnet.MagicDNSEnabled
    /// nil when the status does not list capabilities at all.
    public var funnelAllowed: Bool?
    public var httpsAllowed: Bool?

    public var running: Bool { backendState == "Running" }

    public static func parse(_ data: Data) -> TailscaleStatus? {
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let state = obj["BackendState"] as? String else { return nil }
        var s = TailscaleStatus(backendState: state)
        let me = obj["Self"] as? [String: Any]
        if var dns = me?["DNSName"] as? String, !dns.isEmpty {
            if dns.hasSuffix(".") { dns.removeLast() }
            s.dnsName = dns.lowercased()
        }
        s.magicDNS = (obj["CurrentTailnet"] as? [String: Any])?["MagicDNSEnabled"] as? Bool
        // Newer versions: Self.CapMap {"funnel": null, "https": null, …};
        // older: Self.Capabilities ["funnel", "https", …]. Either may use
        // the long "https://tailscale.com/cap/…" form.
        var caps: [String]?
        if let m = me?["CapMap"] as? [String: Any] { caps = Array(m.keys) }
        if let list = me?["Capabilities"] as? [String] { caps = (caps ?? []) + list }
        if let caps {
            func has(_ name: String) -> Bool {
                caps.contains { $0 == name || $0 == "https://tailscale.com/cap/\(name)" }
            }
            s.funnelAllowed = has("funnel")
            s.httpsAllowed = has("https")
        }
        return s
    }

    public init(backendState: String, dnsName: String? = nil, magicDNS: Bool? = nil,
                funnelAllowed: Bool? = nil, httpsAllowed: Bool? = nil) {
        self.backendState = backendState
        self.dnsName = dnsName
        self.magicDNS = magicDNS
        self.funnelAllowed = funnelAllowed
        self.httpsAllowed = httpsAllowed
    }
}

/// `tailscale funnel status --json`: the node's ServeConfig
/// (tailscale ipn.ServeConfig: TCP, Web, AllowFunnel).
public struct FunnelServeStatus: Equatable, Sendable {
    public struct TCPHandler: Equatable, Sendable {
        public var tcpForward: String?
        public var terminateTLS: String?
        public var https: Bool
        public var proxyProtocol: Int
    }
    public var tcp: [Int: TCPHandler]
    public var allowFunnel: Set<String>   // "host:port" entries set to true

    public static func parse(_ data: Data) -> FunnelServeStatus? {
        // An empty configuration prints {} (or nothing on some versions).
        let trimmed = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return FunnelServeStatus(tcp: [:], allowFunnel: []) }
        guard let obj = try? JSONSerialization.jsonObject(with: Data(trimmed.utf8)) as? [String: Any] else { return nil }
        var tcp: [Int: TCPHandler] = [:]
        for (k, v) in obj["TCP"] as? [String: Any] ?? [:] {
            guard let port = Int(k), let h = v as? [String: Any] else { continue }
            tcp[port] = TCPHandler(tcpForward: h["TCPForward"] as? String, terminateTLS: h["TerminateTLS"] as? String,
                                   https: h["HTTPS"] as? Bool ?? false, proxyProtocol: h["ProxyProtocol"] as? Int ?? 0)
        }
        var allow = Set<String>()
        for (k, v) in obj["AllowFunnel"] as? [String: Any] ?? [:] where (v as? Bool) == true { allow.insert(k.lowercased()) }
        return FunnelServeStatus(tcp: tcp, allowFunnel: allow)
    }

    public enum State: Equatable, Sendable {
        case off                     // nothing on the port
        case on(proxyProtocol: Bool) // raw TCP to the server, public
        case tailnetOnly             // forwarder there but not public (serve, not funnel)
        case terminatesTLS           // forwarder decrypts: not the supported mode
        case otherTarget(String)     // the port forwards somewhere else
        case webServer               // the port serves HTTPS (tailscale serve/funnel <port>)
    }

    /// What the public port of host does for the server listener target.
    public func state(host: String, port: Int = 443, target: String = Funnel.listen) -> State {
        guard let h = tcp[port] else { return .off }
        if h.https { return .webServer }
        guard let fwd = h.tcpForward, !fwd.isEmpty else { return .off }
        let normalized = fwd.hasPrefix("tcp://") ? String(fwd.dropFirst(6)) : fwd
        guard normalized == target || normalized == target.replacingOccurrences(of: "127.0.0.1", with: "localhost") else {
            return .otherTarget(fwd)
        }
        if let t = h.terminateTLS, !t.isEmpty { return .terminatesTLS }
        guard allowFunnel.contains("\(host.lowercased()):\(port)") else { return .tailnetOnly }
        return .on(proxyProtocol: h.proxyProtocol > 0)
    }
}

/// Why a tailscale command failed, in the user's terms.
public enum TailscaleProblem: Equatable, Sendable {
    case notInstalled
    case notRunning            // tailscaled not reachable
    case loggedOut
    case funnelNotAllowed      // tailnet policy lacks the funnel node attribute
    case httpsDisabled         // HTTPS certificates / MagicDNS off
    case variantUnsupported    // this macOS variant cannot do it
    case other(String)

    /// Classifies a failed command's output.
    public static func classify(_ output: String) -> TailscaleProblem {
        let o = output.lowercased()
        if o.contains("failed to connect to local tailscale") || o.contains("is tailscale running") || o.contains("tailscaled") && o.contains("not running") {
            return .notRunning
        }
        if o.contains("needslogin") || o.contains("not logged in") || o.contains("logged out") { return .loggedOut }
        if o.contains("funnel") && (o.contains("not enabled") || o.contains("not available") || o.contains("nodeattr") ||
                                    o.contains("access controls") || o.contains("policy") || o.contains("/f/funnel")) {
            return .funnelNotAllowed
        }
        if o.contains("https") && (o.contains("not enabled") || o.contains("disabled") || o.contains("/f/serve")) { return .httpsDisabled }
        if o.contains("sandbox") || o.contains("not supported on this platform") || o.contains("unsupported") { return .variantUnsupported }
        return .other(output.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// The first https://login.tailscale.com/… link in the output (the CLI
    /// prints one to enable a missing feature), if any.
    public static func adminLink(in output: String) -> URL? {
        for token in output.split(whereSeparator: { $0.isWhitespace || $0 == "\"" || $0 == "'" }) {
            var t = String(token)
            while let last = t.last, ".,;:)".contains(last) { t.removeLast() }
            if t.hasPrefix("https://login.tailscale.com/"), let u = URL(string: t), u.host == "login.tailscale.com" { return u }
        }
        return nil
    }

    public var message: String {
        switch self {
        case .notInstalled: return "이 Mac에 Tailscale이 없어요. 오픈소스 tailscaled(Homebrew: brew install tailscale)를 설치하고 로그인하세요."
        case .notRunning: return "Tailscale이 실행 중이 아니에요. Tailscale을 켜고(오픈소스: sudo brew services start tailscale) 다시 확인하세요."
        case .loggedOut: return "Tailscale에 로그인되어 있지 않아요. 이 Mac에서 tailscale up(또는 앱의 로그인)을 먼저 하세요."
        case .funnelNotAllowed: return "tailnet 정책에서 Funnel이 꺼져 있어요. 관리 콘솔 → Access controls에서 funnel 노드 속성을 허용하세요."
        case .httpsDisabled: return "tailnet에서 HTTPS 인증서(또는 MagicDNS)가 꺼져 있어요. 관리 콘솔 → DNS에서 MagicDNS와 HTTPS Certificates를 켜세요."
        case .variantUnsupported: return "이 Tailscale 설치 방식(앱 스토어/Standalone)은 Funnel을 지원하지 않을 수 있어요. 오픈소스 tailscaled로 바꾸세요."
        case .other(let s): return s.isEmpty ? "tailscale 명령이 실패했어요." : "tailscale: \(s.prefix(300))"
        }
    }
}
