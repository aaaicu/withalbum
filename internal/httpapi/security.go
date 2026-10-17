package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"time"
)

// Response hardening shared by every route, and the TLS identity shown to
// administrators.

// apiCSP is the Content-Security-Policy of every response except the
// dashboard page (which sets its own): API responses are data, never a
// document that may run script, load anything or be framed.
const apiCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// downloadCSP additionally sandboxes a downloaded original: even a stored
// HTML or SVG file opened directly in a browser gets an opaque origin, so
// it cannot read the dashboard's cookies or call the API.
const downloadCSP = apiCSP + "; sandbox"

// dashboardCSP is the policy of the dashboard page. Its one inline <style>
// and one inline <script> are allowed by their SHA-256 hashes, computed from
// the embedded page itself, so no other inline script or style, no event
// handler attribute and no script from anywhere else can run, even if text
// ever reached the page as markup. The page writes the DOM only through
// textContent and element construction; Trusted Types (where the browser
// supports them) additionally refuse every HTML-string sink such as
// innerHTML. It talks to its own origin only, loads no images and submits
// no form (login and every change go through fetch with the CSRF header).
var dashboardCSP = "default-src 'none'; script-src " + inlineHash(dashboardHTML, "script") +
	"; style-src " + inlineHash(dashboardHTML, "style") +
	"; connect-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'" +
	"; require-trusted-types-for 'script'; trusted-types 'none'"

// inlineHash returns the CSP source of the one <tag>...</tag> element of
// page. It panics unless there is exactly one, without attributes.
func inlineHash(page []byte, tag string) string {
	open, end := []byte("<"+tag+">"), []byte("</"+tag+">")
	if bytes.Count(page, []byte("<"+tag)) != 1 || bytes.Count(page, open) != 1 || bytes.Count(page, end) != 1 {
		panic("httpapi: the dashboard needs exactly one plain <" + tag + "> element")
	}
	_, rest, _ := bytes.Cut(page, open)
	body, _, _ := bytes.Cut(rest, end)
	sum := sha256.Sum256(body)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// hstsMaxAge: one year, without includeSubDomains or preload.
const hstsMaxAge = "max-age=31536000"

// securityHeaders sets the headers every response carries. Handlers may
// replace Content-Security-Policy (the dashboard page and downloads do).
//
// Strict-Transport-Security is sent only on TLS requests whose Host is a DNS
// name other than localhost: browsers ignore HSTS for IP addresses, and
// HSTS for localhost would force HTTPS on every other local development
// server of the machine (all ports). Browsers also ignore it on connections
// with certificate errors, i.e. until the self-signed certificate is
// trusted.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy", apiCSP)
		if r.TLS != nil && hstsHost(r.Host) {
			h.Set("Strict-Transport-Security", hstsMaxAge)
		}
		next.ServeHTTP(w, r)
	})
}

func hstsHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host != "" && host != "localhost" && !strings.HasSuffix(host, ".localhost") && net.ParseIP(strings.Trim(host, "[]")) == nil
}

// rateKey is the client key of the login rate limits: the IP address, with
// IPv6 addresses shortened to their /64 prefix, because one host can use
// any address of its /64 (SLAAC privacy addresses) and would otherwise get a
// fresh budget per address. Forwarding headers are never consulted.
func rateKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil || parsed.IsLoopback() {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// TLSInfo describes the server's TLS identity for administrators
// (GET /api/v1/admin/server/tls). nil: the server runs without TLS.
type TLSInfo struct {
	SPKISHA256 string    // pin, 64 hex digits
	CertSHA256 string    // certificate fingerprint, 64 hex digits
	NotAfter   time.Time // end of the certificate's validity
	Pairing    []Pairing // one per address devices can use
}

// Pairing is a server URL and the payload a device scans to pin it.
type Pairing struct {
	URL     string `json:"url"`
	Payload string `json:"payload"`
}

// SetTLSInfo records the TLS identity. Call it before Handler.
func (s *Server) SetTLSInfo(info *TLSInfo) { s.tls = info }

// serverTLS answers GET /api/v1/admin/server/tls (SERVER_ADMIN). It shows
// fingerprints and pairing payloads only, never key material. Compare the
// fingerprint with the one printed at the server console: a page served
// through an intercepted connection could show a forged one.
func (s *Server) serverTLS(w http.ResponseWriter, r *http.Request) {
	if s.tls == nil {
		jsonReply(w, 200, map[string]any{"enabled": false})
		return
	}
	pairing := s.tls.Pairing
	if pairing == nil {
		pairing = []Pairing{}
	}
	jsonReply(w, 200, map[string]any{"enabled": true, "spki_sha256": s.tls.SPKISHA256, "cert_sha256": s.tls.CertSHA256,
		"not_after": ts(s.tls.NotAfter), "pairing": pairing})
}
