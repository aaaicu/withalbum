package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/tlsid"
)

func TestParseFunnelURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://home.example-tailnet.ts.net":      "https://home.example-tailnet.ts.net",
		"https://Home.Example-Tailnet.ts.net/":     "https://home.example-tailnet.ts.net",
		"https://home.example-tailnet.ts.net:443":  "https://home.example-tailnet.ts.net",
		"https://home.example-tailnet.ts.net:8443": "https://home.example-tailnet.ts.net:8443",
		"https://mac-mini.tail1234.ts.net:10000":   "https://mac-mini.tail1234.ts.net:10000",
	} {
		if got, err := parseFunnelURL(in); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "http://home.example-tailnet.ts.net", "https://home.example-tailnet.ts.net:8080",
		"https://home.example-tailnet.ts.net/x", "https://home.example-tailnet.ts.net?a=1", "https://u@home.example-tailnet.ts.net",
		"https://home.example-tailnet.ts.net#f", "https://example.org", "https://ts.net", "https://tailnet.ts.net",
		"https://home.example.com.ts.net.evil.org", "https://-home.example-tailnet.ts.net", "https://ho_me.example-tailnet.ts.net",
		"https://192.0.2.1", "home.example-tailnet.ts.net", "https://home..ts.net"} {
		if got, err := parseFunnelURL(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestCheckFunnel(t *testing.T) {
	tlsOn := listenConfig{Addr: "127.0.0.1:8787", TLS: true}
	const pub = "https://home.example-tailnet.ts.net"
	if got, err := checkFunnel(tlsOn, pub, "127.0.0.1:8788"); err != nil || got != pub {
		t.Fatalf("valid: %q %v", got, err)
	}
	if got, err := checkFunnel(listenConfig{Addr: "127.0.0.1:8787"}, "", "127.0.0.1:8788"); err != nil || got != "" {
		t.Fatalf("off: %q %v", got, err)
	}
	for name, c := range map[string]struct {
		lc     listenConfig
		listen string
		want   string
	}{
		"no tls":       {listenConfig{Addr: "127.0.0.1:8787"}, "127.0.0.1:8788", "-tls"},
		"lan address":  {tlsOn, "192.168.0.10:8788", "loopback"},
		"wildcard":     {tlsOn, "0.0.0.0:8788", "loopback"},
		"empty host":   {tlsOn, ":8788", "loopback"},
		"name":         {tlsOn, "localhost:8788", "loopback"},
		"same as main": {tlsOn, "127.0.0.1:8787", "differ"},
		"no port":      {tlsOn, "127.0.0.1", "want"},
	} {
		if _, err := checkFunnel(c.lc, pub, c.listen); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := checkFunnel(tlsOn, "https://example.org", "127.0.0.1:8788"); err == nil {
		t.Error("non-ts.net accepted")
	}
	if got := funnelCommand("https://h.t.ts.net:8443", "127.0.0.1:8788"); got != "tailscale funnel --bg --tcp=8443 --proxy-protocol=2 tcp://127.0.0.1:8788" {
		t.Errorf("command %q", got)
	}
}

// TestFunnelListenerLoopbackOnly: whatever address it is given, the
// funnel listener refuses to serve on a non-loopback socket.
func TestFunnelListenerLoopbackOnly(t *testing.T) {
	if ln, err := listenFunnel("0.0.0.0:0", false); err == nil {
		ln.Close()
		t.Fatal("wildcard funnel listener accepted")
	}
	ln, err := listenFunnel("127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}

// fakeFunnel stands in for Tailscale Funnel: the relay servers route an
// internet client's TLS connection by the SNI name of its ClientHello
// without terminating TLS, and tailscaled forwards the raw bytes to the
// local target with a PROXY v2 header naming the client. It records the
// bytes it forwards.
type fakeFunnel struct {
	ln       net.Listener
	host     string // the only SNI name served (like the node's ts.net name)
	target   string
	clientIP atomic.Pointer[netip.AddrPort]
	noProxy  atomic.Bool
	seen     *recorder
	mu       sync.Mutex
	snis     []string
}

func newFakeFunnel(t *testing.T, host, target string) *fakeFunnel {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFunnel{ln: ln, host: host, target: target, seen: &recorder{}}
	ip := netip.MustParseAddrPort("203.0.113.7:40001")
	f.clientIP.Store(&ip)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return f
}

var errSNIOnly = errors.New("sni read")

// captureConn records what is read from it (the ClientHello) and
// swallows writes, so that a tls.Server can parse the hello without
// answering.
type captureConn struct {
	net.Conn
	buf bytes.Buffer
}

func (c *captureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.buf.Write(p[:n])
	return n, err
}
func (c *captureConn) Write(p []byte) (int, error) { return len(p), nil }

func (f *fakeFunnel) handle(c net.Conn) {
	defer c.Close()
	cc := &captureConn{Conn: c}
	var sni string
	tls.Server(cc, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni = h.ServerName
		return nil, errSNIOnly
	}}).Handshake()
	f.mu.Lock()
	f.snis = append(f.snis, sni)
	f.mu.Unlock()
	if sni != f.host {
		return // Funnel only proxies names it serves
	}
	up, err := net.Dial("tcp", f.target)
	if err != nil {
		return
	}
	defer up.Close()
	if !f.noProxy.Load() {
		up.Write(proxyV2(*f.clientIP.Load()))
	}
	f.seen.add(cc.buf.Bytes())
	up.Write(cc.buf.Bytes())
	done := make(chan struct{}, 2)
	go func() { io.Copy(recordingConn{up, f.seen}, c); done <- struct{}{} }()
	go func() { io.Copy(c, recordingConn{up, f.seen}); done <- struct{}{} }()
	<-done
}

func proxyV2(src netip.AddrPort) []byte {
	b := []byte("\r\n\r\n\x00\r\nQUIT\n\x21\x11\x00\x0c")
	ip := src.Addr().As4()
	b = append(b, ip[:]...)
	b = append(b, 127, 0, 0, 1)
	b = binary.BigEndian.AppendUint16(b, src.Port())
	return binary.BigEndian.AppendUint16(b, 443)
}

func (f *fakeFunnel) setClient(s string) {
	ip := netip.MustParseAddrPort(s)
	f.clientIP.Store(&ip)
}

// funnelApp is what the app does on the funnel route: TCP to the public
// host, TLS with SNI = the ts.net name, trust = the paired SPKI pin.
func funnelApp(addr, sni string, pin [32]byte) *http.Client {
	cfg := tlsid.PinnedClientConfig(pin)
	cfg.ServerName = sni
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(raw, cfg.Clone())
			if err := tc.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return tc, nil
		},
	}}
}

