package tlsid_test

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".system"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCreateAndReload(t *testing.T) {
	root := newRoot(t)
	now := time.Now()
	if _, err := tlsid.Load(root); !errors.Is(err, tlsid.ErrNotFound) {
		t.Fatalf("Load before create = %v, want ErrNotFound", err)
	}
	id, err := tlsid.LoadOrCreate(root, []string{"192.0.2.10"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Created || id.Renewed {
		t.Fatalf("created=%v renewed=%v", id.Created, id.Renewed)
	}
	fi, err := os.Stat(tlsid.Path(root))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode %v, %v", fi.Mode(), err)
	}
	di, err := os.Stat(filepath.Dir(tlsid.Path(root)))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("tls dir mode %v, %v", di.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(tlsid.Path(root)), ".server-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	leaf := id.Leaf
	if leaf.NotAfter.Sub(now) > 825*24*time.Hour {
		t.Fatalf("validity %v exceeds 825 days", leaf.NotAfter.Sub(now))
	}
	if !strings.Contains(strings.Join(leaf.DNSNames, ","), "localhost") || len(leaf.IPAddresses) != 3 {
		t.Fatalf("SANs %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	again, err := tlsid.LoadOrCreate(root, []string{"192.0.2.10"}, now)
	if err != nil || again.Created || again.Renewed || again.SPKISHA256 != id.SPKISHA256 || again.CertSHA256 != id.CertSHA256 {
		t.Fatalf("reload changed the identity: %+v %v", again, err)
	}
	loaded, err := tlsid.Load(root)
	if err != nil || loaded.SPKISHA256 != id.SPKISHA256 {
		t.Fatalf("Load: %v", err)
	}
	if len(id.SPKIHex()) != 64 || len(tlsid.Colons(id.SPKIHex())) != 95 {
		t.Fatalf("fingerprint formats %q %q", id.SPKIHex(), tlsid.Colons(id.SPKIHex()))
	}
}

// TestRenewalKeepsPin: an expiring certificate and a new listen address
// re-issue the certificate with the same key, so the pin does not change.
func TestRenewalKeepsPin(t *testing.T) {
	root := newRoot(t)
	now := time.Now()
	id, err := tlsid.LoadOrCreate(root, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(tlsid.Validity - tlsid.RenewBefore + time.Hour)
	r, err := tlsid.LoadOrCreate(root, nil, later)
	if err != nil || !r.Renewed || r.SPKISHA256 != id.SPKISHA256 || r.CertSHA256 == id.CertSHA256 || !r.Leaf.NotAfter.After(id.Leaf.NotAfter) {
		t.Fatalf("expiry renewal: %+v %v", r, err)
	}
	h, err := tlsid.LoadOrCreate(root, []string{"192.0.2.20"}, later)
	if err != nil || !h.Renewed || h.SPKISHA256 != id.SPKISHA256 {
		t.Fatalf("host renewal: %+v %v", h, err)
	}
	if len(h.Leaf.IPAddresses) != 3 || !h.Leaf.IPAddresses[2].Equal(net.ParseIP("192.0.2.20")) {
		t.Fatalf("SANs after host renewal: %v", h.Leaf.IPAddresses)
	}
}

func TestRefusesUnsafeOrBrokenFile(t *testing.T) {
	root := newRoot(t)
	id, err := tlsid.LoadOrCreate(root, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := tlsid.Path(root)
	good, _ := os.ReadFile(path)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsid.LoadOrCreate(root, nil, time.Now()); !errors.Is(err, tlsid.ErrInsecureFile) {
		t.Fatalf("0644 file: %v, want ErrInsecureFile", err)
	}
	os.Chmod(path, 0o600)

	other := filepath.Join(t.TempDir(), "x.pem")
	os.WriteFile(other, good, 0o600)
	os.Remove(path)
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsid.LoadOrCreate(root, nil, time.Now()); !errors.Is(err, tlsid.ErrInsecureFile) {
		t.Fatalf("symlink: %v, want ErrInsecureFile", err)
	}
	os.Remove(path)

	// A truncated file is refused, never silently replaced by a new key
	// (that would break every paired device without notice).
	os.WriteFile(path, good[:len(good)/2], 0o600)
	_, err = tlsid.LoadOrCreate(root, nil, time.Now())
	if !errors.Is(err, tlsid.ErrInvalid) {
		t.Fatalf("truncated file: %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), string(good[40:60])) {
		t.Fatalf("error leaks key material: %v", err)
	}
	if b, _ := os.ReadFile(path); len(b) != len(good)/2 {
		t.Fatal("truncated file was changed")
	}
	_ = id
}

func TestPairingPayload(t *testing.T) {
	p := tlsid.PairingPayload("https://192.0.2.10:8787", strings.Repeat("ab", 32))
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "withalbum" || u.Host != "pair" {
		t.Fatalf("payload %q: %v", p, err)
	}
	q := u.Query()
	if q.Get("v") != "1" || q.Get("url") != "https://192.0.2.10:8787" || q.Get("spki_sha256") != strings.Repeat("ab", 32) {
		t.Fatalf("payload query %v", q)
	}
}

// TestPinnedHandshake: a client pinned to the server key connects, a client
// with another pin, a CA-verifying client and a TLS 1.2 client do not.
func TestPinnedHandshake(t *testing.T) {
	root := newRoot(t)
	id, err := tlsid.LoadOrCreate(root, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.TLS = tlsid.ServerConfig(id)
	srv.StartTLS()
	defer srv.Close()

	get := func(cfg *tls.Config) (string, error) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	if body, err := get(tlsid.PinnedClientConfig(id.SPKISHA256)); err != nil || body != "ok" {
		t.Fatalf("pinned client: %q %v", body, err)
	}
	wrong := id.SPKISHA256
	wrong[0] ^= 1
	if _, err := get(tlsid.PinnedClientConfig(wrong)); err == nil || !strings.Contains(err.Error(), "paired fingerprint") {
		t.Fatalf("wrong pin: %v", err)
	}
	if _, err := get(&tls.Config{}); err == nil {
		t.Fatal("system-CA client accepted the self-signed certificate")
	}
	old := tlsid.PinnedClientConfig(id.SPKISHA256)
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, err := get(old); err == nil {
		t.Fatal("TLS 1.2 client was accepted")
	}
}
