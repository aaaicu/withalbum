import XCTest
@testable import WithAlbumCore

final class TailscaleTests: XCTestCase {
    // Shapes of `tailscale status --json` (trimmed; names are examples).
    let statusNew = #"""
    {"Version":"1.90.0","BackendState":"Running","Self":{"DNSName":"Mac-Mini.example-tailnet.ts.net.","HostName":"mac-mini",
     "CapMap":{"funnel":null,"https":null,"https://tailscale.com/cap/funnel-ports?ports=443,8443,10000":null}},
     "CurrentTailnet":{"Name":"user@example.com","MagicDNSSuffix":"example-tailnet.ts.net","MagicDNSEnabled":true}}
    """#
    let statusOld = #"{"BackendState":"Running","Self":{"DNSName":"mac-mini.example-tailnet.ts.net.","Capabilities":["https","https://tailscale.com/cap/funnel"]}}"#
    let statusNoFunnel = #"{"BackendState":"Running","Self":{"DNSName":"mac-mini.example-tailnet.ts.net.","CapMap":{"https":null}},"CurrentTailnet":{"MagicDNSEnabled":true}}"#

    func testParseStatus() throws {
        let s = try XCTUnwrap(TailscaleStatus.parse(Data(statusNew.utf8)))
        XCTAssertTrue(s.running)
        XCTAssertEqual(s.dnsName, "mac-mini.example-tailnet.ts.net")
        XCTAssertEqual(s.magicDNS, true)
        XCTAssertEqual(s.funnelAllowed, true)
        XCTAssertEqual(s.httpsAllowed, true)
        let o = try XCTUnwrap(TailscaleStatus.parse(Data(statusOld.utf8)))
        XCTAssertEqual(o.funnelAllowed, true)
        XCTAssertEqual(o.httpsAllowed, true)
        XCTAssertNil(o.magicDNS)
        let n = try XCTUnwrap(TailscaleStatus.parse(Data(statusNoFunnel.utf8)))
        XCTAssertEqual(n.funnelAllowed, false)
        let bare = try XCTUnwrap(TailscaleStatus.parse(Data(#"{"BackendState":"NeedsLogin","Self":{"DNSName":""}}"#.utf8)))
        XCTAssertFalse(bare.running)
        XCTAssertNil(bare.dnsName)
        XCTAssertNil(bare.funnelAllowed, "no capability list: unknown, not false")
        XCTAssertNil(TailscaleStatus.parse(Data("not json".utf8)))
        XCTAssertNil(TailscaleStatus.parse(Data("{}".utf8)))
    }

    func testParseFunnelStatus() throws {
        let host = "mac-mini.example-tailnet.ts.net"
        let on = #"{"TCP":{"443":{"TCPForward":"127.0.0.1:8788","ProxyProtocol":2}},"AllowFunnel":{"mac-mini.example-tailnet.ts.net:443":true}}"#
        XCTAssertEqual(FunnelServeStatus.parse(Data(on.utf8))?.state(host: host), .on(proxyProtocol: true))
        let noProxy = #"{"TCP":{"443":{"TCPForward":"localhost:8788"}},"AllowFunnel":{"Mac-Mini.example-tailnet.ts.net:443":true}}"#
        XCTAssertEqual(FunnelServeStatus.parse(Data(noProxy.utf8))?.state(host: host), .on(proxyProtocol: false))
        let tailnet = #"{"TCP":{"443":{"TCPForward":"127.0.0.1:8788"}}}"#
        XCTAssertEqual(FunnelServeStatus.parse(Data(tailnet.utf8))?.state(host: host), .tailnetOnly)
        let terminated = #"{"TCP":{"443":{"TCPForward":"127.0.0.1:8788","TerminateTLS":"mac-mini.example-tailnet.ts.net"}},"AllowFunnel":{"mac-mini.example-tailnet.ts.net:443":true}}"#
        XCTAssertEqual(FunnelServeStatus.parse(Data(terminated.utf8))?.state(host: host), .terminatesTLS)
        let web = #"{"TCP":{"443":{"HTTPS":true}},"Web":{"mac-mini.example-tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8788"}}}},"AllowFunnel":{"mac-mini.example-tailnet.ts.net:443":true}}"#
        XCTAssertEqual(FunnelServeStatus.parse(Data(web.utf8))?.state(host: host), .webServer)
        let other = #"{"TCP":{"443":{"TCPForward":"127.0.0.1:22"}}}"#
        XCTAssertEqual(FunnelServeStatus.parse(Data(other.utf8))?.state(host: host), .otherTarget("127.0.0.1:22"))
        XCTAssertEqual(FunnelServeStatus.parse(Data("{}".utf8))?.state(host: host), .off)
        XCTAssertEqual(FunnelServeStatus.parse(Data("\n".utf8))?.state(host: host), .off)
        XCTAssertEqual(FunnelServeStatus.parse(Data(on.utf8))?.state(host: host, port: 8443), .off)
        XCTAssertNil(FunnelServeStatus.parse(Data("No serve config".utf8)))
    }

    func testPublicURL() {
        XCTAssertEqual(Funnel.publicURL(dnsName: "Mac-Mini.example-tailnet.ts.net."), "https://mac-mini.example-tailnet.ts.net")
        XCTAssertEqual(Funnel.publicURL(dnsName: "mac-mini.example-tailnet.ts.net", port: 8443), "https://mac-mini.example-tailnet.ts.net:8443")
        XCTAssertNil(Funnel.publicURL(dnsName: "mac-mini.example-tailnet.ts.net", port: 8080))
        XCTAssertNil(Funnel.publicURL(dnsName: "mac-mini.example.org"))
        XCTAssertNil(Funnel.publicURL(dnsName: "tailnet.ts.net"))
        XCTAssertNil(Funnel.publicURL(dnsName: "mac_mini.example-tailnet.ts.net"))
        XCTAssertTrue(Funnel.isValidURL("https://mac-mini.example-tailnet.ts.net"))
        XCTAssertTrue(Funnel.isValidURL("https://mac-mini.example-tailnet.ts.net:10000"))
        for bad in ["http://mac-mini.example-tailnet.ts.net", "https://mac-mini.example-tailnet.ts.net/x", "https://Mac-Mini.example-tailnet.ts.net",
                    "https://mac-mini.example-tailnet.ts.net:443", "https://mac-mini.example-tailnet.ts.net?x=1", "https://example.org"] {
            XCTAssertFalse(Funnel.isValidURL(bad), bad)
        }
    }

    func testCommands() {
        XCTAssertEqual(Funnel.enableArguments(), ["funnel", "--bg", "--yes", "--tcp=443", "--proxy-protocol=2", "tcp://127.0.0.1:8788"])
        XCTAssertEqual(Funnel.disableArguments(), ["funnel", "--tcp=443", "tcp://127.0.0.1:8788", "off"])
        // Raw TCP forwarding only: never the TLS-terminating forms.
        XCTAssertFalse(Funnel.enableArguments().contains { $0.hasPrefix("--https") || $0.hasPrefix("--tls-terminated-tcp") })
    }

    func testDetectCLI() {
        func detect(exec: Set<String>, files: Set<String> = [], links: [String: String] = [:]) -> TailscaleCLI? {
            TailscaleCLI.detect(isExecutable: { exec.contains($0) }, fileExists: { files.contains($0) || exec.contains($0) },
                                resolve: { links[$0] ?? $0 })
        }
        XCTAssertNil(detect(exec: []))
        XCTAssertEqual(detect(exec: ["/opt/homebrew/bin/tailscale"]), TailscaleCLI(path: "/opt/homebrew/bin/tailscale", variant: .openSource))
        XCTAssertEqual(detect(exec: ["/usr/local/bin/tailscale"]), TailscaleCLI(path: "/usr/local/bin/tailscale", variant: .openSource))
        XCTAssertEqual(detect(exec: [TailscaleCLI.appExecutable]), TailscaleCLI(path: TailscaleCLI.appExecutable, variant: .standalone))
        XCTAssertEqual(detect(exec: [TailscaleCLI.appExecutable], files: [TailscaleCLI.appReceipt]),
                       TailscaleCLI(path: TailscaleCLI.appExecutable, variant: .appStore))
        // A /usr/local/bin link the GUI app installed is the app's CLI.
        XCTAssertEqual(detect(exec: ["/usr/local/bin/tailscale", TailscaleCLI.appExecutable],
                              links: ["/usr/local/bin/tailscale": TailscaleCLI.appExecutable]),
                       TailscaleCLI(path: TailscaleCLI.appExecutable, variant: .standalone))
        // The open source CLI wins when both exist.
        XCTAssertEqual(detect(exec: ["/opt/homebrew/bin/tailscale", TailscaleCLI.appExecutable])?.variant, .openSource)
        XCTAssertTrue(TailscaleCLI(path: "/opt/homebrew/bin/tailscale", variant: .openSource).funnelListedAsSupported)
        XCTAssertFalse(TailscaleCLI(path: TailscaleCLI.appExecutable, variant: .appStore).funnelListedAsSupported)
    }

    func testProblems() {
        let policy = "Funnel not available; \"funnel\" node attribute not set. See https://tailscale.com/kb/1223/funnel.\nTo enable, visit:\n\n         https://login.tailscale.com/f/funnel?node=nXXXX\n"
        XCTAssertEqual(TailscaleProblem.classify(policy), .funnelNotAllowed)
        XCTAssertEqual(TailscaleProblem.adminLink(in: policy)?.absoluteString, "https://login.tailscale.com/f/funnel?node=nXXXX")
        XCTAssertNil(TailscaleProblem.adminLink(in: "see https://evil.example/login.tailscale.com/"))
        XCTAssertEqual(TailscaleProblem.classify("failed to connect to local Tailscale daemon for /localapi/v0/status; not running?"), .notRunning)
        XCTAssertEqual(TailscaleProblem.classify("Logged out.\nNeedsLogin"), .loggedOut)
        XCTAssertEqual(TailscaleProblem.classify("HTTPS is not enabled for your tailnet; visit https://login.tailscale.com/f/serve?node=x"), .httpsDisabled)
        if case .other(let s) = TailscaleProblem.classify("  something odd \n") { XCTAssertEqual(s, "something odd") } else { XCTFail() }
    }

    func testAgentWithFunnel() throws {
        var c = AgentConfig(program: "/Applications/WithAlbum Server.app/Contents/Helpers/withalbum-server", logDirectory: "/Users/test/Library/Logs/WithAlbum")
        XCTAssertEqual(c.arguments.count, 1)
        c.setFunnel("https://mac-mini.example-tailnet.ts.net")
        XCTAssertTrue(c.tls)
        XCTAssertEqual(Array(c.arguments.dropFirst()), ["-tls"])
        XCTAssertEqual(c.fullEnvironment["WITHALBUM_FUNNEL_LISTEN"], "127.0.0.1:8788")
        XCTAssertEqual(c.fullEnvironment["WITHALBUM_LISTEN"], "127.0.0.1:8787")
        let back = try XCTUnwrap(ExistingAgent.parse(try c.plistData()))
        XCTAssertTrue(back.tls)
        XCTAssertFalse(back.lan)
        XCTAssertEqual(back.funnelURL, "https://mac-mini.example-tailnet.ts.net")
        // LAN and funnel together: both flags, the funnel stays loopback.
        c.lan = true
        c.listen = "192.168.0.10:8787"
        XCTAssertEqual(Array(c.arguments.dropFirst()), ["-tls", "-allow-lan"])
        XCTAssertNoThrow(try c.plistData())
        c.setFunnel(nil)
        XCTAssertNil(c.fullEnvironment["WITHALBUM_FUNNEL_URL"])
        XCTAssertNil(c.fullEnvironment["WITHALBUM_FUNNEL_LISTEN"])
        // Refused: a non-ts.net URL, a funnel listener off loopback.
        c.lan = false
        c.listen = ListenAddress.loopbackDefault
        c.environment["WITHALBUM_FUNNEL_URL"] = "https://example.org"
        XCTAssertThrowsError(try c.plistData())
        c.setFunnel("https://mac-mini.example-tailnet.ts.net")
        c.environment["WITHALBUM_FUNNEL_LISTEN"] = "192.168.0.10:8788"
        XCTAssertThrowsError(try c.plistData())
        c.environment["WITHALBUM_FUNNEL_LISTEN"] = "127.0.0.1:8787"
        XCTAssertThrowsError(try c.plistData(), "same as WITHALBUM_LISTEN")
        // Take-over keeps a valid funnel and drops an invalid one.
        let kept = ExistingAgent(label: AgentConfig.label, arguments: ["/usr/local/bin/withalbum-server", "-tls"],
                                 environment: ["WITHALBUM_FUNNEL_URL": "https://mac-mini.example-tailnet.ts.net", "WITHALBUM_FUNNEL_LISTEN": "127.0.0.1:8788"])
        XCTAssertEqual(kept.takeOver(helperPath: "/h", logDirectory: "/l").config.arguments, ["/h", "-tls"])
        let dropped = ExistingAgent(label: AgentConfig.label, arguments: ["/x", "-tls"], environment: ["WITHALBUM_FUNNEL_URL": "https://evil.example"])
        XCTAssertEqual(dropped.takeOver(helperPath: "/h", logDirectory: "/l").config.arguments, ["/h"])
    }
}
