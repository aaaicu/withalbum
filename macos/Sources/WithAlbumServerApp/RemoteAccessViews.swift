import SwiftUI
import WithAlbumCore

// MARK: - Tailscale Funnel

/// "집 밖에서 접속 (Tailscale Funnel)". Nothing runs until a button is
/// pressed: the status check only reads, the switches change the server's
/// agent and the Tailscale forwarder. See docs/FUNNEL.md.
struct FunnelView: View {
    @ObservedObject var model: AppModel

    private var running: Bool { if case .running = model.state { return true } else { return false } }
    private var funnelOn: Bool { if case .on? = model.funnelState { return true } else { return false } }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                Text("집 밖에서 접속 (Tailscale Funnel)").font(.title2.weight(.bold))
                Text("가족 휴대폰에는 아무것도 더 설치하지 않아요. 이 Mac의 Tailscale이 공개 주소(https://…ts.net)로 들어온 연결을 서버로 그대로 넘겨요. 공유기 포트포워딩은 필요 없어요.")
                    .foregroundStyle(Theme.muted).fixedSize(horizontal: false, vertical: true)

                GroupBox {
                    VStack(alignment: .leading, spacing: 8) {
                        row("서버", model.funnelURL.map { "Funnel 경로 켜짐 · \($0)" } ?? "Funnel 경로 꺼짐")
                        if model.funnelChecked {
                            row("Tailscale", model.tailscale.map { "\($0.variantName) · \($0.path)" } ?? "찾지 못함")
                            if let t = model.tailscale, !t.funnelListedAsSupported {
                                Text("Tailscale 공식 표에는 이 설치 방식의 Funnel 지원이 '아니요'로 나와 있어요. 켜기가 실패하면 오픈소스 tailscaled(Homebrew)로 바꾸세요.")
                                    .font(.callout).foregroundStyle(Theme.warning).fixedSize(horizontal: false, vertical: true)
                            }
                            if let s = model.tailscaleStatus {
                                row("로그인", s.running ? "연결됨 · \(s.dnsName ?? "이름 없음")" : "연결 안 됨 (\(s.backendState))")
                                row("정책", policyText(s))
                            }
                            row("Funnel", stateText)
                        } else {
                            Text("‘상태 확인’을 누르면 이 Mac의 tailscale 명령으로 상태만 읽어요.").foregroundStyle(Theme.muted)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }

                if let p = model.funnelProblem {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(p.message).foregroundStyle(Theme.critical).fixedSize(horizontal: false, vertical: true)
                        HStack {
                            if let link = model.funnelLink { Button("Tailscale에서 켜기 열기") { model.openURL(link) } }
                            if p == .funnelNotAllowed { Button("관리 콘솔: Access controls") { model.openURL(Funnel.policyPage) } }
                            if p == .httpsDisabled { Button("관리 콘솔: DNS") { model.openURL(Funnel.dnsPage) } }
                        }
                    }
                }
                if let m = model.funnelMessage { Text(m).fixedSize(horizontal: false, vertical: true) }

                HStack {
                    Button("상태 확인") { Task { await model.checkFunnel() } }.disabled(model.busy)
                    Button("Funnel 켜기") { model.enableFunnel() }
                        .buttonStyle(BrandButtonStyle())
                        .disabled(model.busy || !running || !model.ownedByThisApp || model.storageRoot == nil)
                    Button("Funnel 끄기") { model.disableFunnel() }
                        .disabled(model.busy || (model.funnelURL == nil && !funnelOn))
                    if model.busy { ProgressView().controlSize(.small) }
                }
                if !running { Text("서버가 정상 실행 중일 때(설치를 마친 뒤)만 켤 수 있어요.").foregroundStyle(Theme.muted) }
                if model.funnelURL != nil {
                    Button("새 페어링 QR 보기 (공개 주소 포함)") { model.show("pairing") }
                }

                Note(text: "켜기를 누르면 ① 서버를 이 Mac 안(127.0.0.1:8788)에서만 받는 Funnel용 입구와 함께 다시 시작하고 ② 다음 명령을 실행해요:\n\(cliLine)\nTLS는 Tailscale이 풀지 않고 서버까지 그대로 가요(앱은 지금처럼 공개키 지문으로 서버를 확인). 관리 화면·설치 화면은 이 길로 열리지 않아요.")
                Note(text: "공개 주소는 인터넷의 누구나 접속을 시도할 수 있어요. 로그인 시도 제한·기기별 로그아웃이 있지만, 홈캠·Home Assistant와 이 Mac을 네트워크에서 떼어 놓는 것이 가장 중요한 방어예요(‘내부망 격리 점검’). Funnel은 베타이고 대역폭 제한이 있어 큰 동영상은 느릴 수 있어요.", warning: true)
                HStack {
                    Button("Tailscale Funnel 문서") { model.openURL(Funnel.docs) }
                    Button("Tailscale 관리 콘솔") { model.openURL(Funnel.adminConsole) }
                }
            }
            .padding(24)
        }
    }

    private var cliLine: String {
        "tailscale " + Funnel.enableArguments().joined(separator: " ")
    }

    private var stateText: String {
        switch model.funnelState {
        case nil: return "알 수 없음"
        case .off?: return "꺼짐"
        case .on(let pp)?: return pp ? "켜짐 · 서버로 그대로 전달" : "켜짐 · 서버로 그대로 전달(접속 주소 정보 없음)"
        case .tailnetOnly?: return "tailnet 안에서만 열림 (공개 안 됨)"
        case .terminatesTLS?: return "TLS를 Tailscale이 풀고 있어요 — 지원하지 않는 방식이에요. 끄고 다시 켜세요."
        case .otherTarget(let t)?: return "443 포트가 다른 곳(\(t))으로 연결돼 있어요"
        case .webServer?: return "443 포트를 다른 웹 공유(tailscale serve/funnel)가 쓰고 있어요"
        }
    }

    private func policyText(_ s: TailscaleStatus) -> String {
        var parts: [String] = []
        if let f = s.funnelAllowed { parts.append(f ? "Funnel 허용" : "Funnel 허용 안 됨") }
        if let h = s.httpsAllowed { parts.append(h ? "HTTPS 켜짐" : "HTTPS 꺼짐") }
        if let m = s.magicDNS { parts.append(m ? "MagicDNS 켜짐" : "MagicDNS 꺼짐") }
        return parts.isEmpty ? "확인 못 함(이 버전은 알려 주지 않아요)" : parts.joined(separator: " · ")
    }

    private func row(_ k: String, _ v: String) -> some View {
        HStack(alignment: .firstTextBaseline) {
            Text(k).foregroundStyle(Theme.muted).frame(width: 80, alignment: .leading)
            Text(v).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
        }
    }
}

