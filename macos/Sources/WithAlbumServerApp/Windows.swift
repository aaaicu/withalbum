import AppKit
import CoreImage.CIFilterBuiltins
import SwiftUI
import WithAlbumCore

/// Colours derived from SEED Design tokens (github.com/daangn/seed-design,
/// Apache-2.0, Copyright Daangn Market Co., Ltd.) with the WithAlbum
/// "archive green" brand, the same values as the built-in web pages
/// (internal/httpapi/web/index.html). See third_party/seed-design/.
enum Theme {
    static func dynamic(light: UInt32, dark: UInt32) -> Color {
        Color(nsColor: NSColor(name: nil) { appearance in
            let v = appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light
            return NSColor(srgbRed: CGFloat((v >> 16) & 0xff) / 255, green: CGFloat((v >> 8) & 0xff) / 255,
                           blue: CGFloat(v & 0xff) / 255, alpha: 1)
        })
    }
    static let brand = dynamic(light: 0x00745f, dark: 0x117956)        // bg-brand-solid
    static let positive = dynamic(light: 0x079171, dark: 0x22b27f)     // fg-positive
    static let warning = dynamic(light: 0x9b7821, dark: 0xca901c)      // fg-warning
    static let warningWeak = dynamic(light: 0xfff7de, dark: 0x302819)  // bg-warning-weak
    static let critical = dynamic(light: 0xfa342c, dark: 0xff6e60)     // fg-critical
    static let neutralWeak = dynamic(light: 0xf3f4f5, dark: 0x2b2e35)  // bg-neutral-weak
    static let muted = dynamic(light: 0x555d6d, dark: 0xdcdee3)        // fg-neutral-muted
}

struct Note: View {
    var text: String
    var warning = false
    var body: some View {
        Text(text)
            .font(.callout)
            .foregroundStyle(warning ? Theme.warning : Theme.muted)
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(warning ? Theme.warningWeak : Theme.neutralWeak, in: RoundedRectangle(cornerRadius: 12))
            .fixedSize(horizontal: false, vertical: true)
    }
}

struct BrandButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.body.weight(.semibold))
            .foregroundStyle(.white)
            .padding(.horizontal, 16).padding(.vertical, 8)
            .background(Theme.brand.opacity(configuration.isPressed ? 0.8 : 1), in: RoundedRectangle(cornerRadius: 10))
    }
}

// MARK: - Setup code

struct SetupCodeView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("설치 코드").font(.title2.weight(.bold))
            if let c = model.setupCode {
                Text(c.code)
                    .font(.system(size: 32, weight: .semibold, design: .monospaced))
                    .textSelection(.enabled)
                    .accessibilityLabel("설치 코드 \(c.code)")
                if let end = c.expires {
                    Text(end > Date() ? "\(end.formatted(date: .omitted, time: .shortened))까지 한 번만 쓸 수 있어요. 5번 틀리면 잠겨요."
                         : "이 코드는 시간이 지났어요. 서버를 다시 시작하면 새 코드가 나와요.")
                        .foregroundStyle(end > Date() ? Theme.muted : Theme.critical)
                }
                HStack {
                    Button("코드 복사") { model.copy(c.code) }
                    Button("설치 화면 열기") { model.openDashboard() }.buttonStyle(BrandButtonStyle())
                    Spacer()
                    Button("새 코드 받기(서버 다시 시작)") { model.restart() }.disabled(!model.ownedByThisApp)
                }
            } else if model.state == .setupNeeded {
                Text("서버 로그에서 설치 코드를 아직 찾지 못했어요. 잠시 뒤 다시 보거나 서버를 다시 시작하세요.")
            } else {
                Text("서버가 설치 모드가 아니에요. 지금 상태: \(model.state.title)")
            }
            Note(text: "코드는 이 Mac의 서버 로그(~/Library/Logs/WithAlbum)에서 읽었어요. 앱은 코드를 어디에도 보내지 않아요. 설치 화면은 이 Mac에서만 열려요.")
        }
        .padding(24)
    }
}

// MARK: - LAN

