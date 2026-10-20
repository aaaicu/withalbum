package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/relay"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

func TestCheckRelay(t *testing.T) {
	for _, c := range []struct {
		addr string
		tls  bool
		err  string
	}{
		{"", false, ""},
		{"relay.example.org:443", true, ""},
		{"203.0.113.5:8443", true, ""},
		{"[2001:db8::1]:443", true, ""},
		{"relay.example.org:443", false, "-tls"},
		{"relay.example.org", true, "host:port"},
		{"https://relay.example.org:443", true, "host:port"},
		{"relay.example.org:0", true, "port"},
		{"relay.example.org:+443", true, "port"},
		{"relay.example.org:70000", true, "port"},
		{"relay..example.org:443", true, "host name"},
		{"relay_example.org:443", true, "host name"},
		{"relay.example.org/x:443", true, "host name"},
		{":443", true, "host name"},
	} {
		err := checkRelay(listenConfig{Addr: "127.0.0.1:8787", TLS: c.tls}, c.addr)
		if (err == nil) != (c.err == "") || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("checkRelay(%q, tls=%v) = %v, want %q", c.addr, c.tls, err, c.err)
		}
	}
}

// TestRelayAllowed: through the relay only the app routes are served; the
// dashboard and every administration route are not, whatever the spelling.
func TestRelayAllowed(t *testing.T) {
	for _, p := range []string{"/api/v1/health", "/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/me", "/api/v1/me/devices/x",
		"/api/v1/folders", "/api/v1/folders/f/assets/a/original", "/api/v1/invitations", "/api/v1/invitations/i/accept"} {
		if !relayAllowed(p) {
			t.Errorf("%s refused", p)
		}
	}
	for _, p := range []string{"", "/", "/index.html", "/api/v1/admin/users", "/api/v1/admin/server/tls", "/api/v1/admin",
		"/api/v1/folders/../admin/users", "/api/v1/folders//x", "/api/v1/folders/x/", "/api/v1/mexico", "/api/v1/foldersx",
		"/api/v2/folders", "/setup", "/api/v1/setup/storage", "/api/v1/"} {
		if relayAllowed(p) {
			t.Errorf("%s allowed", p)
		}
	}
	// Every declared route is either an app route (served) or an
	// administration route or the dashboard (refused): no route is added
	// to the relay without a decision here.
	for _, rp := range httpapi.Routes() {
		_, p, ok := strings.Cut(rp.Pattern, " ")
		if !ok {
			p = rp.Pattern
		}
		p = strings.ReplaceAll(strings.ReplaceAll(p, "{$}", ""), "{", "")
		p = strings.ReplaceAll(p, "}", "")
		admin := strings.HasPrefix(p, "/api/v1/admin/") || p == "/"
		if p == "/api/v1/" {
			continue // the catch-all
		}
		if relayAllowed(p) == admin {
			t.Errorf("route %s: relayAllowed=%v", rp.Pattern, relayAllowed(p))
		}
	}
}

// recorder keeps every byte that crossed the relay at one end.
type recorder struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (r *recorder) add(p []byte) {
	r.mu.Lock()
	r.b.Write(p)
	r.mu.Unlock()
}

func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.b.Bytes())
}

type recordingConn struct {
	net.Conn
	rec *recorder
}

func (c recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.rec.add(p[:n])
	return n, err
}

func (c recordingConn) Write(p []byte) (int, error) {
	c.rec.add(p)
	return c.Conn.Write(p)
}

type recordingListener struct {
	net.Listener
	rec *recorder
}

func (l recordingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return recordingConn{c, l.rec}, nil
}

