import Foundation

/// Runs a command without a shell and returns its exit status and output.
public struct CommandResult: Sendable {
    public var status: Int32
    public var stdout: String
    public var stderr: String
}

public enum Command {
    public static func run(_ path: String, _ args: [String], environment: [String: String]? = nil,
                    timeout: TimeInterval = 15) -> CommandResult {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = args
        if let environment { p.environment = environment }
        p.currentDirectoryURL = URL(fileURLWithPath: "/")
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        p.standardInput = FileHandle.nullDevice
        do { try p.run() } catch {
            return CommandResult(status: -1, stdout: "", stderr: "\(error)")
        }
        // Read before waiting so a full pipe cannot block the child.
        var outData = Data(), errData = Data()
        let group = DispatchGroup()
        group.enter()
        DispatchQueue.global().async { outData = out.fileHandleForReading.readDataToEndOfFile(); group.leave() }
        group.enter()
        DispatchQueue.global().async { errData = err.fileHandleForReading.readDataToEndOfFile(); group.leave() }
        let deadline = Date().addingTimeInterval(timeout)
        while p.isRunning && Date() < deadline { usleep(20_000) }
        if p.isRunning { p.terminate() }
        p.waitUntilExit()
        group.wait()
        return CommandResult(status: p.terminationStatus,
                             stdout: String(decoding: outData, as: UTF8.self),
                             stderr: String(decoding: errData, as: UTF8.self))
    }
}

public struct AgentError: LocalizedError {
    public var message: String
    public var errorDescription: String? { message }
}

/// The org.withalbum.server LaunchAgent of the logged-in user, managed with
/// launchctl like scripts/install-macos.sh. In dry-run mode
/// (WITHALBUM_APP_DRY_RUN=1) every file lives in a temporary directory and
/// state-changing launchctl calls are only logged.
public final class LaunchAgentController {
    public let location: AgentLocation
    public let dryRun: Bool
    private let launchctl = "/bin/launchctl"
    private var domain: String { "gui/\(getuid())" }
    private var service: String { "\(domain)/\(AgentConfig.label)" }

    public init(location: AgentLocation, dryRun: Bool) {
        self.location = location
        self.dryRun = dryRun
    }

    public struct Installed {
        public var path: String
        public var agent: ExistingAgent?    // nil: present but not a readable agent plist
        public var loginStart: Bool
    }

    /// The agent plist: the login location first (the install script's),
    /// then the app's manual-start location.
    public func installed() -> Installed? {
        for (path, login) in [(location.loginPlist, true), (location.manualPlist, false)] {
            guard FileManager.default.fileExists(atPath: path) else { continue }
            let data = FileManager.default.contents(atPath: path) ?? Data()
            return Installed(path: path, agent: ExistingAgent.parse(data), loginStart: login)
        }
        return nil
    }

    public func runtime() -> AgentRuntime {
        if dryRun { return .notLoaded }
        let r = Command.run(launchctl, ["print", service], timeout: 5)
        return r.status == 0 ? AgentRuntime.parsePrint(r.stdout) : .notLoaded
    }

    /// Writes the plist (refusing to replace one this app does not own
    /// unless `takeOver`), removes the copy in the other location, and
    /// (re)loads the agent.
    public func install(_ config: AgentConfig, loginStart: Bool, helperPath: String, takeOver: Bool) throws {
        if let cur = installed(), !takeOver {
            guard let a = cur.agent, a.owner(helperPath: helperPath) == .thisApp else {
                throw AgentError(message: "다른 방법으로 설치한 서버 설정이 있어요. 먼저 \"이 앱으로 관리 전환\"을 확인하세요.")
            }
        }
        let data = try config.plistData()
        try prepareLogDirectory(config.logDirectory)
        let target = loginStart ? location.loginPlist : location.manualPlist
        let other = loginStart ? location.manualPlist : location.loginPlist
        try writePlist(data, to: target)
        if FileManager.default.fileExists(atPath: other) { try FileManager.default.removeItem(atPath: other) }
        _ = launch(["bootout", service])
        // bootout finishes asynchronously; bootstrap right after it can
        // fail with EIO (5) for a moment.
        var r = launch(["bootstrap", domain, target])
        for _ in 0..<5 where r.status != 0 {
            usleep(500_000)
            r = launch(["bootstrap", domain, target])
        }
        if r.status != 0 {
            throw AgentError(message: "launchctl bootstrap 실패(\(r.status)): \(r.stderr.trimmingCharacters(in: .whitespacesAndNewlines))")
        }
    }

    public func start() throws {
        guard let cur = installed() else { throw AgentError(message: "등록된 서버가 없어요.") }
        if runtime().loaded {
            try check(launch(["kickstart", service]), "kickstart")
        } else {
            try check(launch(["bootstrap", domain, cur.path]), "bootstrap")
        }
    }

    /// Unloads the agent; a login plist still starts it at the next login.
    public func stop() throws {
        let r = launch(["bootout", service])
        // 3: no such process (not loaded) is fine.
        if r.status != 0 && r.status != 3 && runtime().loaded { try check(r, "bootout") }
    }

    public func restart() throws {
        if runtime().loaded {
            try check(launch(["kickstart", "-k", service]), "kickstart -k")
        } else {
            try start()
        }
    }

    /// Moves the plist between the login and the manual location. The
    /// running server is not touched.
    public func setLoginStart(_ on: Bool) throws {
        guard let cur = installed() else { return }
        let target = on ? location.loginPlist : location.manualPlist
        guard cur.path != target else { return }
        let data = FileManager.default.contents(atPath: cur.path) ?? Data()
        try writePlist(data, to: target)
        try FileManager.default.removeItem(atPath: cur.path)
    }

    /// Unloads and deletes the plist the app wrote. Logs, the config file
    /// and the storage are kept.
    public func unregister(helperPath: String) throws {
        guard let cur = installed() else { return }
        guard cur.agent?.owner(helperPath: helperPath) == .thisApp else {
            throw AgentError(message: "이 앱이 등록하지 않은 서버 설정은 지우지 않아요.")
        }
        try stop()
        try FileManager.default.removeItem(atPath: cur.path)
    }

    // MARK: -

    private func launch(_ args: [String]) -> CommandResult {
        if dryRun {
            FileHandle.standardError.write(Data("dry-run: launchctl \(args.joined(separator: " "))\n".utf8))
            return CommandResult(status: 0, stdout: "", stderr: "")
        }
        return Command.run(launchctl, args)
    }

    private func check(_ r: CommandResult, _ what: String) throws {
        if r.status != 0 {
            throw AgentError(message: "launchctl \(what) 실패(\(r.status)): \(r.stderr.trimmingCharacters(in: .whitespacesAndNewlines))")
        }
    }

    private func writePlist(_ data: Data, to path: String) throws {
        let dir = (path as NSString).deletingLastPathComponent
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try data.write(to: URL(fileURLWithPath: path), options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: path)
    }

    /// Logs can contain the one-time setup code: owner-only (0700), as the
    /// install script does.
    private func prepareLogDirectory(_ dir: String) throws {
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: dir)
    }
}