// MARK: - Home network isolation check

/// "내부망 격리 점검": the user types the addresses of devices this Mac
/// must NOT reach (home cameras, Home Assistant, the router admin page);
/// one TCP connection per target, 2 s timeout, nothing saved or sent.
struct IsolationView: View {
    @State private var text = "# 예) 이름 IP:포트 — 한 줄에 하나\n"
    @State private var results: [(IsolationCheck.Target, IsolationCheck.Outcome)] = []
    @State private var errors: [String] = []
    @State private var running = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                Text("내부망 격리 점검").font(.title2.weight(.bold))
                Text("이 Mac이 털리더라도 홈캠·Home Assistant에 닿지 못하는지 확인해요. 그 기기들의 IP 주소와 서비스 포트를 적으세요(예: 홈캠 RTSP 554·웹 80/443, Home Assistant 8123, 공유기 관리 화면 80/443).")
                    .foregroundStyle(Theme.muted).fixedSize(horizontal: false, vertical: true)
                TextEditor(text: $text)
                    .font(.system(.body, design: .monospaced))
                    .frame(minHeight: 120)
                    .overlay(RoundedRectangle(cornerRadius: 6).stroke(Theme.neutralWeak))
                HStack {
                    Button("점검 시작") { run() }.buttonStyle(BrandButtonStyle()).disabled(running)
                    if running { ProgressView().controlSize(.small) }
                }
                ForEach(errors, id: \.self) { Text($0).foregroundStyle(Theme.critical) }
                if !results.isEmpty {
                    Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 6) {
                        ForEach(Array(results.enumerated()), id: \.offset) { _, r in
                            GridRow {
                                Text(r.0.label)
                                Text(r.0.address).font(.system(.body, design: .monospaced))
                                Text(r.1.title).bold().foregroundStyle(r.1.blocked ? Theme.positive : Theme.critical)
                            }
                            GridRow {
                                Color.clear.gridCellUnsizedAxes([.horizontal, .vertical])
                                Text(r.1.detail).font(.caption).foregroundStyle(Theme.muted).gridCellColumns(2)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                    }
                    verdictView
                }
                Note(text: "‘막힘’이 모두 나오면 1단계(이 Mac → 기기 접속 실패)를 통과한 거예요. 격리를 끝낸 건 아직 아니에요: ② 스마트홈 기기가 있는 전체 대역의 방화벽 정책 검토 ③ 재부팅·DHCP 주소 변경·Wi-Fi↔유선 전환 뒤 다시 점검 ④ 사진 업로드·외장 디스크 등 정상 동작 확인 ⑤ 필요하면 기기 → 이 Mac 방향 차단까지 확인하세요. 자세한 안내: docs/HOME_NETWORK_ISOLATION.md")
                Note(text: "적은 주소로만 한 번씩 접속을 시도해요(주변 스캔 없음, 이름 조회 없음). 결과는 저장하거나 서버로 보내지 않아요. macOS가 ‘로컬 네트워크’ 접근을 물으면 허용해야 정확해요 — 거부하면 열린 기기도 ‘막힘(경로 없음)’으로 보일 수 있어요(시스템 설정 → 개인정보 보호 및 보안 → 로컬 네트워크).", warning: true)
            }
            .padding(24)
        }
    }

    @ViewBuilder private var verdictView: some View {
        switch IsolationCheck.verdict(results.map(\.1)) {
        case .isolated?:
            Label("적은 기기 모두 이 Mac에서 막혀 있어요", systemImage: "checkmark.shield").foregroundStyle(Theme.positive)
        case .notIsolated?:
            Label("이 Mac에서 닿는 기기가 있어요 — 공유기의 망 분리(VLAN/게스트망)를 다시 확인하세요", systemImage: "exclamationmark.triangle")
                .foregroundStyle(Theme.critical)
        case .inconclusive?:
            Label("일부 기기는 확인하지 못했어요", systemImage: "questionmark.circle").foregroundStyle(Theme.warning)
        case nil:
            EmptyView()
        }
    }

    private func run() {
        let parsed = IsolationCheck.parse(text)
        errors = parsed.errors.map { "\($0.line): \(IsolationCheck.message($0.error))" }
        results = []
        guard !parsed.targets.isEmpty else {
            if errors.isEmpty { errors = ["점검할 주소를 한 줄에 하나씩 적어 주세요 (예: 홈캠 192.168.0.30:554)"] }
            return
        }
        running = true
        Task {
            let out = await withTaskGroup(of: (Int, IsolationCheck.Outcome).self) { g in
                for (i, t) in parsed.targets.enumerated() {
                    g.addTask { (i, await Task.detached { IsolationCheck.probe(t.ip, port: t.port) }.value) }
                }
                var r = [IsolationCheck.Outcome?](repeating: nil, count: parsed.targets.count)
                for await (i, o) in g { r[i] = o }
                return r.compactMap { $0 }
            }
            results = Array(zip(parsed.targets, out))
            running = false
        }
    }
}