struct LANView: View {
    @ObservedObject var model: AppModel
    @State private var candidates: [IPAddress] = []
    @State private var selected: IPAddress?
    @State private var port = String(ListenAddress.defaultPort)

    private var address: String? { selected.map { ListenAddress.join($0, port: Int(port) ?? 0) } }
    private var running: Bool { if case .running = model.state { return true } else { return false } }
    private var ready: Bool { running && model.storageRoot != nil && model.ownedByThisApp }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("LAN에서 접속 허용").font(.title2.weight(.bold))
            Text("같은 집 Wi-Fi·유선 LAN의 가족 기기가 이 Mac의 서버에 연결해요. 서버는 -tls -allow-lan으로 다시 시작하고, 앱은 인증서 지문으로만 서버를 믿어요.")
                .foregroundStyle(Theme.muted).fixedSize(horizontal: false, vertical: true)
            if model.lan {
                Text("지금: \(model.exposureText)").font(.headline)
                HStack {
                    Button("기기 연결 정보 보기") { model.show("pairing") }.buttonStyle(BrandButtonStyle())
                    Button("이 Mac에서만 접속으로 되돌리기") { model.disableLAN() }.disabled(model.busy)
                }
            } else {
                check(model.storageRoot != nil, "저장소가 등록되어 있어요")
                check(running, "서버가 정상 실행 중이에요(설치 화면에서 관리자를 만들었어요)")
                Picker("이 Mac의 사설 주소", selection: $selected) {
                    ForEach(candidates, id: \.self) { Text($0.description).tag(Optional($0)) }
                }
                .disabled(candidates.isEmpty)
                if candidates.isEmpty {
                    Text("사설 LAN 주소(192.168.x.x, 10.x.x.x, 172.16~31.x.x, fd00::/8)를 가진 네트워크가 없어요.")
                        .foregroundStyle(Theme.critical)
                }
                TextField("포트", text: $port).frame(width: 120)
                if let a = address, case .failure(let e) = ListenAddress.validateLAN(a) {
                    Text(ListenAddress.message(e)).foregroundStyle(Theme.critical)
                }
                Button("LAN 접속 켜기") { if let a = address { model.enableLAN(a) } }
                    .buttonStyle(BrandButtonStyle())
                    .disabled(!ready || address == nil || model.busy)
            }
            if let m = model.message { Text(m).foregroundStyle(Theme.critical) }
            if model.lan, let p = model.lastProblem { Text("서버가 거부한 이유: \(p)").foregroundStyle(Theme.critical).textSelection(.enabled) }
            Note(text: "공유기에서 이 포트를 포트포워딩하지 마세요. 인터넷 공개는 지원하지 않아요. LAN에서는 평문 HTTP를 쓰지 않고, 로그인할 수 있는 관리자가 없으면 서버가 시작을 거부해요.", warning: true)
        }
        .padding(24)
        .onAppear {
            candidates = ListenAddress.lanCandidates(Interfaces.addresses())
            if selected == nil { selected = candidates.first }
        }
    }

    private func check(_ ok: Bool, _ text: String) -> some View {
        Label(text, systemImage: ok ? "checkmark.circle.fill" : "circle")
            .foregroundStyle(ok ? Theme.positive : Theme.muted)
    }
}

// MARK: - Pairing

