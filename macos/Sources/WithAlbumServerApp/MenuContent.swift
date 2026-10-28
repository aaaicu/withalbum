import SwiftUI
import WithAlbumCore

/// The menu bar menu. Wording follows the dashboard rules (see
/// StatusTests.testWordingRules): "보관됨" is not used here at all.
struct MenuContent: View {
    @ObservedObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Group {
            Text("WithAlbum Server · \(model.state.title)")
            if let d = model.state.detail { Text(d) }
            if let p = model.lastProblem { Text("최근 오류: \(p)").lineLimit(2) }
            if model.installed != nil { Text(model.exposureText) }
            Text("저장 위치: \(model.storageRoot ?? "아직 정하지 않음")")
            if let v = model.state.version ?? (model.helperVersion.isEmpty ? nil : model.helperVersion) {
                Text("서버 버전: \(v.replacingOccurrences(of: "withalbum-server ", with: ""))")
            }
            if let m = model.message { Text(m) }
            Divider()

            Button("관리 화면 열기") { model.openDashboard() }
                .disabled(!canOpenDashboard)
            if model.state == .setupNeeded {
                Button("설치 코드 보기…") { model.show("setup") }
            }
            if model.lan || model.funnelURL != nil {
                Button("기기 연결 정보(페어링 QR)…") { model.show("pairing") }
            }
            Divider()

            agentSection
            Divider()

            Button("LAN에서 접속 허용…") { model.show("lan") }
                .disabled(!model.ownedByThisApp)
            Button("집 밖에서 접속 (Tailscale Funnel)…") { model.show("funnel") }
                .disabled(!model.ownedByThisApp)
            Button("내부망 격리 점검…") { model.show("isolation") }
            Toggle("로그인 시 서버 시작", isOn: Binding(get: { model.loginStart }, set: { model.setLoginStart($0) }))
                .disabled(!model.ownedByThisApp || model.busy)
            Toggle("로그인 시 이 앱 열기", isOn: Binding(get: { model.menuAppAtLogin }, set: { model.setMenuAppAtLogin($0) }))
            Button("로그 폴더 열기") { model.openLogs() }
            Divider()

            if model.ownedByThisApp {
                Button("서버 등록 해제…") { confirmUnregister() }.disabled(model.busy)
            }
            Button("메뉴 막대 앱 종료 (서버는 계속 실행)") { model.quit(stoppingServer: false) }
            if model.ownedByThisApp && model.state != .stopped {
                Button("서버를 멈추고 종료") { model.quit(stoppingServer: true) }
            }
        }
        .onAppear { model.openWindow = { openWindow(id: $0) } }
    }

    private var canOpenDashboard: Bool {
        switch model.state {
        case .running, .storageUnavailable, .setupNeeded, .unmanagedRunning: return true
        default: return false
        }
    }

    @ViewBuilder private var agentSection: some View {
        switch model.owner {
        case nil:
            if model.installed != nil {
                Text("읽을 수 없는 서버 설정 파일이 있어요")
                Button("서버 관리 전환…") { model.show("takeover") }
            } else {
                Button("서버 등록하고 시작") { model.register() }
                    .disabled(model.busy || !model.canRegisterHere)
                if !model.canRegisterHere {
                    Text("응용 프로그램 폴더로 옮긴 뒤 다시 여세요")
                }
            }
        case .installScript?, .otherAppCopy?:
            Text(model.owner == .installScript ? "스크립트로 설치한 서버가 있어요" : "다른 위치의 WithAlbum Server 앱이 관리 중이에요")
            Button("이 앱으로 관리 전환…") { model.show("takeover") }
        case .thisApp?:
            Button("시작") { model.start() }
                .disabled(model.busy || isRunningish)
            Button("멈춤") { model.stop() }
                .disabled(model.busy || model.state == .stopped)
            Button("다시 시작") { model.restart() }
                .disabled(model.busy)
        }
    }

    private var isRunningish: Bool {
        switch model.state {
        case .running, .storageUnavailable, .setupNeeded, .starting: return true
        default: return false
        }
    }

    private func confirmUnregister() {
        NSApp.activate(ignoringOtherApps: true)
        let a = NSAlert()
        a.messageText = "서버 등록을 해제할까요?"
        a.informativeText = "서버를 멈추고 자동 시작 설정을 지워요. 사진 원본, 저장소, 설정 파일, 로그는 그대로 남아요."
        a.addButton(withTitle: "등록 해제")
        a.addButton(withTitle: "취소")
        if a.runModal() == .alertFirstButtonReturn { model.unregister() }
    }
}