type funnelFixture struct {
	a      *app
	id     *tlsid.Identity
	funnel *fakeFunnel
	direct http.Handler
	pin    [32]byte
}

const (
	funnelHost = "home.example-tailnet.ts.net"
	funnelPub  = "https://" + funnelHost
	bobPass    = "bob password 123"
)

func newFunnelFixture(t *testing.T) *funnelFixture {
	t.Helper()
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	if r := runAdminPipe(relayTestPassword+"\n", "init", "-login", "alice"); r.code != 0 {
		t.Fatalf("admin init: %d %s", r.code, r.stderr)
	}
	if r := runAdminPipe(bobPass+"\n", "create-user", "-login", "bob"); r.code != 0 {
		t.Fatalf("create bob: %d %s", r.code, r.stderr)
	}
	a := mustOpenApp(t, root, nil)
	t.Cleanup(func() { a.Close() })
	id, err := tlsid.LoadOrCreate(a.vol.Root(), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := checkFunnel(listenConfig{Addr: "127.0.0.1:8787", TLS: true}, funnelPub, "127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.setupFunnel(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	rr := remoteRoutes{Public: pub}
	urls := pairingURLs("127.0.0.1:8787", pub, nil)
	a.api.SetTLSInfo(tlsInfo(id, urls, rr))
	handler := a.api.Handler()
	ln, err := listenFunnel("127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	srv := serveFunnel(ln, handler, tlsid.ServerConfig(id))
	t.Cleanup(func() { srv.Close() })
	f := &funnelFixture{a: a, id: id, direct: handler, pin: id.SPKISHA256}
	f.funnel = newFakeFunnel(t, funnelHost, ln.Addr().String())
	return f
}

func (f *funnelFixture) app() *http.Client {
	return funnelApp(f.funnel.ln.Addr().String(), funnelHost, f.pin)
}

func (f *funnelFixture) login(t *testing.T, c *http.Client, login, pw, device string) (access, refresh string) {
	t.Helper()
	r := call(t, c, "", "POST", "/api/v1/auth/login",
		[]byte(`{"login_name":"`+login+`","password":"`+pw+`","device_name":"`+device+`","platform":"ios"}`),
		map[string]string{"Content-Type": "application/json"})
	access, _ = r.body["access_token"].(string)
	refresh, _ = r.body["refresh_token"].(string)
	if r.status != 200 || access == "" {
		t.Fatalf("login %s: %d %s", login, r.status, r.raw)
	}
	return access, refresh
}

// TestFunnelEndToEnd: an app reaches the server only through the
// (simulated) Funnel, with the server's own certificate and pin end to
// end. Administration is refused, logins are audited with the route and
// the client IP from the PROXY header, and what the forwarder handled is
// ciphertext only.
func TestFunnelEndToEnd(t *testing.T) {
	f := newFunnelFixture(t)

	// The pairing payload of a loopback-only server names the public URL
	// as url (old apps) and as public_url with its route (new apps).
	info := serveJSON(f.direct, "", "GET", "/api/v1/health", "")
	if info.Code != 200 {
		t.Fatalf("direct health %d", info.Code)
	}
	payload := tlsid.PairingPayloadWith(funnelPub, f.id.SPKIHex(), remoteRoutes{Public: funnelPub}.extras())
	u, _ := url.Parse(payload)
	q := u.Query()
	if q.Get("v") != "1" || q.Get("url") != funnelPub || q.Get("public_url") != funnelPub || q.Get("public_route") != "funnel" {
		t.Fatalf("payload %s", payload)
	}

	app := f.app()
	if h := call(t, app, "", "GET", "/api/v1/health", nil, nil); h.status != 200 {
		t.Fatalf("health %d %s", h.status, h.raw)
	}
	token, _ := f.login(t, app, "alice", relayTestPassword, "phone on LTE")
	me := call(t, app, token, "GET", "/api/v1/me", nil, nil)
	pubv, _ := me.body["public"].(map[string]any)
	if me.status != 200 || pubv["url"] != funnelPub || pubv["route"] != "funnel" || me.body["connection_route"] != "funnel" {
		t.Fatalf("me %d %s", me.status, me.raw)
	}
	if d := serveJSON(f.direct, token, "GET", "/api/v1/me", ""); !strings.Contains(d.Body.String(), `"connection_route":"direct"`) {
		t.Fatalf("direct me %s", d.Body)
	}
	// Administration and the dashboard: refused, also for the
	// administrator, also for paths that are not canonical.
	for _, p := range []string{"/api/v1/admin/server/tls", "/api/v1/admin/users", "/", "/setup", "/api/v1/setup/storage", "/api/v1/folders/../admin/users"} {
		if r := call(t, app, token, "GET", p, nil, nil); r.status != 403 || r.body["error"] != "not_available_via_relay" || r.body["route"] != "funnel" {
			t.Errorf("%s through the funnel: %d %s", p, r.status, r.raw)
		}
	}
	// Unauthenticated app routes: 401.
	for _, p := range []string{"/api/v1/folders", "/api/v1/me", "/api/v1/invitations", "/api/v1/me/devices"} {
		if r := call(t, app, "", "GET", p, nil, nil); r.status != 401 {
			t.Errorf("%s without a token: %d", p, r.status)
		}
	}
	if r := call(t, app, "not-a-token", "GET", "/api/v1/folders", nil, nil); r.status != 401 {
		t.Errorf("bad token: %d", r.status)
	}

	// Upload and download an original.
	folder := call(t, app, token, "POST", "/api/v1/folders", []byte(`{"name":"Trip"}`), map[string]string{"Content-Type": "application/json"})
	folderID, _ := folder.body["id"].(string)
	if folder.status != 201 {
		t.Fatalf("folder %d %s", folder.status, folder.raw)
	}
	marker := "FUNNEL-PLAINTEXT-" + rand.Text()
	photo := append(testPhoto(t, 3), []byte(strings.Repeat(marker, 2000))...)
	sum := sha256.Sum256(photo)
	up := call(t, app, token, "POST", "/api/v1/folders/"+folderID+"/assets", photo, map[string]string{"Idempotency-Key": "funnel-1"})
	assetID, _ := up.body["asset_id"].(string)
	if up.status != 201 || up.body["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("upload %d %s", up.status, up.raw)
	}
	orig := call(t, app, token, "GET", "/api/v1/folders/"+folderID+"/assets/"+assetID+"/original", nil, nil)
	if orig.status != 200 || !bytes.Equal(orig.raw, photo) {
		t.Fatalf("download %d", orig.status)
	}

	// The forwarder saw only ciphertext (a real Funnel and tailscaled see
	// the same: the TLS session ends in this server).
	seen := f.funnel.seen.bytes()
	if len(seen) < 2*len(photo) {
		t.Fatalf("forwarder recorded only %d bytes", len(seen))
	}
	for what, secret := range map[string]string{"photo marker": marker, "password": relayTestPassword, "token": token, "login": `"alice"`, "path": "/api/v1/"} {
		if bytes.Contains(seen, []byte(secret)) {
			t.Errorf("the forwarder saw the %s in plaintext", what)
		}
	}
	// The ClientHello named the ts.net host (that is what Funnel routes on).
	f.funnel.mu.Lock()
	snis := append([]string{}, f.funnel.snis...)
	f.funnel.mu.Unlock()
	if len(snis) == 0 || snis[0] != funnelHost {
		t.Fatalf("SNI %v", snis)
	}

	// The login is audited with the route and the client IP of the header.
	var details string
	db := f.a.meta.(interface{ DB() *sql.DB }).DB()
	if err := db.QueryRow("SELECT details_json FROM audit_log WHERE action = 'auth.login' AND result = 'allowed' ORDER BY occurred_at DESC LIMIT 1").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"route":"funnel"`) || !strings.Contains(details, `"ip":"203.0.113.7"`) {
		t.Fatalf("login audit %s", details)
	}

	// The admin view on the direct listener names the public route.
	adminTLS := serveJSON(f.direct, token, "GET", "/api/v1/admin/server/tls", "")
	if adminTLS.Code != 200 || !strings.Contains(adminTLS.Body.String(), `"public":{"route":"funnel","url":"`+funnelPub+`"}`) ||
		!strings.Contains(adminTLS.Body.String(), "public_url=") {
		t.Fatalf("admin tls %d %s", adminTLS.Code, adminTLS.Body)
	}
}

// TestFunnelTrust: a wrong pin, or a ClientHello for another name, gets
// nowhere.
func TestFunnelTrust(t *testing.T) {
	f := newFunnelFixture(t)
	wrong := f.pin
	wrong[0] ^= 1
	req, _ := http.NewRequest("GET", "https://withalbum.invalid/api/v1/health", nil)
	if _, err := funnelApp(f.funnel.ln.Addr().String(), funnelHost, wrong).Do(req); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("wrong pin: %v", err)
	}
	req, _ = http.NewRequest("GET", "https://withalbum.invalid/api/v1/health", nil)
	if _, err := funnelApp(f.funnel.ln.Addr().String(), "other.example-tailnet.ts.net", f.pin).Do(req); err == nil {
		t.Fatal("other SNI name served")
	}
}

// TestFunnelAccessControl is §7 of the external access proposal over the
// public route: expired invitations, another album's ID, a lost device.
func TestFunnelAccessControl(t *testing.T) {
	f := newFunnelFixture(t)
	app := f.app()
	alice, _ := f.login(t, app, "alice", relayTestPassword, "alice phone")
	bob, _ := f.login(t, app, "bob", bobPass, "bob phone")
	bobMe := call(t, app, bob, "GET", "/api/v1/me", nil, nil)
	bobID, _ := bobMe.body["user"].(map[string]any)["user_id"].(string)
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	folder := call(t, app, alice, "POST", "/api/v1/folders", []byte(`{"name":"Family"}`), jsonHdr)
	folderID, _ := folder.body["id"].(string)
	up := call(t, app, alice, "POST", "/api/v1/folders/"+folderID+"/assets", testPhoto(t, 9), map[string]string{"Idempotency-Key": "ac-1"})
	assetID, _ := up.body["asset_id"].(string)
	if folder.status != 201 || up.status != 201 {
		t.Fatalf("setup %d %d", folder.status, up.status)
	}

	// Another album's IDs: 404 for a non-member, for every kind of route.
	for _, p := range []string{"/api/v1/folders/" + folderID, "/api/v1/folders/" + folderID + "/assets",
		"/api/v1/folders/" + folderID + "/assets/" + assetID + "/original", "/api/v1/folders/" + folderID + "/assets/" + assetID + "/preview",
		"/api/v1/folders/" + folderID + "/sync"} {
		if r := call(t, app, bob, "GET", p, nil, nil); r.status != 404 {
			t.Errorf("bob %s: %d %s", p, r.status, r.raw)
		}
	}
	if r := call(t, app, bob, "POST", "/api/v1/folders/"+folderID+"/assets", testPhoto(t, 10), map[string]string{"Idempotency-Key": "ac-2"}); r.status != 404 {
		t.Errorf("bob upload into alice's album: %d", r.status)
	}

	// An expired invitation cannot be accepted (410); a valid one can, by
	// its addressee only.
	inv := call(t, app, alice, "POST", "/api/v1/folders/"+folderID+"/invitations", []byte(`{"invited_user_id":"`+bobID+`","role":"VIEWER"}`), jsonHdr)
	invID, _ := inv.body["id"].(string)
	if inv.status != 201 || invID == "" {
		t.Fatalf("invite %d %s", inv.status, inv.raw)
	}
	db := f.a.meta.(interface{ DB() *sql.DB }).DB()
	if _, err := db.Exec("UPDATE folder_invitations SET expires_at = ? WHERE invitation_id = ?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), invID); err != nil {
		t.Fatal(err)
	}
	if r := call(t, app, bob, "POST", "/api/v1/invitations/"+invID+"/accept", nil, nil); r.status != 410 {
		t.Fatalf("expired accept: %d %s", r.status, r.raw)
	}
	inv = call(t, app, alice, "POST", "/api/v1/folders/"+folderID+"/invitations", []byte(`{"invited_user_id":"`+bobID+`","role":"VIEWER"}`), jsonHdr)
	invID, _ = inv.body["id"].(string)
	if r := call(t, app, alice, "POST", "/api/v1/invitations/"+invID+"/accept", nil, nil); r.status != 403 {
		t.Errorf("accept by the owner (not the addressee): %d", r.status)
	}
	if r := call(t, app, "", "POST", "/api/v1/invitations/"+invID+"/accept", nil, nil); r.status != 401 {
		t.Errorf("accept without a token: %d", r.status)
	}
	if r := call(t, app, bob, "POST", "/api/v1/invitations/"+invID+"/accept", nil, nil); r.status != 200 {
		t.Fatalf("accept: %d %s", r.status, r.raw)
	}
	if r := call(t, app, bob, "GET", "/api/v1/folders/"+folderID+"/assets/"+assetID+"/original", nil, nil); r.status != 200 {
		t.Errorf("member download: %d", r.status)
	}
	if r := call(t, app, bob, "POST", "/api/v1/folders/"+folderID+"/assets", testPhoto(t, 11), map[string]string{"Idempotency-Key": "ac-3"}); r.status != 403 {
		t.Errorf("viewer upload: %d", r.status)
	}

	// A lost phone: bob signs out his other devices from a new one.
	lostAccess, lostRefresh := f.login(t, app, "bob", bobPass, "bob old phone")
	newAccess, _ := f.login(t, app, "bob", bobPass, "bob new phone")
	devs := call(t, app, newAccess, "GET", "/api/v1/me/devices", nil, nil)
	if list, _ := devs.body["devices"].([]any); devs.status != 200 || len(list) != 3 {
		t.Fatalf("devices %d %s", devs.status, devs.raw)
	}
	rev := call(t, app, newAccess, "POST", "/api/v1/me/devices/revoke-others", nil, nil)
	if n, _ := rev.body["revoked"].(float64); rev.status != 200 || n != 2 {
		t.Fatalf("revoke others %d %s", rev.status, rev.raw)
	}
	for _, tok := range []string{lostAccess, bob} {
		if r := call(t, app, tok, "GET", "/api/v1/folders", nil, nil); r.status != 401 {
			t.Errorf("revoked device still works: %d", r.status)
		}
	}
	if r := call(t, app, "", "POST", "/api/v1/auth/refresh", []byte(`{"refresh_token":"`+lostRefresh+`"}`), jsonHdr); r.status != 401 {
		t.Errorf("revoked refresh: %d", r.status)
	}
	if r := call(t, app, newAccess, "GET", "/api/v1/folders", nil, nil); r.status != 200 {
		t.Errorf("current device signed out: %d", r.status)
	}
	if r := call(t, app, alice, "GET", "/api/v1/folders", nil, nil); r.status != 200 {
		t.Errorf("another account signed out: %d", r.status)
	}
	if devs := call(t, app, newAccess, "GET", "/api/v1/me/devices", nil, nil); !strings.Contains(string(devs.raw), `"current":true`) || strings.Count(string(devs.raw), `"device_id"`) != 1 {
		t.Errorf("devices after revoke %s", devs.raw)
	}
	var details string
	if err := db.QueryRow("SELECT details_json FROM audit_log WHERE action = 'device.revoke_others'").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"revoked":2`) || !strings.Contains(details, `"route":"funnel"`) {
		t.Errorf("audit %s", details)
	}
}

// TestFunnelLoginLimits: with the PROXY header, login limits are per
// internet client; without it, every funnel client shares one key, which
// then limits the whole route.
func TestFunnelLoginLimits(t *testing.T) {
	f := newFunnelFixture(t)
	app := f.app()
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	try := func(login, pw string) int {
		return call(t, app, "", "POST", "/api/v1/auth/login",
			[]byte(`{"login_name":"`+login+`","password":"`+pw+`","device_name":"d","platform":"ios"}`), jsonHdr).status
	}
	f.funnel.setClient("203.0.113.50:1000")
	for i := range 5 {
		if s := try("bob", "wrong password "+string(rune('a'+i))); s != 401 {
			t.Fatalf("attempt %d: %d", i, s)
		}
	}
	if s := try("bob", bobPass); s != 429 {
		t.Fatalf("after 5 failures from one client: %d", s)
	}
	// Another internet client is not blocked by the first one.
	app.CloseIdleConnections()
	f.funnel.setClient("198.51.100.60:2000")
	if s := try("bob", bobPass); s != 200 {
		t.Fatalf("other client: %d", s)
	}
	// No PROXY header: one shared bucket for the route (burst 10).
	app.CloseIdleConnections()
	f.funnel.noProxy.Store(true)
	codes := map[int]int{}
	for range 12 {
		codes[try("nobody", "x")]++
	}
	if codes[429] == 0 {
		t.Fatalf("no limit for the whole route without client addresses: %v", codes)
	}
	var ips string
	db := f.a.meta.(interface{ DB() *sql.DB }).DB()
	if err := db.QueryRow("SELECT group_concat(details_json) FROM audit_log WHERE action = 'auth.login_throttled'").Scan(&ips); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ips, `"ip":"203.0.113.50"`) || !strings.Contains(ips, `"ip":"funnel-unknown"`) {
		t.Errorf("throttle audit %s", ips)
	}
}

func TestFunnelNeedsAdministrator(t *testing.T) {
	root := newRoot(t)
	a := mustOpenApp(t, root, nil)
	defer a.Close()
	if err := a.setupFunnel(t.Context(), funnelPub); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("setupFunnel without administrator: %v", err)
	}
}