struct PairingView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text("기기 연결 정보").font(.title2.weight(.bold))
                if let p = model.pairing {
                    Text("가족 기기의 WithAlbum 앱에서 \"서버 연결\" → QR 스캔으로 이 코드를 읽으세요. 앱은 아래 공개키 지문이 같은 서버에만 연결해요.")
                        .foregroundStyle(Theme.muted).fixedSize(horizontal: false, vertical: true)
                    ForEach(p.pairing, id: \.payload) { e in
                        VStack(alignment: .leading, spacing: 8) {
                            Text(e.url).font(.headline)
                            if let img = QR.image(e.payload) {
                                Image(nsImage: img).interpolation(.none).resizable().frame(width: 220, height: 220)
                                    .accessibilityLabel("\(e.url) 페어링 QR 코드")
                            }
                            Text(e.payload).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                            Button("페어링 문자열 복사") { model.copy(e.payload) }
                        }
                    }
                    Text("공개키 지문(SPKI SHA-256)").font(.headline)
                    Text(TLSPairing.colons(p.spkiSHA256)).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                    Text("인증서 유효 기간: \(p.notAfter)까지").foregroundStyle(Theme.muted)
                } else if let e = model.pairingError, !e.isEmpty {
                    Text(e).foregroundStyle(Theme.critical).textSelection(.enabled)
                } else {
                    Text("불러오는 중…")
                }
                if let f = model.funnelURL {
                    Note(text: "이 QR에는 집 밖 주소(\(f), Tailscale Funnel)도 들어 있어요. 새 앱은 집 Wi-Fi → 집 밖 주소 → 중계 순서로 연결하고, 어느 길이든 같은 공개키 지문으로만 서버를 믿어요. 이미 연결한 기기는 다시 스캔하지 않아도 로그인 뒤 서버에서 이 주소를 배워요.")
                }
                Note(text: "QR에는 서버 주소와 공개키 지문만 들어 있어요(비밀번호·토큰 없음). 지문은 서버를 처음 -tls로 시작할 때 저장소의 .system/tls에 만들어져요.")
            }
            .padding(24)
        }
        .task { await model.loadPairing() }
    }
}

enum QR {
    static func image(_ text: String) -> NSImage? {
        let f = CIFilter.qrCodeGenerator()
        f.message = Data(text.utf8)
        f.correctionLevel = "M"
        guard let out = f.outputImage?.transformed(by: CGAffineTransform(scaleX: 8, y: 8)) else { return nil }
        let rep = NSCIImageRep(ciImage: out)
        let img = NSImage(size: rep.size)
        img.addRepresentation(rep)
        return img
    }
}

// MARK: - Take over

struct TakeOverView: View {
    @ObservedObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("서버 관리 전환").font(.title2.weight(.bold))
            if let a = model.agent {
                Text(model.owner == .otherAppCopy
                     ? "다른 위치의 WithAlbum Server 앱이 서버를 등록했어요(앱을 옮겼거나 이전 버전일 수 있어요)."
                     : "scripts/install-macos.sh 같은 다른 방법으로 등록한 서버가 있어요. 같은 이름(org.withalbum.server)의 자동 시작은 하나만 둘 수 있어요.")
                    .fixedSize(horizontal: false, vertical: true)
                Grid(alignment: .leading, horizontalSpacing: 12, verticalSpacing: 6) {
                    GridRow { Text("실행 파일").foregroundStyle(Theme.muted); Text(a.program).textSelection(.enabled) }
                    GridRow { Text("접속").foregroundStyle(Theme.muted); Text(a.lan ? "LAN · https://\(a.listen)" : "이 Mac에서만 · \(a.listen)") }
                    GridRow { Text("저장 위치").foregroundStyle(Theme.muted); Text(a.environment["WITHALBUM_ROOT"] ?? "설치 화면에서 등록한 저장소(설정 파일)") }
                }
                Text("전환하면 저장 위치·설정 파일·접속 방식은 그대로 두고, 실행 파일만 이 앱에 들어 있는 서버로 바꿔 다시 시작해요. 예전 실행 파일(\(a.program))은 지우지 않아요.")
                    .foregroundStyle(Theme.muted).fixedSize(horizontal: false, vertical: true)
                HStack {
                    Button("이 앱으로 관리 전환") { model.takeOver(); dismiss() }
                        .buttonStyle(BrandButtonStyle())
                        .disabled(!model.canRegisterHere || model.busy)
                    Button("그대로 두기") { dismiss() }
                }
                if !model.canRegisterHere {
                    Text("응용 프로그램 폴더로 옮긴 뒤 앱을 다시 여세요.").foregroundStyle(Theme.critical)
                }
            } else if model.installed != nil {
                Text("~/Library/LaunchAgents/org.withalbum.server.plist를 읽을 수 없어요. 직접 확인한 뒤 지우거나 고치세요. 앱은 이 파일을 덮어쓰지 않아요.")
                    .fixedSize(horizontal: false, vertical: true)
            } else {
                Text("전환할 서버 설정이 없어요.")
            }
        }
        .padding(24)
    }
}
