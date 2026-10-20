import AppKit
import SwiftUI
import WebKit
import WithAlbumCore

/// The built-in dashboard while the server serves only the LAN (TLS): the
/// self-signed certificate is accepted only when its key matches the pin
/// read from this Mac's storage, and the view never leaves that origin.
/// On loopback the app opens the dashboard in the default browser instead.
struct DashboardWindow: View {
    @ObservedObject var model: AppModel

    var body: some View {
        if model.lan, let base = model.baseURL, let pin = model.pairing?.spkiSHA256 {
            PinnedWebView(url: base, pin: pin)
        } else if model.lan {
            Text("서버의 공개키 지문을 읽는 중이에요…").padding(24)
                .task { await model.loadPairing() }
        } else {
            VStack(spacing: 12) {
                Text("이 Mac에서만 접속하는 동안에는 브라우저에서 관리 화면을 열어요.")
                Button("브라우저에서 열기") { model.openDashboard() }
            }.padding(24)
        }
    }
}

struct PinnedWebView: NSViewRepresentable {
    let url: URL
    let pin: String

    func makeCoordinator() -> Coordinator { Coordinator(origin: url, pin: pin) }

    func makeNSView(context: Context) -> WKWebView {
        let cfg = WKWebViewConfiguration()
        cfg.websiteDataStore = .default()
        let v = WKWebView(frame: .zero, configuration: cfg)
        v.navigationDelegate = context.coordinator
        v.load(URLRequest(url: url))
        return v
    }

    func updateNSView(_ nsView: WKWebView, context: Context) {}

    final class Coordinator: NSObject, WKNavigationDelegate {
        let origin: URL
        let pin: String
        init(origin: URL, pin: String) {
            self.origin = origin
            self.pin = pin
        }

        private func sameOrigin(_ u: URL) -> Bool {
            u.scheme == origin.scheme && u.host == origin.host && u.port == origin.port
        }

        func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            guard let u = action.request.url else { return decisionHandler(.cancel) }
            if sameOrigin(u) || ["about", "blob", "data"].contains(u.scheme ?? "") {
                return decisionHandler(.allow)
            }
            if u.scheme == "https" || u.scheme == "http" { NSWorkspace.shared.open(u) }
            decisionHandler(.cancel)
        }

        func webView(_ webView: WKWebView, didReceive challenge: URLAuthenticationChallenge,
                     completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
            let ps = challenge.protectionSpace
            guard ps.host == origin.host, ps.port == origin.port else {
                return completionHandler(.cancelAuthenticationChallenge, nil)
            }
            let (d, c) = PinnedTrust.decide(challenge, pin: pin)
            completionHandler(d, c)
        }
    }
}
