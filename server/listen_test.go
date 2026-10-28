package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

// TestCheckListen: a non-loopback address is refused unless -allow-lan, TLS
// and a usable administrator are all present, and only private addresses
// or the wildcard qualify. Loopback is always allowed, also in plain HTTP.
func TestCheckListen(t *testing.T) {
	for _, tt := range []struct {
		addr     string
		allowLAN bool
		tls      bool
		admins   int
		ok       bool
		lan      bool
		missing  string // part of the error
	}{
		// Loopback: plain HTTP or TLS, no further conditions.
		{"127.0.0.1:8787", false, false, 0, true, false, ""},
		{"localhost:8787", false, false, 0, true, false, ""},
		{"[::1]:8787", false, true, 0, true, false, ""},
		{"127.0.0.2:8787", true, false, 0, true, false, ""},
		// LAN: all three conditions.
		{"192.168.0.21:8787", true, true, 1, true, true, ""},
		{"10.1.2.3:8787", true, true, 2, true, true, ""},
		{"172.16.5.5:8787", true, true, 1, true, true, ""},
		{"[fd00::1]:8787", true, true, 1, true, true, ""},
		{"0.0.0.0:8787", true, true, 1, true, true, ""},
		{"[::]:8787", true, true, 1, true, true, ""},
		{":8787", true, true, 1, true, true, ""},
		// One condition missing each.
		{"192.168.0.21:8787", false, true, 1, false, true, "-allow-lan"},
		{"192.168.0.21:8787", true, false, 1, false, true, "TLS"},
		{"192.168.0.21:8787", true, true, 0, false, true, "administrator"},
		{"0.0.0.0:8787", false, true, 1, false, true, "-allow-lan"},
		{"0.0.0.0:8787", true, false, 1, false, true, "TLS"},
		{":8787", true, true, 0, false, true, "administrator"},
		{"[::]:8787", false, false, 0, false, true, "-allow-lan"},
		// Never: public addresses, names, malformed.
		{"203.0.113.7:8787", true, true, 1, false, false, "not a private"},
		{"8.8.8.8:443", true, true, 1, false, false, "not a private"},
		{"[2001:db8::1]:8787", true, true, 1, false, false, "not a private"},
		{"100.64.0.1:8787", true, true, 1, false, false, "not a private"},
		{"169.254.1.1:8787", true, true, 1, false, false, "not a private"},
		{"myhost.local:8787", true, true, 1, false, false, "IP address"},
		{"badaddr", true, true, 1, false, false, "invalid"},
		{"192.168.0.21:", true, true, 1, false, false, "no port"},
	} {
		lan, err := checkListen(listenConfig{Addr: tt.addr, AllowLAN: tt.allowLAN, TLS: tt.tls}, tt.admins)
		if (err == nil) != tt.ok || (err == nil && lan != tt.lan) {
			t.Errorf("checkListen(%q, lan=%v tls=%v admins=%d) = %v, %v; want ok=%v lan=%v", tt.addr, tt.allowLAN, tt.tls, tt.admins, lan, err, tt.ok, tt.lan)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), tt.missing) {
			t.Errorf("checkListen(%q): error %q does not name %q", tt.addr, err, tt.missing)
		}
	}
	// Every missing condition is named at once.
	_, err := checkListen(listenConfig{Addr: "192.168.0.21:8787"}, 0)
	if err == nil || !strings.Contains(err.Error(), "-allow-lan") || !strings.Contains(err.Error(), "TLS") || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("all missing: %v", err)
	}
}

func TestServerURLsAndCertHosts(t *testing.T) {
	ifaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.1.20").To4(), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("fd12::5"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("2001:db8::5"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	for addr, want := range map[string]string{
		"192.168.1.20:8787": "https://192.168.1.20:8787",
		"[fd12::5]:8787":    "https://[fd12::5]:8787",
		"0.0.0.0:8787":      "https://192.168.1.20:8787",
		"[::]:8787":         "https://192.168.1.20:8787 https://[fd12::5]:8787",
		"127.0.0.1:8787":    "https://127.0.0.1:8787",
	} {
		if got := strings.Join(serverURLs(addr, ifaces), " "); got != want {
			t.Errorf("serverURLs(%q) = %q, want %q", addr, got, want)
		}
	}
	none := func() ([]net.Addr, error) { return nil, nil }
	if got := serverURLs(":8787", none); len(got) != 1 || got[0] != "https://127.0.0.1:8787" {
		t.Errorf("serverURLs without interfaces = %v", got)
	}
	if h := certHosts("192.168.1.20:8787"); len(h) != 1 || h[0] != "192.168.1.20" {
		t.Errorf("certHosts = %v", h)
	}
	if h := certHosts("0.0.0.0:8787"); h != nil {
		t.Errorf("certHosts wildcard = %v", h)
	}
}

