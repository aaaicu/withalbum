package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/proxyproto"
)

// Access from outside the home through Tailscale Funnel (docs/FUNNEL.md).
//
// With -funnel-url https://<machine>.<tailnet>.ts.net (or
// WITHALBUM_FUNNEL_URL) the server opens a second listener on loopback
// only (-funnel-listen, default 127.0.0.1:8788) for a Funnel *TCP*
// forwarder:
//
//	tailscale funnel --bg --tcp=443 --proxy-protocol=2 tcp://127.0.0.1:8788
//
// Funnel routes the app's TLS connection by its SNI name without
// terminating it, and tailscaled forwards the raw bytes, so the app's TLS
// session with the server's own key and SPKI pin reaches this listener
// unchanged: neither Tailscale's relay servers nor tailscaled can read it.
// (A plain `tailscale funnel 8788` would terminate TLS in tailscaled with a
// *.ts.net certificate instead; that mode is not supported, see
// docs/FUNNEL.md.)
//
// Connections on this listener are classified like relay connections: only
// app routes are served (relayAllowed), administration answers 403. The
// listener never binds to anything but loopback (in a container: see
// container.go); a LAN listener still needs
// -allow-lan with its own rules. Like the relay it needs -tls and an
// administrator who can log in.

// funnelUnknown is the client of a funnel connection without a usable
// PROXY header: all such connections share one login rate-limit key, which
// then acts as a limit for the whole route.
const funnelUnknown = "funnel-unknown:0"

// funnelPorts are the public ports Funnel can listen on.
var funnelPorts = map[string]bool{"": true, "443": true, "8443": true, "10000": true}

// funnelURLSetting is -funnel-url, else WITHALBUM_FUNNEL_URL.
func funnelURLSetting(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("WITHALBUM_FUNNEL_URL")
}

// funnelListenSetting is -funnel-listen, else WITHALBUM_FUNNEL_LISTEN,
// else 127.0.0.1:8788.
func funnelListenSetting(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("WITHALBUM_FUNNEL_LISTEN"); v != "" {
		return v
	}
	return "127.0.0.1:8788"
}

// parseFunnelURL validates a Funnel address and returns it in canonical
// form: https://<name>.ts.net with :8443 or :10000 when not 443, nothing
// else (no path, query, user or fragment). Funnel only serves names in the
// tailnet's ts.net domain on these three ports.
func parseFunnelURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.ForceQuery {
		return "", fmt.Errorf("funnel address %q must be https://<machine>.<tailnet>.ts.net (no path or query)", s)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if !funnelPorts[port] {
		return "", fmt.Errorf("funnel address %q: Funnel only listens on ports 443, 8443 and 10000", s)
	}
	labels := strings.Split(host, ".")
	if !strings.HasSuffix(host, ".ts.net") || len(labels) < 4 || len(host) > 253 {
		return "", fmt.Errorf("funnel address %q: the host must be the machine's MagicDNS name <machine>.<tailnet>.ts.net", s)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return "", fmt.Errorf("funnel address %q: invalid host name", s)
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("funnel address %q: invalid host name", s)
			}
		}
	}
	if port == "" || port == "443" {
		return "https://" + host, nil
	}
	return "https://" + host + ":" + port, nil
}

// checkFunnel decides whether the funnel listener may be used as
// configured, before the storage is opened. It returns the canonical
// public URL. The administrator condition is checked later.
func checkFunnel(c listenConfig, funnelURL, funnelListen string) (string, error) {
	if funnelURL == "" {
		return "", nil
	}
	pub, err := parseFunnelURL(funnelURL)
	if err != nil {
		return "", err
	}
	if !c.TLS {
		return "", fmt.Errorf("the funnel route (%s) needs TLS (-tls): app connections are only accepted over the pinned TLS identity", pub)
	}
	host, _, err := net.SplitHostPort(funnelListen)
	if err != nil {
		return "", fmt.Errorf("funnel listen address %q (want 127.0.0.1:<port>): %v", funnelListen, err)
	}
	if c.Container {
		// container.go: reachable only from the internal network.
		if _, err := scopeOf(funnelListen); err != nil || host != "" && net.ParseIP(host) == nil {
			return "", fmt.Errorf("funnel listen address %q must be an IP address of the container (e.g. 0.0.0.0:8788)", funnelListen)
		}
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("funnel listen address %q must be a loopback IP address (127.0.0.1 or ::1): only tailscaled on this Mac may connect to it", funnelListen)
	}
	if funnelListen == c.Addr {
		return "", fmt.Errorf("funnel listen address %q must differ from WITHALBUM_LISTEN: the funnel route is served on its own listener", funnelListen)
	}
	return pub, nil
}

// setupFunnel checks the administrator condition and tells the API about
// the public route (before Handler).
func (a *app) setupFunnel(ctx context.Context, pub string) error {
	n, err := a.auth.Users.UsableAdminCount(ctx)
	if err != nil {
		return fmt.Errorf("cannot count administrators: %w", err)
	}
	if n < 1 {
		return fmt.Errorf("the funnel route (%s) needs an active server administrator who can log in (run: withalbum-server admin init -login <name>)", pub)
	}
	a.api.SetPublicRoute(&httpapi.PublicRoute{URL: pub, Route: httpapi.RouteFunnel})
	return nil
}

// listenFunnel opens the funnel listener: loopback only (verified after
// binding) unless in a container, with an optional PROXY protocol header
// per connection.
func listenFunnel(addr string, container bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if err := checkBoundAddr(ln.Addr(), container); err != nil {
		ln.Close()
		return nil, err
	}
	return &limitListener{Listener: &proxyproto.Listener{Listener: ln, Unknown: funnelUnknown}, sem: make(chan struct{}, funnelMaxConns)}, nil
}

// funnelMaxConns bounds the connections served at once on the funnel
// route (a household needs a few dozen: previews load in parallel). More
// wait in the kernel's backlog until one closes.
const funnelMaxConns = 128

// limitListener accepts at most cap(sem) connections at a time.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: sync.OnceFunc(func() { <-l.sem })}, nil
}

type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// serveFunnel serves the app routes on ln with the server's TLS identity.
// Shut the returned server down to stop it.
func serveFunnel(ln net.Listener, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	srv := newHTTPServer(remoteOnly(httpapi.RouteFunnel, handler), tlsConfig)
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("funnel: %v", err)
		}
	}()
	return srv
}

// funnelCommand is the tailscale command that forwards the public URL pub
// to the funnel listener addr without terminating TLS.
func funnelCommand(pub, addr string) string {
	port := "443"
	if u, err := url.Parse(pub); err == nil && u.Port() != "" {
		port = u.Port()
	}
	return "tailscale funnel --bg --tcp=" + port + " --proxy-protocol=2 tcp://" + addr
}
