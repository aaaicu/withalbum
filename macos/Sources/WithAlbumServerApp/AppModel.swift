import AppKit
import Foundation
import ServiceManagement
import WithAlbumCore

/// State of the menu and the actions behind it. Everything the app does
/// stays on this Mac: it reads the agent plist, the server's config file
/// and its logs, asks launchctl, and talks to the server on its own listen
/// address (loopback, or this Mac's private LAN address with the key pin).
@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var state: ServerState = .notRegistered
    @Published private(set) var installed: LaunchAgentController.Installed?
    @Published private(set) var setupCode: SetupCode?
    @Published private(set) var lastProblem: String?
    @Published private(set) var storageRoot: String?
    @Published private(set) var pairing: TLSPairing?
    @Published private(set) var pairingError: String?
    @Published private(set) var helperVersion = ""
    @Published var message: String?
    @Published private(set) var busy = false
    @Published private(set) var menuAppAtLogin = false
    // Tailscale Funnel (docs/FUNNEL.md). Filled only when the user asks.
    @Published private(set) var tailscale: TailscaleCLI?
    @Published private(set) var tailscaleStatus: TailscaleStatus?
    @Published private(set) var funnelState: FunnelServeStatus.State?
    @Published private(set) var funnelProblem: TailscaleProblem?
    @Published private(set) var funnelLink: URL?
    @Published private(set) var funnelChecked = false
    @Published private(set) var funnelMessage: String?

    let dryRun: Bool
    let bundlePath: String
    let helperPath: String
    let agents: LaunchAgentController
    private var timer: Timer?
    private var openSetupWhenReady = false
    var openWindow: ((String) -> Void)?

    init() {
        let env = ProcessInfo.processInfo.environment
        dryRun = env["WITHALBUM_APP_DRY_RUN"] == "1"
        bundlePath = Bundle.main.bundlePath
        helperPath = AppBundle.helperPath(bundlePath: bundlePath)
        var home = FileManager.default.homeDirectoryForCurrentUser.path
        if dryRun {
            home = (NSTemporaryDirectory() as NSString).appendingPathComponent("withalbum-app-dry-run-\(getpid())")
            try? FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
            FileHandle.standardError.write(Data("WithAlbum Server app: dry run, files under \(home), launchctl not called\n".utf8))
        }
        agents = LaunchAgentController(location: AgentLocation(home: home), dryRun: dryRun)
        menuAppAtLogin = SMAppService.mainApp.status == .enabled
        let r = Command.run(helperPath, ["-version"], timeout: 5)
        helperVersion = r.status == 0 ? r.stdout.trimmingCharacters(in: .whitespacesAndNewlines) : ""
        timer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refresh() }
        }
        Task { await refresh() }
    }

    // MARK: - Derived

    var agent: ExistingAgent? { installed?.agent }
    var owner: ExistingAgent.Owner? { agent?.owner(helperPath: helperPath) }
    var ownedByThisApp: Bool { owner == .thisApp }
    var lan: Bool { agent?.lan ?? false }
    /// -tls: the LAN, or the funnel route (loopback HTTPS with the pin).
    var tls: Bool { agent?.tls ?? false }
    var funnelURL: String? { agent?.funnelURL }
    var listen: String { agent?.listen ?? ListenAddress.loopbackDefault }
    var loginStart: Bool { installed?.loginStart ?? false }
    var canRegisterHere: Bool { AppBundle.canRegister(bundlePath: bundlePath) && !helperVersion.isEmpty }

    /// Base URL of the server: plain HTTP on loopback, HTTPS with -tls.
    var baseURL: URL? { URL(string: (tls ? "https://" : "http://") + listen) }

    var exposureText: String {
        let base = lan ? "LAN 접속 허용 · https://\(listen) (TLS)" : "이 Mac에서만 접속 · \(listen)"
        guard let f = funnelURL else { return base }
        return base + " · 집 밖: \(f) (Tailscale Funnel)"
    }

    var config: AgentConfig? {
        guard let a = agent else { return nil }
        var env = a.environment
        env.removeValue(forKey: "WITHALBUM_LISTEN")
        return AgentConfig(program: helperPath, lan: a.lan, listen: a.listen, environment: env,
                           logDirectory: agents.location.logDirectory)
    }

    // MARK: - Refresh

    func refresh() async {
        let agents = self.agents
        let inst = await Task.detached { agents.installed() }.value
        let rt = await Task.detached { agents.runtime() }.value
        installed = inst
        storageRoot = readStorageRoot(inst?.agent)
        var pin: String?
        if inst?.agent?.tls == true {
            if pairing == nil { await loadPairing() }
            pin = pairing?.spkiSHA256
        }
        var probe = Probe.Result()
        if let base = baseURL, inst?.agent?.tls != true || pin != nil {
            probe = await Probe.run(base: base, pin: pin)
        }
        let newState = ServerState.combine(agentPlistExists: inst != nil, runtime: rt, health: probe.health,
                                           setupPage: probe.setupPage, answered: probe.answered)
        state = newState
        let log = readLogTail(errorLogPath)
        if newState == .setupNeeded {
            setupCode = log.flatMap(SetupCode.lastIn).flatMap { $0.isLoopback ? $0 : nil }
            if openSetupWhenReady, setupCode != nil {
                openSetupWhenReady = false
                show("setup")
            }
        } else {
            setupCode = nil
        }
        lastProblem = (newState == .failing || newState == .starting) ? log.flatMap(LogReason.lastProblem) : nil
    }

    private var errorLogPath: String {
        agent?.standardErrorPath ?? (agents.location.logDirectory + "/error.log")
    }

    /// The last 64 KiB of a log file.
    private func readLogTail(_ path: String) -> String? {
        guard let h = FileHandle(forReadingAtPath: path) else { return nil }
        defer { try? h.close() }
        let end = (try? h.seekToEnd()) ?? 0
        try? h.seek(toOffset: end > 65536 ? end - 65536 : 0)
        return String(decoding: h.readDataToEndOfFile(), as: UTF8.self)
    }

    /// WITHALBUM_ROOT of the agent, or the root the setup screen registered
    /// in the server's config file.
    private func readStorageRoot(_ a: ExistingAgent?) -> String? {
        if let root = a?.environment["WITHALBUM_ROOT"], !root.isEmpty { return root }
        let path = a?.environment["WITHALBUM_CONFIG"] ?? agents.location.defaultConfigFile
        return FileManager.default.contents(atPath: path).flatMap(ServerConfigFile.storageRoot)
    }

    /// Runs `withalbum-server tls -json` with the agent's environment: it
    /// only reads the identity in the storage (safe while the server runs).
    func loadPairing() async {
        guard let a = agent else { return }
        var env = ProcessInfo.processInfo.environment
        for (k, v) in a.environment { env[k] = v }
        let helper = helperPath
        let r = await Task.detached { Command.run(helper, ["tls", "-json"], environment: env, timeout: 10) }.value
        if r.status == 0, let p = TLSPairing.parse(Data(r.stdout.utf8)) {
            pairing = p
            pairingError = nil
        } else {
            pairing = nil
            pairingError = r.stderr.trimmingCharacters(in: .whitespacesAndNewlines)
        }
    }

    // MARK: - Actions

    private func perform(_ what: String, success: String? = nil, _ body: @escaping (LaunchAgentController) throws -> Void) {
        busy = true
        message = nil
        let agents = self.agents
        Task {
            let err: String? = await Task.detached {
                do { try body(agents); return nil } catch { return "\(what): \(error.localizedDescription)" }
            }.value
            message = err ?? success
            busy = false
            pairing = nil
            await refresh()
        }
    }

    /// Registers the bundled server for this user: plain HTTP on loopback,
    /// storage chosen by the setup screen (or already in the config file).
    func register() {
        guard canRegisterHere else {
            message = "응용 프로그램 폴더로 옮긴 뒤 앱을 다시 여세요. 디스크 이미지 안에서는 등록할 수 없어요."
            return
        }
        if state == .unmanagedRunning {
            message = "이미 다른 WithAlbum 서버가 \(listen)에서 실행 중이에요. 그 서버를 먼저 멈추세요."
            return
        }
        var env: [String: String] = [:]
        if let cfg = ProcessInfo.processInfo.environment["WITHALBUM_CONFIG"], !cfg.isEmpty {
            env["WITHALBUM_CONFIG"] = cfg
        }
        let c = AgentConfig(program: helperPath, environment: env, logDirectory: agents.location.logDirectory)
        let helper = helperPath
        openSetupWhenReady = true
        perform("서버 등록") { try $0.install(c, loginStart: true, helperPath: helper, takeOver: false) }
    }

    /// Replaces the program of an agent the install script (or another copy
    /// of the app) wrote, keeping its storage, config file and exposure.
    func takeOver() {
        guard canRegisterHere, let a = agent else { return }
        let (c, dropped) = a.takeOver(helperPath: helperPath, logDirectory: agents.location.logDirectory)
        let login = loginStart
        let helper = helperPath
        let note = dropped ? "기존 LAN 주소(\(a.listen))는 규칙에 맞지 않아 이 Mac에서만 접속으로 바꿨어요." : nil
        perform("관리 전환", success: note) { try $0.install(c, loginStart: login, helperPath: helper, takeOver: true) }
    }

    func start() { perform("시작") { try $0.start() } }
    func stop() { perform("멈춤") { try $0.stop() } }
    func restart() { perform("다시 시작") { try $0.restart() } }

    func setLoginStart(_ on: Bool) {
        guard ownedByThisApp else { return }
        perform("로그인 시 시작") { try $0.setLoginStart(on) }
    }

    func unregister() {
        let helper = helperPath
        perform("등록 해제") { try $0.unregister(helperPath: helper) }
    }

    /// -tls -allow-lan on a private address of this Mac (the script's --lan).
    /// Needs a running server with a registered storage: on loopback that
    /// means the administrator exists (otherwise it would be in setup mode).
    /// The server checks the administrator again and refuses otherwise.
    func enableLAN(_ addr: String) {
        guard var c = config, ownedByThisApp else { return }
        guard case .running = state, storageRoot != nil else {
            message = "LAN 접속은 설치를 마치고(저장소 등록 + 관리자) 서버가 정상 실행 중일 때만 켤 수 있어요."
            return
        }
        guard case .success(.privateLAN) = ListenAddress.validateLAN(addr) else {
            if case .failure(let e) = ListenAddress.validateLAN(addr) { message = ListenAddress.message(e) }
            return
        }
        c.lan = true
        c.listen = addr
        let login = loginStart, helper = helperPath
        perform("LAN 접속 허용") { try $0.install(c, loginStart: login, helperPath: helper, takeOver: false) }
    }

    func disableLAN() {
        guard var c = config, ownedByThisApp else { return }
        c.lan = false
        c.listen = ListenAddress.loopbackDefault
        let login = loginStart, helper = helperPath
        perform("이 Mac에서만 접속") { try $0.install(c, loginStart: login, helperPath: helper, takeOver: false) }
    }

    func setMenuAppAtLogin(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
        } catch {
            message = "로그인 시 앱 열기: \(error.localizedDescription)"
        }
        menuAppAtLogin = SMAppService.mainApp.status == .enabled
    }

    func openDashboard() {
        if tls {
            show("dashboard")
            return
        }
        guard let base = baseURL else { return }
        NSWorkspace.shared.open(state == .setupNeeded ? base.appendingPathComponent("setup") : base)
    }

    func openLogs() {
        let dir = agents.location.logDirectory
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try? FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: dir)
        NSWorkspace.shared.open(URL(fileURLWithPath: dir))
    }

    func copy(_ text: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
    }

    func show(_ id: String) {
        NSApp.activate(ignoringOtherApps: true)
        openWindow?(id)
    }

    // MARK: - Tailscale Funnel

    /// Runs one tailscale command off the main thread.
    private func tailscaleRun(_ cli: TailscaleCLI, _ args: [String], timeout: TimeInterval) async -> CommandResult {
        await Task.detached { Command.run(cli.path, args, timeout: timeout) }.value
    }

    private func detectTailscale() -> TailscaleCLI? {
        let fm = FileManager.default
        return TailscaleCLI.detect(isExecutable: { fm.isExecutableFile(atPath: $0) }, fileExists: { fm.fileExists(atPath: $0) },
                                   resolve: { (try? fm.destinationOfSymbolicLink(atPath: $0)).map { URL(fileURLWithPath: $0).path } ?? $0 })
    }

    /// "상태 확인": finds the CLI and reads `status --json` and
    /// `funnel status --json` (read-only). Only on the user's click.
    func checkFunnel() async {
        funnelChecked = true
        funnelProblem = nil
        funnelLink = nil
        tailscale = detectTailscale()
        guard let cli = tailscale else {
            tailscaleStatus = nil
            funnelState = nil
            funnelProblem = .notInstalled
            return
        }
        let st = await tailscaleRun(cli, Funnel.statusArguments, timeout: 8)
        tailscaleStatus = TailscaleStatus.parse(Data(st.stdout.utf8))
        guard let s = tailscaleStatus else {
            funnelProblem = TailscaleProblem.classify(st.stderr + st.stdout)
            funnelState = nil
            return
        }
        guard s.running else {
            funnelProblem = .loggedOut
            funnelState = nil
            return
        }
        if s.funnelAllowed == false { funnelProblem = .funnelNotAllowed }
        else if s.httpsAllowed == false || s.magicDNS == false { funnelProblem = .httpsDisabled }
        let fs = await tailscaleRun(cli, Funnel.funnelStatusArguments, timeout: 8)
        if let parsed = FunnelServeStatus.parse(Data(fs.stdout.utf8)), let host = s.dnsName {
            funnelState = parsed.state(host: host)
        } else {
            funnelState = nil
            if fs.status != 0, funnelProblem == nil { funnelProblem = TailscaleProblem.classify(fs.stderr + fs.stdout) }
        }
    }

    /// "Funnel 켜기": restarts the server with the funnel listener
    /// (loopback, -tls), then asks tailscale to forward the public port to
    /// it as raw TCP. Each step reports what failed.
    func enableFunnel() {
        guard ownedByThisApp, var c = config else { return }
        guard case .running = state, storageRoot != nil else {
            funnelMessage = "집 밖 접속은 설치를 마치고(저장소 등록 + 관리자) 서버가 정상 실행 중일 때만 켤 수 있어요."
            return
        }
        busy = true
        funnelMessage = nil
        Task {
            defer { busy = false }
            await checkFunnel()
            guard let cli = tailscale, let s = tailscaleStatus, s.running else { return }
            guard let host = s.dnsName, let pub = Funnel.publicURL(dnsName: host) else {
                funnelMessage = "이 Mac의 Tailscale 이름(MagicDNS)을 읽지 못했어요. 관리 콘솔에서 MagicDNS를 켜세요."
                return
            }
            if funnelProblem == .funnelNotAllowed {
                funnelLink = Funnel.policyPage
                return
            }
            if c.funnelURL != pub {
                c.setFunnel(pub)
                let login = loginStart, helper = helperPath, agents = self.agents
                let err: String? = await Task.detached {
                    do { try agents.install(c, loginStart: login, helperPath: helper, takeOver: false); return nil }
                    catch { return error.localizedDescription }
                }.value
                if let err {
                    funnelMessage = "서버 설정 변경 실패: \(err)"
                    return
                }
            }
            let r = await tailscaleRun(cli, Funnel.enableArguments(), timeout: 30)
            if r.status != 0 {
                let out = r.stderr + "\n" + r.stdout
                funnelProblem = TailscaleProblem.classify(out)
                funnelLink = TailscaleProblem.adminLink(in: out)
                funnelMessage = "서버는 Funnel용으로 다시 시작했지만 tailscale이 거부했어요. 아래 안내를 따른 뒤 다시 켜세요."
            } else {
                funnelMessage = "켰어요. 휴대폰 Wi-Fi를 끄고 LTE로 \(pub) 연결을 확인하세요."
            }
            pairing = nil
            await refresh()
            await checkFunnel()
            if r.status == 0 { funnelProblem = nil }
        }
    }

    /// "Funnel 끄기": stops the forwarder, then restarts the server without
    /// the funnel listener (plain loopback HTTP again unless the LAN is on).
    func disableFunnel() {
        guard ownedByThisApp, var c = config else { return }
        busy = true
        funnelMessage = nil
        Task {
            defer { busy = false }
            var note = "껐어요. 집 밖에서는 더 이상 연결되지 않아요."
            if let cli = tailscale ?? detectTailscale() {
                let r = await tailscaleRun(cli, Funnel.disableArguments(), timeout: 20)
                if r.status != 0 {
                    note = "서버 쪽은 껐지만 tailscale 끄기가 실패했어요(\(TailscaleProblem.classify(r.stderr + r.stdout).message)). 터미널에서 tailscale funnel status로 확인하세요."
                }
            }
            if c.funnelURL != nil {
                c.setFunnel(nil)
                let login = loginStart, helper = helperPath, agents = self.agents
                let err: String? = await Task.detached {
                    do { try agents.install(c, loginStart: login, helperPath: helper, takeOver: false); return nil }
                    catch { return error.localizedDescription }
                }.value
                if let err { note = "서버 설정 변경 실패: \(err)" }
            }
            funnelMessage = note
            pairing = nil
            await refresh()
            await checkFunnel()
        }
    }

    func openURL(_ u: URL) { NSWorkspace.shared.open(u) }

    func quit(stoppingServer: Bool) {
        if stoppingServer, ownedByThisApp { try? agents.stop() }
        NSApp.terminate(nil)
    }
}