func TestTLSInfoCommandWithFunnel(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	t.Setenv("WITHALBUM_LISTEN", "127.0.0.1:8787")
	t.Setenv("WITHALBUM_FUNNEL_URL", "https://Home.Example-Tailnet.ts.net:443")
	a := mustOpenApp(t, root, nil)
	id, err := tlsid.LoadOrCreate(a.vol.Root(), nil, time.Now())
	a.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runTLSInfo([]string{"-json"}, &out, &errOut); code != exitOK {
		t.Fatalf("tls -json: %d %s", code, errOut.String())
	}
	want := tlsid.PairingPayloadWith(funnelPub, id.SPKIHex(), tlsid.PairingExtras{PublicURL: funnelPub, PublicRoute: "funnel"})
	if !strings.Contains(out.String(), `"url": "`+funnelPub+`"`) || !strings.Contains(out.String(), strings.ReplaceAll(want, "&", `\`+"u0026")) {
		t.Fatalf("tls -json:\n%s\nwant payload %s", out.String(), want)
	}
	t.Setenv("WITHALBUM_FUNNEL_URL", "https://example.org")
	if code := runTLSInfo(nil, &out, &errOut); code != exitUsage {
		t.Fatalf("bad funnel url: %d", code)
	}
	// With a LAN listener the direct URL stays the LAN one.
	if got := pairingURLs("192.168.0.10:8787", funnelPub, nil); len(got) != 1 || got[0] != "https://192.168.0.10:8787" {
		t.Fatalf("lan pairing urls %v", got)
	}
}