// TestListenAllowedNeedsUsableAdmin: on a fresh storage (only the locked
// bootstrap account) a LAN listener is refused; after admin init it is
// allowed.
func TestListenAllowedNeedsUsableAdmin(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	lc := listenConfig{Addr: "192.168.0.21:8787", AllowLAN: true, TLS: true}
	a := mustOpenApp(t, root, nil)
	if _, err := a.listenAllowed(t.Context(), lc); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("fresh storage: %v", err)
	}
	if lan, err := a.listenAllowed(t.Context(), listenConfig{Addr: "127.0.0.1:8787"}); err != nil || lan {
		t.Fatalf("loopback on fresh storage: %v %v", lan, err)
	}
	a.Close()
	if r := runAdminPipe("first admin password\n", "init", "-login", "alice"); r.code != 0 {
		t.Fatalf("admin init: %d %s", r.code, r.stderr)
	}
	a = mustOpenApp(t, root, nil)
	defer a.Close()
	if lan, err := a.listenAllowed(t.Context(), lc); err != nil || !lan {
		t.Fatalf("with administrator: %v %v", lan, err)
	}
}

// TestLANOverPinnedTLS runs the real handler behind the server's TLS
// identity and checks what a LAN client sees: a pinned client can read
// health and log in, cookies are Secure, every non-public route is 401
// without a session, security headers are present, a plain HTTP request to
// the TLS port gets no API answer, and a client with another pin cannot
// connect.
func TestLANOverPinnedTLS(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	const pw = "first admin password"
	if r := runAdminPipe(pw+"\n", "init", "-login", "alice"); r.code != 0 {
		t.Fatalf("admin init: %d %s", r.code, r.stderr)
	}
	a := mustOpenApp(t, root, nil)
	defer a.Close()
	id, err := tlsid.LoadOrCreate(a.vol.Root(), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a.api.SetTLSInfo(tlsInfo(id, []string{"https://192.0.2.10:8787"}, remoteRoutes{}))
	srv := httptest.NewUnstartedServer(nil)
	srv.Config = newHTTPServer(a.api.Handler(), tlsid.ServerConfig(id))
	srv.TLS = srv.Config.TLSConfig
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsid.PinnedClientConfig(id.SPKISHA256)}, Timeout: 10 * time.Second}
	type reply struct {
		status int
		header http.Header
		body   map[string]any
	}
	call := func(method, path string, body []byte, hdr map[string]string) reply {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if host := hdr["Host"]; host != "" {
			req.Host = host
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		r := reply{status: resp.StatusCode, header: resp.Header}
		_ = json.Unmarshal(b, &r.body)
		return r
	}

	h := call("GET", "/api/v1/health", nil, nil)
	if h.status != 200 || h.body["status"] != "ok" {
		t.Fatalf("health %d %v", h.status, h.body)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if h.header.Get(k) != v {
			t.Errorf("health %s = %q, want %q", k, h.header.Get(k), v)
		}
	}
	if !strings.Contains(h.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("health CSP %q", h.header.Get("Content-Security-Policy"))
	}
	// The test server's Host is an IP address: no HSTS (browsers ignore it
	// there, and it must never be sent for localhost).
	if h.header.Get("Strict-Transport-Security") != "" {
		t.Errorf("HSTS sent for an IP host")
	}
	if r := call("GET", "/api/v1/health", nil, map[string]string{"Host": "mac-mini.local"}); r.header.Get("Strict-Transport-Security") == "" {
		t.Errorf("no HSTS for a DNS host over TLS")
	}

	// Bearer login (app) and an authenticated call.
	login := call("POST", "/api/v1/auth/login", []byte(`{"login_name":"alice","password":"`+pw+`","device_name":"phone","platform":"ios"}`), map[string]string{"Content-Type": "application/json"})
	token, _ := login.body["access_token"].(string)
	if login.status != 200 || token == "" {
		t.Fatalf("login %d %v", login.status, login.body)
	}
	if r := call("GET", "/api/v1/me", nil, map[string]string{"Authorization": "Bearer " + token}); r.status != 200 {
		t.Fatalf("me %d", r.status)
	}
	tlsReply := call("GET", "/api/v1/admin/server/tls", nil, map[string]string{"Authorization": "Bearer " + token})
	if tlsReply.status != 200 || tlsReply.body["spki_sha256"] != id.SPKIHex() || tlsReply.body["enabled"] != true {
		t.Fatalf("admin tls %d %v", tlsReply.status, tlsReply.body)
	}
	// Browser login: every cookie is Secure over TLS.
	web := call("POST", "/api/v1/auth/login", []byte(`{"login_name":"alice","password":"`+pw+`","device_name":"browser","platform":"web"}`), map[string]string{"Content-Type": "application/json"})
	cookies := web.header.Values("Set-Cookie")
	if web.status != 200 || len(cookies) != 3 {
		t.Fatalf("web login %d %v", web.status, cookies)
	}
	for _, c := range cookies {
		if !strings.Contains(c, "; Secure") || !strings.Contains(c, "SameSite=Strict") {
			t.Errorf("cookie without Secure/SameSite over TLS: %s", c)
		}
	}

	// Every non-public route is 401 without a session.
	param := regexp.MustCompile(`\{[A-Za-z]+\}`)
	public := 0
	for _, rp := range httpapi.Routes() {
		if rp.Access == httpapi.AccessPublic {
			public++
			continue
		}
		method, path, ok := strings.Cut(rp.Pattern, " ")
		if !ok {
			method, path = "GET", rp.Pattern
		}
		path = param.ReplaceAllString(path, "0123456789abcdef0123456789abcdef")
		for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer " + strings.Repeat("A", 43)}} {
			if r := call(method, path, []byte("{}"), hdr); r.status != 401 || r.body["error"] != "unauthorized" {
				t.Errorf("%s %s without session: %d %v", method, path, r.status, r.body)
			}
		}
	}
	if public != 4 {
		t.Fatalf("%d public routes, want 4", public)
	}

	// Plain HTTP to the TLS port: no API answer.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /api/v1/health HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	conn.Close()
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || strings.Contains(string(b), "version") {
			t.Fatalf("plain HTTP on the TLS port: %d %q", resp.StatusCode, b)
		}
	}

	// Another pin: no connection.
	wrong := id.SPKISHA256
	wrong[31] ^= 0xff
	bad := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsid.PinnedClientConfig(wrong)}, Timeout: 10 * time.Second}
	if _, err := bad.Get(srv.URL + "/api/v1/health"); err == nil {
		t.Fatal("client with another pin connected")
	}
	// TLS 1.2 only: refused.
	old := tlsid.PinnedClientConfig(id.SPKISHA256)
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: old}, Timeout: 10 * time.Second}).Get(srv.URL + "/api/v1/health"); err == nil {
		t.Fatal("TLS 1.2 client connected")
	}
}

