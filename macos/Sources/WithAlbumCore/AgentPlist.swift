import Foundation

/// The per-user LaunchAgent that runs the bundled server. Same label,
/// keys and log locations as scripts/install-macos.sh, so the two never
/// run side by side: there is one org.withalbum.server agent per user.
public struct AgentConfig: Equatable, Sendable {
    public static let label = "org.withalbum.server"
    public static let throttleInterval = 30

    /// Absolute path of the server executable.
    public var program: String
    /// -tls -allow-lan on `listen` (the install script's --lan).
    public var lan: Bool
    /// WITHALBUM_LISTEN.
    public var listen: String
    /// Other environment variables: WITHALBUM_ROOT, WITHALBUM_CONFIG and
    /// anything an operator added to an agent the app took over.
    public var environment: [String: String]
    /// Directory of server.log and error.log (~/Library/Logs/WithAlbum).
    public var logDirectory: String

    public init(program: String, lan: Bool = false, listen: String = ListenAddress.loopbackDefault,
                environment: [String: String] = [:], logDirectory: String) {
        self.program = program
        self.lan = lan
        self.listen = listen
        self.environment = environment
        self.logDirectory = logDirectory
    }

    public var arguments: [String] { lan ? [program, "-tls", "-allow-lan"] : [program] }

    /// The full environment of the agent (WITHALBUM_LISTEN always set).
    public var fullEnvironment: [String: String] {
        var env = environment
        env["WITHALBUM_LISTEN"] = listen
        return env
    }

    public var storageRoot: String? { environment["WITHALBUM_ROOT"].flatMap { $0.isEmpty ? nil : $0 } }
    public var configFile: String? { environment["WITHALBUM_CONFIG"].flatMap { $0.isEmpty ? nil : $0 } }
    public var errorLog: String { (logDirectory as NSString).appendingPathComponent("error.log") }
    public var serverLog: String { (logDirectory as NSString).appendingPathComponent("server.log") }

    public enum Invalid: Error, Equatable, Sendable {
        case newline(String)          // the install script refuses newlines too
        case relativePath(String)
        case listen(ListenAddressError)
        case loopbackOnlyWithoutLAN   // plain HTTP is loopback only
    }

    /// Checks the rules the install script and the server apply.
    public func validate() throws {
        let values = [program, listen, logDirectory] + environment.keys + environment.values
        for v in values where v.contains("\n") || v.contains("\r") || v.contains("\0") {
            throw Invalid.newline(v)
        }
        for p in [program, logDirectory] where !p.hasPrefix("/") { throw Invalid.relativePath(p) }
        if let root = storageRoot, !root.hasPrefix("/") { throw Invalid.relativePath(root) }
        if lan {
            if case .failure(let e) = ListenAddress.validateLAN(listen) { throw Invalid.listen(e) }
        } else {
            switch ListenAddress.scope(listen) {
            case .success(.loopback): break
            case .success: throw Invalid.loopbackOnlyWithoutLAN
            case .failure(let e): throw Invalid.listen(e)
            }
        }
    }

    /// The plist dictionary. RunAtLoad + KeepAlive + ThrottleInterval 30
    /// as in the install script; whether it starts at login is decided by
    /// where the file is (see AgentLocation).
    public var plist: [String: Any] {
        [
            "Label": Self.label,
            "ProgramArguments": arguments,
            "EnvironmentVariables": fullEnvironment,
            "RunAtLoad": true,
            "KeepAlive": true,
            "ThrottleInterval": Self.throttleInterval,
            "StandardOutPath": serverLog,
            "StandardErrorPath": errorLog,
        ]
    }

    /// XML plist bytes. PropertyListSerialization does the XML escaping
    /// (the script's xml_escape).
    public func plistData() throws -> Data {
        try validate()
        return try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0)
    }
}

/// An existing agent plist, written by the app or by the install script.
public struct ExistingAgent: Equatable, Sendable {
    public var label: String
    public var arguments: [String]
    public var environment: [String: String]
    public var standardErrorPath: String?