// relayPKI writes a private CA (as WITHALBUM_RELAY_CA_FILE) and returns a
// certificate for localhost signed by it: the relay's outer identity.
func relayPKI(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test relay CA"}, IsCA: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	caFile := filepath.Join(t.TempDir(), "relay-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITHALBUM_RELAY_CA_FILE", caFile)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// relayFixture is a relay, a home server connected to it and what both
// ends of the relay saw.
type relayFixture struct {
	a         *app
	id        *tlsid.Identity
	relay     *relay.Relay
	relayAddr string
	pool      *x509.CertPool
	client    *relay.Client
	direct    http.Handler // the same API as on the LAN listener
	homeSide  *recorder
	appSide   *recorder // under the app's TLS: what the relay handles
	appPlain  *recorder // above it: what the app sent and received
	relayLogs *bytes.Buffer
	logMu     *sync.Mutex
}

const relayTestPassword = "relay test password"

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	if r := runAdminPipe(relayTestPassword+"\n", "init", "-login", "alice"); r.code != 0 {
		t.Fatalf("admin init: %d %s", r.code, r.stderr)
	}
	a := mustOpenApp(t, root, nil)
	t.Cleanup(func() { a.Close() })
	id, err := tlsid.LoadOrCreate(a.vol.Root(), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, pool := relayPKI(t)
	f := &relayFixture{a: a, id: id, pool: pool, homeSide: &recorder{}, appSide: &recorder{}, appPlain: &recorder{}, relayLogs: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	f.relay = relay.New(relay.Config{Logger: slog.New(slog.NewJSONHandler(lockedWriter{f.logMu, f.relayLogs}, nil))},
		&tls.Config{Certificates: []tls.Certificate{cert}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go f.relay.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.relay.Shutdown(ctx)
	})
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f.relayAddr = "localhost:" + port

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.client, err = a.setupRelay(ctx, f.relayAddr, id)
	if err != nil {
		t.Fatal(err)
	}
	rt := relayTargetFor(id, f.relayAddr)
	a.api.SetTLSInfo(tlsInfo(id, []string{"https://192.0.2.10:8787"}, rt))
	handler := a.api.Handler()
	f.direct = handler
	// serveRelay, with a recorder under the server's TLS.
	srv := newHTTPServer(relayOnly(handler), tlsid.ServerConfig(id))
	go f.client.Run(ctx)
	go srv.ServeTLS(recordingListener{f.client, f.homeSide}, "", "")
	t.Cleanup(func() { srv.Close() })
	f.waitConnected(t)
	return f
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (f *relayFixture) logs() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return f.relayLogs.String()
}

func (f *relayFixture) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f.client.Status().Connected || f.relay.Stats().Tunnels == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("tunnel not up: %+v", f.client.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// appClient is what the app does: connect to the relay with the
// rendezvous ID from the pairing payload, then run TLS pinned to the
// server's key over that connection.
func (f *relayFixture) appClient(pin [32]byte, rid string) *http.Client {
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := relay.Dial(ctx, f.relayAddr, rid, &tls.Config{RootCAs: f.pool})
			if err != nil {
				return nil, err
			}
			tc := tls.Client(recordingConn{raw, f.appSide}, tlsid.PinnedClientConfig(pin))
			if err := tc.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return recordingConn{tc, f.appPlain}, nil
		},
		ForceAttemptHTTP2: false,
	}
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

type apiReply struct {
	status int
	body   map[string]any
	raw    []byte
	header http.Header
}

func call(t *testing.T, c *http.Client, token, method, path string, body []byte, hdr map[string]string) apiReply {
	t.Helper()
	// The host is never resolved: the connection always goes to the relay.
	req, err := http.NewRequest(method, "https://withalbum.invalid"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := apiReply{status: resp.StatusCode, raw: raw, header: resp.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

// TestRelayEndToEnd: an app reaches the home server only through the
// relay, logs in, uploads an original with a resumable session across a
// tunnel drop, downloads it back, and nothing readable ever crosses the
// relay: the bytes both ends exchanged with it contain neither the photo,
// nor the password, nor the access token.
func TestRelayEndToEnd(t *testing.T) {
	f := newRelayFixture(t)
	rt := relayTargetFor(f.id, f.relayAddr)

	// The pairing payload names the relay and the ID; the app reads both.
	payload := tlsid.PairingPayloadRelay("https://192.0.2.10:8787", f.id.SPKIHex(), rt.Addr, rt.RID)
	u, err := url.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("v") != "1" || q.Get("relay") != f.relayAddr || q.Get("rid") != f.client.RendezvousID() || q.Get("spki_sha256") != f.id.SPKIHex() {
		t.Fatalf("payload %s", payload)
	}
	pinBytes, _ := hex.DecodeString(q.Get("spki_sha256"))
	var pin [32]byte
	copy(pin[:], pinBytes)
	if relay.RendezvousID(pin) != q.Get("rid") {
		t.Fatal("rid is not derived from the pin")
	}
	app := f.appClient(pin, q.Get("rid"))

	h := call(t, app, "", "GET", "/api/v1/health", nil, nil)
	if h.status != 200 || h.body["status"] != "ok" {
		t.Fatalf("health %d %s", h.status, h.raw)
	}
	login := call(t, app, "", "POST", "/api/v1/auth/login",
		[]byte(`{"login_name":"alice","password":"`+relayTestPassword+`","device_name":"phone on LTE","platform":"ios"}`),
		map[string]string{"Content-Type": "application/json"})
	token, _ := login.body["access_token"].(string)
	if login.status != 200 || token == "" {
		t.Fatalf("login %d %s", login.status, login.raw)
	}
	me := call(t, app, token, "GET", "/api/v1/me", nil, nil)
	if rel, _ := me.body["relay"].(map[string]any); me.status != 200 || rel["endpoint"] != f.relayAddr || rel["rendezvous_id"] != rt.RID {
		t.Fatalf("me %d %s", me.status, me.raw)
	}
	// Administration is not served through the relay, even to the
	// administrator; it is on the direct listener.
	for _, p := range []string{"/api/v1/admin/server/tls", "/api/v1/admin/users", "/"} {
		if r := call(t, app, token, "GET", p, nil, nil); r.status != 403 || r.body["error"] != "not_available_via_relay" {
			t.Errorf("%s through the relay: %d %s", p, r.status, r.raw)
		}
	}
	folder := call(t, app, token, "POST", "/api/v1/folders", []byte(`{"name":"Trip"}`), map[string]string{"Content-Type": "application/json"})
	folderID, _ := folder.body["id"].(string)
	if folder.status != 201 || folderID == "" {
		t.Fatalf("create folder %d %s", folder.status, folder.raw)
	}

	// The original: a JPEG with a marker repeated in its trailing data.
	marker := "PLAINTEXT-MARKER-" + rand.Text()
	var photo bytes.Buffer
	photo.Write(testPhoto(t, 7))
	for photo.Len() < 3<<20 {
		photo.WriteString(marker)
		chunk := make([]byte, 4096)
		rand.Read(chunk)
		photo.Write(chunk)
	}
	sum := sha256.Sum256(photo.Bytes())
	sumHex := hex.EncodeToString(sum[:])
	jsonHdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "relay-e2e-1"}
	up := call(t, app, token, "POST", "/api/v1/folders/"+folderID+"/uploads",
		[]byte(fmt.Sprintf(`{"bytes":%d,"sha256":"%s","filename":"IMG_0001.JPG"}`, photo.Len(), sumHex)), jsonHdr)
	uploadID, _ := up.body["upload_id"].(string)
	if up.status != 201 || uploadID == "" {
		t.Fatalf("create upload %d %s", up.status, up.raw)
	}
	base := "/api/v1/folders/" + folderID + "/uploads/" + uploadID
	const chunk = 1 << 20
	send := func(off int) apiReply {
		end := min(off+chunk, photo.Len())
		return call(t, app, token, "PATCH", base, photo.Bytes()[off:end],
			map[string]string{"Upload-Offset": strconv.Itoa(off), "Content-Type": "application/offset+octet-stream"})
	}
	if r := send(0); r.status != 200 && r.status != 204 {
		t.Fatalf("first chunk %d %s", r.status, r.raw)
	}
	// The network drops the tunnel mid-upload; the server reconnects and
	// the app resumes at the offset the server reports.
	before := f.relay.Stats().Tunnels
	app.CloseIdleConnections()
	dropTunnel(t, f)
	f.waitConnected(t)
	if before != 1 || !strings.Contains(f.logs(), `"tunnel_down"`) {
		t.Fatalf("tunnel did not drop: %d %s", before, f.logs())
	}
	st := call(t, app, token, "GET", base, nil, nil)
	off, _ := st.body["offset"].(float64)
	if st.status != 200 || int(off) != chunk {
		t.Fatalf("offset after reconnect %d %s", st.status, st.raw)
	}
	for o := int(off); o < photo.Len(); o += chunk {
		if r := send(o); r.status != 200 && r.status != 204 {
			t.Fatalf("chunk at %d: %d %s", o, r.status, r.raw)
		}
	}
	done := call(t, app, token, "POST", base+"/complete", nil, nil)
	assetID, _ := done.body["asset_id"].(string)
	if done.status != 201 || assetID == "" || done.body["sha256"] != sumHex {
		t.Fatalf("complete %d %s", done.status, done.raw)
	}
	orig := call(t, app, token, "GET", "/api/v1/folders/"+folderID+"/assets/"+assetID+"/original", nil, nil)
	if orig.status != 200 || !bytes.Equal(orig.raw, photo.Bytes()) {
		t.Fatalf("download %d, %d bytes (want %d)", orig.status, len(orig.raw), photo.Len())
	}

	// What crossed the relay is ciphertext only. (Control: the same
	// search finds everything in the app's plaintext.)
	plain := f.appPlain.bytes()
	for _, secret := range []string{marker, relayTestPassword, token, `"alice"`} {
		if !bytes.Contains(plain, []byte(secret)) {
			t.Fatalf("control: %q not in the app's plaintext", secret)
		}
	}
	for side, seen := range map[string][]byte{"app side": f.appSide.bytes(), "home side": f.homeSide.bytes()} {
		if len(seen) < 2*photo.Len() {
			t.Fatalf("%s: only %d bytes recorded", side, len(seen))
		}
		for what, secret := range map[string]string{"photo marker": marker, "password": relayTestPassword, "access token": token,
			"login name": `"alice"`, "upload path": "/uploads/", "JPEG header": string(testPhoto(t, 7)[:64])} {
			if bytes.Contains(seen, []byte(secret)) {
				t.Errorf("%s: the relay saw the %s in plaintext", side, what)
			}
		}
	}
	// Nor does the relay log anything but IDs, timings and counts.
	logs := f.logs()
	for _, secret := range []string{marker, token, "alice", folderID, assetID, "127.0.0.1"} {
		if strings.Contains(logs, secret) {
			t.Errorf("relay log contains %q", secret)
		}
	}
	if !strings.Contains(logs, `"rid":"`+rt.RID+`"`) || !strings.Contains(logs, `"bytes_to_server"`) {
		t.Errorf("relay log lacks the documented metadata:\n%s", logs)
	}

	// The administrator sees the relay on the direct (LAN) listener.
	adminTLS := serveJSON(f.direct, token, "GET", "/api/v1/admin/server/tls", "")
	var tv struct {
		Relay   map[string]any    `json:"relay"`
		Pairing []httpapi.Pairing `json:"pairing"`
	}
	json.Unmarshal(adminTLS.Body.Bytes(), &tv)
	if adminTLS.Code != 200 || tv.Relay["connected"] != true || len(tv.Pairing) != 1 || tv.Pairing[0].Payload != payload {
		t.Fatalf("admin tls %d %s", adminTLS.Code, adminTLS.Body)
	}
}

// dropTunnel breaks the home server's tunnel as a network failure would:
// the relay's side of the connection goes away.
func dropTunnel(t *testing.T, f *relayFixture) {
	t.Helper()
	before := strings.Count(f.logs(), `"tunnel_up"`)
	// Restarting the relay on the same address drops every tunnel; a
	// shorter way is to make the relay forget it: a second home server
	// process with the same key replaces it, then goes away.
	key := f.id.Certificate.PrivateKey.(*ecdsa.PrivateKey)
	c, err := relay.NewClient(relay.ClientConfig{Addr: f.relayAddr, TLS: &tls.Config{RootCAs: f.pool}, Key: key,
		SPKI: f.id.Leaf.RawSubjectPublicKeyInfo})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(f.logs(), `"tunnel_up"`) == before {
		if time.Now().After(deadline) {
			t.Fatal("second tunnel not up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	c.Close()
	// The real server notices its tunnel is gone and reconnects.
	deadline = time.Now().Add(15 * time.Second)
	for strings.Count(f.logs(), `"tunnel_up"`) < before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("server did not reconnect:\n%s", f.logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRelayWrongPin: an app paired with another key, or a relay that
// splices the app to another server, fails the pinned handshake.
func TestRelayWrongPin(t *testing.T) {
	f := newRelayFixture(t)
	wrong := f.id.SPKISHA256
	wrong[0] ^= 0xff
	rid := f.client.RendezvousID()
	req, _ := http.NewRequest("GET", "https://withalbum.invalid/api/v1/health", nil)
	if _, err := f.appClient(wrong, rid).Do(req); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("wrong pin: %v", err)
	}
	// A rendezvous ID nobody holds: the relay says offline.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := relay.Dial(ctx, f.relayAddr, relay.RendezvousID(wrong), &tls.Config{RootCAs: f.pool})
	var re *relay.RefusedError
	if !errors.As(err, &re) || re.Code != relay.ErrCodeOffline {
		t.Fatalf("unknown rid: %v", err)
	}
	// A relay certificate that is not trusted: the outer TLS fails.
	_, err = relay.Dial(ctx, f.relayAddr, rid, &tls.Config{})
	if err == nil {
		t.Fatal("untrusted relay certificate accepted")
	}
}

// TestRelayNeedsAdministrator: like a LAN listener, the relay is refused
// on a storage without an administrator who can log in.
func TestRelayNeedsAdministrator(t *testing.T) {
	root := newRoot(t)
	a := mustOpenApp(t, root, nil)
	defer a.Close()
	id, err := tlsid.LoadOrCreate(a.vol.Root(), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.setupRelay(t.Context(), "relay.example.org:443", id); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("setupRelay without administrator: %v", err)
	}
}

func TestTLSInfoCommandWithRelay(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	t.Setenv("WITHALBUM_LISTEN", "192.0.2.10:8787")
	t.Setenv("WITHALBUM_RELAY", "relay.example.org:443")
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
	var j struct {
		Relay   map[string]string `json:"relay"`
		Pairing []httpapi.Pairing `json:"pairing"`
	}
	if err := json.Unmarshal(out.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	rid := relay.RendezvousID(id.SPKISHA256)
	want := tlsid.PairingPayloadRelay("https://192.0.2.10:8787", id.SPKIHex(), "relay.example.org:443", rid)
	if j.Relay["rendezvous_id"] != rid || len(j.Pairing) != 1 || j.Pairing[0].Payload != want {
		t.Fatalf("tls -json with relay: %s", out.String())
	}
	if code := runTLSInfo([]string{"-relay", "https://x"}, &out, &errOut); code != exitUsage {
		t.Fatalf("bad -relay: %d", code)
	}
}
