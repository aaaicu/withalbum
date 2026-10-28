import AppKit
import SwiftUI
import WithAlbumCore

@main
enum Main {
    static func main() {
        // `--self-check`: print what the bundle contains and exit (used by
        // scripts/build-macos-app.sh; no window, no launchctl, no network).
        if CommandLine.arguments.contains("--self-check") {
            let helper = AppBundle.helperPath(bundlePath: Bundle.main.bundlePath)
            let r = Command.run(helper, ["-version"], timeout: 10)
            print("app: \(Bundle.main.bundleIdentifier ?? "?") \(Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?")")
            print("server: \(r.stdout.trimmingCharacters(in: .whitespacesAndNewlines))")
            exit(r.status == 0 && r.stdout.hasPrefix("withalbum-server ") ? 0 : 1)
        }
        // `--status`: one read-only status check printed to stdout (no
        // window, nothing written, launchctl only asked). For support and
        // tests; WITHALBUM_APP_PROBE_LISTEN / _LAN / _LOG override what the
        // agent plist says.
        if CommandLine.arguments.contains("--status") {
            exit(StatusCLI.run())
        }
        WithAlbumServerApp.main()
    }
}

enum StatusCLI {
    static func run() -> Int32 {
        let env = ProcessInfo.processInfo.environment
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let agents = LaunchAgentController(location: AgentLocation(home: home), dryRun: env["WITHALBUM_APP_DRY_RUN"] == "1")
        let inst = agents.installed()
        let rt = agents.runtime()
        let agent = inst?.agent
        let listen = env["WITHALBUM_APP_PROBE_LISTEN"] ?? agent?.listen ?? ListenAddress.loopbackDefault
        let lan = env["WITHALBUM_APP_PROBE_LAN"].map { $0 == "1" } ?? agent?.tls ?? false
        let log = env["WITHALBUM_APP_PROBE_LOG"] ?? agent?.standardErrorPath ?? agents.location.logDirectory + "/error.log"
        var pin: String?
        if lan {
            var penv = env
            for (k, v) in agent?.environment ?? [:] { penv[k] = v }
            penv["WITHALBUM_LISTEN"] = listen
            let helper = AppBundle.helperPath(bundlePath: Bundle.main.bundlePath)
            let r = Command.run(helper, ["tls", "-json"], environment: penv, timeout: 10)
            pin = TLSPairing.parse(Data(r.stdout.utf8))?.spkiSHA256
            print("pin: \(pin ?? "none (\(r.stderr.trimmingCharacters(in: .whitespacesAndNewlines)))")")
        }
        var probe = Probe.Result()
        let done = DispatchSemaphore(value: 0)
        if let base = URL(string: (lan ? "https://" : "http://") + listen), !lan || pin != nil {
            Task.detached {
                probe = await Probe.run(base: base, pin: pin)
                done.signal()
            }
            done.wait()
        }
        let state = ServerState.combine(agentPlistExists: inst != nil, runtime: rt, health: probe.health,
                                        setupPage: probe.setupPage, answered: probe.answered)
        print("agent: \(inst.map { "\($0.path) owner=\(String(describing: $0.agent?.owner(helperPath: AppBundle.helperPath(bundlePath: Bundle.main.bundlePath))))" } ?? "none") loaded=\(rt.loaded) running=\(rt.running)")
        print("probe: \(lan ? "https" : "http")://\(listen) answered=\(probe.answered) health=\(probe.health.map { "\($0)" } ?? "nil") setupPage=\(probe.setupPage)")
        print("state: \(state.title)")
        let text = FileManager.default.contents(atPath: log).map { String(decoding: $0, as: UTF8.self) }
        if let c = text.flatMap(SetupCode.lastIn), c.isLoopback {
            // The code itself stays on screen only in the app; here just
            // that one was found and until when it is valid.
            print("setup code: found in log, valid until \(c.expires.map { "\($0)" } ?? "?")")
        }
        if let p = text.flatMap(LogReason.lastProblem) { print("last problem: \(p)") }
        return 0
    }
}

struct WithAlbumServerApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Image(systemName: model.state.symbol)
                .accessibilityLabel("WithAlbum Server · \(model.state.title)")
        }
        .menuBarExtraStyle(.menu)

        Window("설치 코드", id: "setup") {
            SetupCodeView(model: model).frame(minWidth: 420)
        }
        .windowResizability(.contentSize)

        Window("LAN에서 접속 허용", id: "lan") {
            LANView(model: model).frame(minWidth: 480)
        }
        .windowResizability(.contentSize)

        Window("집 밖에서 접속 (Tailscale Funnel)", id: "funnel") {
            FunnelView(model: model).frame(minWidth: 520, minHeight: 480)
        }

        Window("내부망 격리 점검", id: "isolation") {
            IsolationView().frame(minWidth: 520, minHeight: 480)
        }

        Window("기기 연결 정보", id: "pairing") {
            PairingView(model: model).frame(minWidth: 480, minHeight: 420)
        }

        Window("서버 관리 전환", id: "takeover") {
            TakeOverView(model: model).frame(minWidth: 480)
        }
        .windowResizability(.contentSize)

        Window("WithAlbum 관리 화면", id: "dashboard") {
            DashboardWindow(model: model).frame(minWidth: 900, minHeight: 640)
        }
    }
}

extension ServerState {
    var symbol: String {
        switch self {
        case .running(let h): return h.integrityOK ? "photo.stack" : "exclamationmark.triangle"
        case .storageUnavailable, .failing: return "exclamationmark.triangle"
        case .setupNeeded: return "key"
        case .starting: return "hourglass"
        case .notRegistered, .stopped: return "pause.circle"
        case .unmanagedRunning: return "questionmark.circle"
        }
    }
}