    public var program: String { arguments.first ?? "" }
    /// The install script's --lan: both flags.
    public var lan: Bool { arguments.contains("-tls") && arguments.contains("-allow-lan") }
    public var listen: String { environment["WITHALBUM_LISTEN"] ?? ListenAddress.loopbackDefault }

    public static func parse(_ data: Data) -> ExistingAgent? {
        guard let obj = try? PropertyListSerialization.propertyList(from: data, options: [], format: nil),
              let dict = obj as? [String: Any],
              let label = dict["Label"] as? String else { return nil }
        let args = (dict["ProgramArguments"] as? [Any])?.compactMap { $0 as? String }
            ?? (dict["Program"] as? String).map { [$0] } ?? []
        let env = (dict["EnvironmentVariables"] as? [String: Any])?.compactMapValues { $0 as? String } ?? [:]
        return ExistingAgent(label: label, arguments: args, environment: env,
                             standardErrorPath: dict["StandardErrorPath"] as? String)
    }

    public enum Owner: Equatable, Sendable {
        case thisApp            // runs the server inside this app bundle
        case otherAppCopy       // another copy of the app (moved, old version)
        case installScript      // scripts/install-macos.sh or a hand-made plist
    }

    public func owner(helperPath: String) -> Owner {
        if program == helperPath { return .thisApp }
        if program.hasSuffix(".app/Contents/Helpers/\(AppBundle.helperName)") { return .otherAppCopy }
        return .installScript
    }

    /// The configuration the app writes when it takes this agent over:
    /// the same storage, config file, other variables and exposure, with
    /// the program replaced by the bundled server. A LAN address that the
    /// rules no longer accept falls back to this Mac only (fail closed).
    public func takeOver(helperPath: String, logDirectory: String) -> (config: AgentConfig, droppedLAN: Bool) {
        var env = environment
        env.removeValue(forKey: "WITHALBUM_LISTEN")
        var c = AgentConfig(program: helperPath, lan: false, listen: ListenAddress.loopbackDefault,
                            environment: env, logDirectory: logDirectory)
        if lan {
            if case .success = ListenAddress.validateLAN(listen) {
                c.lan = true
                c.listen = listen
                return (c, false)
            }
            return (c, true)
        }
        if case .success(.loopback) = ListenAddress.scope(listen) { c.listen = listen }
        return (c, false)
    }
}

public enum AppBundle {
    /// The server executable inside the app: Contents/Helpers/withalbum-server.
    public static let helperName = "withalbum-server"

    public static func helperPath(bundlePath: String) -> String {
        (bundlePath as NSString).appendingPathComponent("Contents/Helpers/\(helperName)")
    }

    /// A LaunchAgent must not point into a disk image or a translocated
    /// copy (Gatekeeper runs quarantined apps from a random read-only path):
    /// the path disappears once the DMG is ejected.
    public static func canRegister(bundlePath: String) -> Bool {
        !bundlePath.hasPrefix("/Volumes/") && !bundlePath.contains("/AppTranslocation/")
    }
}

/// Where the agent plist lives.
///
/// Starting at login is the install script's location,
/// ~/Library/LaunchAgents (launchd loads it at every login). With login
/// start off the same plist is kept in the app's support directory and
/// loaded only when the user presses start, until logout.
public struct AgentLocation: Equatable, Sendable {
    public var home: String
    public init(home: String) { self.home = home }

    public var loginPlist: String { home + "/Library/LaunchAgents/\(AgentConfig.label).plist" }
    public var manualPlist: String { home + "/Library/Application Support/WithAlbum/ServerApp/\(AgentConfig.label).plist" }
    public var logDirectory: String { home + "/Library/Logs/WithAlbum" }
    /// The server's default config file (server/setup.go configPath).
    public var defaultConfigFile: String { home + "/Library/Application Support/WithAlbum/server.json" }
}
