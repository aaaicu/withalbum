package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
)

// TestSecurityHeaders: every response carries the hardening headers; the
// dashboard keeps its own CSP; API responses may not be framed or run
// anything.
func TestSecurityHeaders(t *testing.T) {
	e := newACLEnv(t)
	for _, r := range []reply{
		do(t, e.Raw, "GET", "/api/v1/health", nil, nil),
		do(t, e.Raw, "GET", "/api/v1/folders", nil, nil),
		e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family, nil, nil),
		do(t, e.Raw, "GET", "/", nil, nil),
	} {
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
			"Referrer-Policy": "no-referrer", "Cross-Origin-Resource-Policy": "same-origin"} {
			if r.Header.Get(k) != v {
				t.Errorf("status %d: %s = %q, want %q", r.Status, k, r.Header.Get(k), v)
			}
		}
		if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("status %d: CSP %q", r.Status, csp)
		}
		if r.Header.Get("Strict-Transport-Security") != "" {
			t.Errorf("HSTS on plain HTTP")
		}
	}
	if csp := do(t, e.Raw, "GET", "/", nil, nil).Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'sha256-") {
		t.Errorf("dashboard CSP replaced: %q", csp)
	}
}

// TestStoredHTMLIsNotServedAsDocument: an uploaded HTML file is stored and
// returned byte for byte, but as a sandboxed attachment, so opening its URL
// in a browser cannot run script in the dashboard's origin.
func TestStoredHTMLIsNotServedAsDocument(t *testing.T) {
	e := newACLEnv(t)
	page := []byte("<!doctype html><html><script>fetch('/api/v1/me')</script></html>")
	up := wantJSON(t, "upload html", e.as(t, "contributor", "POST", "/api/v1/folders/"+e.family+"/assets", page, map[string]string{"X-Filename": "evil \"x\".html"}), 201)
	r := e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/assets/"+up["asset_id"].(string)+"/original", nil, nil)
	if r.Status != 200 || string(r.Body) != string(page) {
		t.Fatalf("download %d %q", r.Status, r.Body)
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("original CSP %q, want sandbox", csp)
	}
	if cd := r.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, `filename="evil \"x\".html"`) {
		t.Errorf("Content-Disposition %q", cd)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("nosniff missing")
	}
	p := e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/assets/"+e.photo+"/preview", nil, nil)
	if p.Status != 200 || p.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("preview %d %s", p.Status, p.Header.Get("Content-Type"))
	}
}

// TestAdminServerTLS: only a SERVER_ADMIN reads the TLS identity, which
// holds fingerprints and pairing payloads, never key material.
func TestAdminServerTLS(t *testing.T) {
	e := newACLEnv(t)
	wantError(t, "unauthenticated", do(t, e.Raw, "GET", "/api/v1/admin/server/tls", nil, nil), 401, "unauthorized")
	wantError(t, "user", e.as(t, "owner", "GET", "/api/v1/admin/server/tls", nil, nil), 403, "forbidden")
	m := wantJSON(t, "admin, no TLS", e.as(t, "root", "GET", "/api/v1/admin/server/tls", nil, nil), 200)
	if m["enabled"] != false || len(m) != 1 {
		t.Fatalf("without TLS: %v", m)
	}
}

func TestAdminServerTLSEnabled(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	info := &httpapi.TLSInfo{SPKISHA256: pin, CertSHA256: strings.Repeat("cd", 32), NotAfter: time.Date(2028, 1, 2, 3, 4, 5, 0, time.UTC),
		Pairing: []httpapi.Pairing{{URL: "https://192.0.2.10:8787", Payload: "withalbum://pair?x"}}}
	e := newContractEnvWith(t, openSQLiteStore, func(c *httpapi.Config) { c.TLS = info })
	m := wantJSON(t, "admin tls", do(t, e.Handler, "GET", "/api/v1/admin/server/tls", nil, nil), 200)
	if m["enabled"] != true || m["spki_sha256"] != pin || m["cert_sha256"] != strings.Repeat("cd", 32) || m["not_after"] != "2028-01-02T03:04:05Z" {
		t.Fatalf("tls info %v", m)
	}
	p := m["pairing"].([]any)[0].(map[string]any)
	if p["url"] != "https://192.0.2.10:8787" || p["payload"] != "withalbum://pair?x" {
		t.Fatalf("pairing %v", p)
	}
}
