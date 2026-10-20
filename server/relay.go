package main

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/relay"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

// Access from outside the home through a relay (docs/RELAY_DESIGN.md).
//
// With -relay host:port (or WITHALBUM_RELAY) the server keeps one outbound
// TLS connection to that relay and serves the app connections it carries
// with the same TLS identity and pin as on the LAN. The server opens no
// inbound port for it. The relay forwards the bytes of each app's pinned
// TLS session and cannot read or change them.
//
// It needs -tls and an administrator who can log in, like a LAN listener.
// Connections through the relay are authenticated users over pinned TLS,
// but only the routes an app needs are served there (relayAllowed): the
// dashboard page and every /api/v1/admin route answer 403
// not_available_via_relay, so account and server administration stay on
// this Mac or the home network. The setup screen never runs with -tls.

// relayTarget is the relay a server is reachable through (zero: none).
type relayTarget struct {
	Addr string // host:port
	RID  string // rendezvous ID
}

// relayAddrSetting is -relay, else WITHALBUM_RELAY.
func relayAddrSetting(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("WITHALBUM_RELAY")
}

// checkRelayAddr validates a relay address: host:port with a DNS name or
// IP address and a port, nothing else (no scheme, no path).
func checkRelayAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("relay address %q must be host:port (for example relay.example.org:443): %v", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return fmt.Errorf("relay address %q: invalid port", addr)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if host == "" || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return fmt.Errorf("relay address %q: invalid host name", addr)
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return fmt.Errorf("relay address %q: invalid host name", addr)
		}
	}
	return nil
}

// checkRelay decides whether the relay may be used as configured, before
// the storage is opened. The administrator condition is checked later.
func checkRelay(c listenConfig, addr string) error {
	if addr == "" {
		return nil
	}
	if err := checkRelayAddr(addr); err != nil {
		return err
	}
	if !c.TLS {
		return fmt.Errorf("the relay (%s) needs TLS (-tls): app connections are only accepted over the pinned TLS identity", addr)
	}
	return nil
}

// relayOuterTLS is the TLS configuration for the connection to the relay
// itself: the system roots, or the PEM certificates of
// WITHALBUM_RELAY_CA_FILE (a self-hosted relay with a private CA).
func relayOuterTLS(getenv func(string) string) (*tls.Config, error) {
	file := getenv("WITHALBUM_RELAY_CA_FILE")
	if file == "" {
		return &tls.Config{}, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("WITHALBUM_RELAY_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("WITHALBUM_RELAY_CA_FILE %s holds no PEM certificate", file)
	}
	return &tls.Config{RootCAs: pool}, nil
}

// newRelayClient builds the tunnel client for the server identity id.
func newRelayClient(id *tlsid.Identity, addr string, outer *tls.Config) (*relay.Client, error) {
	key, ok := id.Certificate.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("relay: the TLS key cannot sign")
	}
	return relay.NewClient(relay.ClientConfig{Addr: addr, TLS: outer, Key: key, SPKI: id.Leaf.RawSubjectPublicKeyInfo, Logf: log.Printf})
}

// relayTargetFor is the relay target of the identity id at addr (zero
// when addr is empty).
func relayTargetFor(id *tlsid.Identity, addr string) relayTarget {
	if addr == "" {
		return relayTarget{}
	}
	return relayTarget{Addr: addr, RID: relay.RendezvousID(id.SPKISHA256)}
}

// relayAllowed lists the paths served through the relay: what an app uses
// (login, its own account and devices, albums, invitations, health). Every
// other path, in particular the dashboard page and /api/v1/admin/, is not.
// A path that is not in canonical form is refused rather than interpreted.
func relayAllowed(p string) bool {
	if p == "" || path.Clean(p) != p {
		return false
	}
	under := func(prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	return p == "/api/v1/health" || under("/api/v1/auth") || under("/api/v1/me") || under("/api/v1/folders") || under("/api/v1/invitations")
}

// relayOnly wraps the API handler for connections that came through the
// relay.
func relayOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !relayAllowed(r.URL.Path) {
			h := w.Header()
			h.Set("Content-Type", "application/json; charset=utf-8")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":"not_available_via_relay"}`+"\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setupRelay checks the administrator condition, builds the tunnel client
// for the identity id and tells the API about the relay (before Handler).
func (a *app) setupRelay(ctx context.Context, addr string, id *tlsid.Identity) (*relay.Client, error) {
	n, err := a.auth.Users.UsableAdminCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot count administrators: %w", err)
	}
	if n < 1 {
		return nil, fmt.Errorf("the relay (%s) needs an active server administrator who can log in (run: withalbum-server admin init -login <name>)", addr)
	}
	outer, err := relayOuterTLS(os.Getenv)
	if err != nil {
		return nil, err
	}
	c, err := newRelayClient(id, addr, outer)
	if err != nil {
		return nil, err
	}
	a.api.SetRelayInfo(&httpapi.RelayInfo{Endpoint: addr, RendezvousID: c.RendezvousID(),
		Connected: func() bool { return c.Status().Connected }})
	log.Printf("relay: enabled through %s, rendezvous id %s. No inbound port is opened. The relay operator can see when and how much data flows, never its content.", addr, c.RendezvousID())
	return c, nil
}

// serveRelay keeps the tunnel up and serves the app connections it
// carries with the server's TLS identity, restricted by relayOnly. Shut
// the returned server down to stop both.
func serveRelay(ctx context.Context, c *relay.Client, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	srv := newHTTPServer(relayOnly(handler), tlsConfig)
	go c.Run(ctx)
	go func() {
		if err := srv.ServeTLS(c, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("relay: %v", err)
		}
	}()
	return srv
}