func TestTLSInfoCommand(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	t.Setenv("WITHALBUM_LISTEN", "192.0.2.10:8787")
	var out, errOut bytes.Buffer
	if code := runTLSInfo(nil, &out, &errOut); code != exitProblems || !strings.Contains(errOut.String(), "-tls") {
		t.Fatalf("before creation: %d %s", code, errOut.String())
	}
	a := mustOpenApp(t, root, nil)
	id, err := tlsid.LoadOrCreate(a.vol.Root(), nil, time.Now())
	a.Close()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := runTLSInfo(nil, &out, &errOut); code != exitOK {
		t.Fatalf("tls: %d %s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, tlsid.Colons(id.SPKIHex())) || !strings.Contains(text, "sha256//"+id.SPKIBase64()) ||
		!strings.Contains(text, tlsid.PairingPayload("https://192.0.2.10:8787", id.SPKIHex())) {
		t.Fatalf("tls output:\n%s", text)
	}
	if strings.Contains(text, "PRIVATE") || strings.Contains(text, "BEGIN") {
		t.Fatalf("tls output contains PEM material:\n%s", text)
	}
	out.Reset()
	if code := runTLSInfo([]string{"-json"}, &out, &errOut); code != exitOK {
		t.Fatalf("tls -json: %d", code)
	}
	var j map[string]any
	if err := json.Unmarshal(out.Bytes(), &j); err != nil || j["spki_sha256"] != id.SPKIHex() {
		t.Fatalf("tls -json: %v %s", err, out.String())
	}
}

func TestLoopbackHostOnly(t *testing.T) {
	h := loopbackHostOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	for host, want := range map[string]int{
		"127.0.0.1:8787": 200, "localhost:8787": 200, "LOCALHOST": 200, "[::1]:8787": 200, "127.0.0.1": 200,
		"evil.example:8787": 421, "evil.example": 421, "192.168.1.20:8787": 421, "": 421,
	} {
		req := httptest.NewRequest("GET", "/api/v1/health", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Host %q: %d, want %d", host, w.Code, want)
		}
	}
}

func TestCheckBoundAddr(t *testing.T) {
	for _, tc := range []struct {
		addr net.Addr
		lan  bool
		ok   bool
	}{
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8787}, false, true},
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 8787}, false, true},
		{&net.TCPAddr{IP: net.IPv4(192, 168, 0, 10), Port: 8787}, false, false},
		{&net.TCPAddr{IP: net.IPv4zero, Port: 8787}, false, false},
		{&net.TCPAddr{IP: net.IPv4(192, 168, 0, 10), Port: 8787}, true, true},
	} {
		if err := checkBoundAddr(tc.addr, tc.lan); (err == nil) != tc.ok {
			t.Errorf("checkBoundAddr(%v, lan=%v) = %v, want ok=%v", tc.addr, tc.lan, err, tc.ok)
		}
	}
}
