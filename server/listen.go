package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

// Network exposure.
//
// Plain HTTP is served on loopback only. Any other listen address (a LAN
// address, or the 0.0.0.0 / :: wildcard) needs all of:
//
//   - the explicit -allow-lan flag,
//   - -tls (the self-signed identity of internal/tlsid, pinned by clients),
//   - at least one administrator who can log in (active SERVER_ADMIN with a
//     usable password), so that the server is never reachable on a network
//     before someone can manage its accounts,
//
// and a specific address must be private (RFC 1918 IPv4 or fc00::/7 IPv6
// ULA). Public addresses and host names are refused: exposing the server to
// the internet is not supported.

// listenConfig is the exposure the operator asked for.
type listenConfig struct {
	Addr     string // WITHALBUM_LISTEN
	AllowLAN bool   // -allow-lan
	TLS      bool   // -tls
}

// listenScope classifies the host of a listen address.
type listenScope int

const (
	scopeLoopback listenScope = iota
	scopePrivate              // a private IP address
	scopeWildcard             // 0.0.0.0, :: or empty: every interface
)

// isLoopbackHost reports whether host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func scopeOf(addr string) (listenScope, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("invalid listen address %q (want host:port): %v", addr, err)
	}
	if port == "" {
		return 0, fmt.Errorf("invalid listen address %q: no port", addr)
	}
	if isLoopbackHost(host) {
		return scopeLoopback, nil
	}
	if host == "" {
		return scopeWildcard, nil
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return 0, fmt.Errorf("listen address %q: a non-loopback host must be an IP address, not a name", addr)
	case ip.IsUnspecified():
		return scopeWildcard, nil
	case ip.IsPrivate():
		return scopePrivate, nil
	}
	return 0, fmt.Errorf("listen address %q is not a private LAN address (RFC 1918 or fc00::/7); exposing the server to the internet is not supported", addr)
}

// checkListen decides whether the server may listen as c asks, given the
// number of administrators who can log in. lan reports a non-loopback
// listener. Every missing condition is named in the error.
func checkListen(c listenConfig, usableAdmins int) (lan bool, err error) {
	scope, err := scopeOf(c.Addr)
	if err != nil {
		return false, err
	}
	if scope == scopeLoopback {
		return false, nil
	}
	var missing []string
	if !c.AllowLAN {
		missing = append(missing, "the -allow-lan flag")
	}
	if !c.TLS {
		missing = append(missing, "TLS (-tls); passwords and tokens must not cross the network in clear text")
	}
	if usableAdmins < 1 {
		missing = append(missing, "an active server administrator who can log in (run: withalbum-server admin init -login <name>)")
	}
	if len(missing) > 0 {
		return true, fmt.Errorf("refusing non-loopback address %q: it needs %s", c.Addr, strings.Join(missing, ", "))
	}
	return true, nil
}

// checkBoundAddr verifies that a listener approved as loopback-only really
// bound to a loopback address.
func checkBoundAddr(addr net.Addr, lan bool) error {
	if lan {
		return nil
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		return fmt.Errorf("listener bound to %s, which is not a loopback address; refusing to serve", addr)
	}
	return nil
}

// listenAllowed applies checkListen with the administrators stored in the
// database of a started app.
func (a *app) listenAllowed(ctx context.Context, c listenConfig) (bool, error) {
	n, err := a.auth.Users.UsableAdminCount(ctx)
	if err != nil {
		return false, fmt.Errorf("cannot count administrators: %w", err)
	}
	return checkListen(c, n)
}

// listenAddr is WITHALBUM_LISTEN, default 127.0.0.1:8787.
func listenAddr() string {
	if addr := os.Getenv("WITHALBUM_LISTEN"); addr != "" {
		return addr
	}
	return "127.0.0.1:8787"
}

// certHosts are the names the certificate should carry for addr, in
// addition to tlsid.DefaultHosts: the listen IP when it is a specific one.
func certHosts(addr string) []string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return nil
	}
	return []string{host}
}

// serverURLs lists the https URLs devices can use for addr: the address
// itself, or for a wildcard every private address of the machine's
// interfaces (loopback when there is none).
func serverURLs(addr string, interfaceAddrs func() ([]net.Addr, error)) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{"https://" + net.JoinHostPort(host, port)}
	}
	var urls []string
	if addrs, err := interfaceAddrs(); err == nil {
		v4only := host == "0.0.0.0"
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || !n.IP.IsPrivate() || (v4only && n.IP.To4() == nil) {
				continue
			}
			urls = append(urls, "https://"+net.JoinHostPort(n.IP.String(), port))
		}
	}
	if len(urls) == 0 {
		urls = []string{"https://" + net.JoinHostPort("127.0.0.1", port)}
	}
	return urls
}

// tlsInfo is what the dashboard shows administrators about id.
func tlsInfo(id *tlsid.Identity, urls []string) *httpapi.TLSInfo {
	info := &httpapi.TLSInfo{SPKISHA256: id.SPKIHex(), CertSHA256: id.CertHex(), NotAfter: id.Leaf.NotAfter}
	for _, u := range urls {
		info.Pairing = append(info.Pairing, httpapi.Pairing{URL: u, Payload: tlsid.PairingPayload(u, id.SPKIHex())})
	}
	return info
}

// writeFingerprints prints the pin and pairing payloads (never the key).
func writeFingerprints(w io.Writer, id *tlsid.Identity, urls []string) {
	fmt.Fprintf(w, "TLS public key pin (SPKI SHA-256): %s\n", tlsid.Colons(id.SPKIHex()))
	fmt.Fprintf(w, "TLS pin for curl --pinnedpubkey:   sha256//%s\n", id.SPKIBase64())
	fmt.Fprintf(w, "TLS certificate SHA-256:           %s\n", tlsid.Colons(id.CertHex()))
	fmt.Fprintf(w, "TLS certificate valid until:       %s\n", id.Leaf.NotAfter.UTC().Format(time.RFC3339))
	for _, u := range urls {
		fmt.Fprintf(w, "Pairing payload for %s: %s\n", u, tlsid.PairingPayload(u, id.SPKIHex()))
	}
}

// loopbackHostOnly protects the plain HTTP loopback listener against DNS
// rebinding: a web page of another site whose name was re-pointed at
// 127.0.0.1 reaches the listener with its own name in Host. Only requests
// addressed to localhost or a loopback IP are served; others get 421.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !isLoopbackHost(strings.Trim(strings.ToLower(host), "[]")) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusMisdirectedRequest)
			io.WriteString(w, `{"error":"invalid_host"}`+"\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newHTTPServer sets the limits of the listener. ReadHeaderTimeout also
// bounds the TLS handshake. There is no ReadTimeout or WriteTimeout: an
// upload or download of a large original over Wi-Fi may legitimately take
// many minutes, and only authenticated requests read or write large bodies.
func newHTTPServer(h http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{Handler: h, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 64 << 10, ErrorLog: log.Default()}
}

// runTLSInfo implements "withalbum-server tls [-json]": it prints the
// fingerprints and pairing payloads of the existing identity without
// creating or changing anything (safe while the server runs).
func runTLSInfo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Print as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: withalbum-server tls [-json]")
		fmt.Fprintln(stderr, "Prints the TLS public key pin and the pairing payloads for WITHALBUM_LISTEN. The identity is created by the first start with -tls.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitUsage
	}
	vol, ok := openSource("tls", stderr)
	if !ok {
		return exitProblems
	}
	id, err := tlsid.Load(vol.Root())
	if errors.Is(err, tlsid.ErrNotFound) {
		fmt.Fprintln(stderr, "tls: no TLS identity yet; start the server once with -tls to create it.")
		return exitProblems
	}
	if err != nil {
		fmt.Fprintf(stderr, "tls: %v\n", err)
		return exitProblems
	}
	urls := serverURLs(listenAddr(), net.InterfaceAddrs)
	if *asJSON {
		info := tlsInfo(id, urls)
		if err := printJSON(stdout, map[string]any{"spki_sha256": info.SPKISHA256, "spki_sha256_base64": id.SPKIBase64(),
			"cert_sha256": info.CertSHA256, "not_after": info.NotAfter.UTC().Format(time.RFC3339), "pairing": info.Pairing}); err != nil {
			fmt.Fprintf(stderr, "tls: %v\n", err)
			return exitProblems
		}
		return exitOK
	}
	writeFingerprints(stdout, id, urls)
	return exitOK
}
