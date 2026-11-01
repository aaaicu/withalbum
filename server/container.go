package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Running in a container (docs/DOCKER.md).
//
// The image sets WITHALBUM_CONTAINER=1. The server then runs on a Docker
// network with no route out ("internal"): it cannot open a connection to
// the home network, the host or the internet, so a compromised server
// cannot reach a home camera or the router. Phones reach it through a
// second container on both networks, `withalbum-server forward`, which
// only copies bytes between published ports and fixed targets on the
// internal network (TLS stays end to end; a PROXY header from tailscaled
// passes through unchanged).
//
// Container mode changes one rule: the funnel listener may bind to a
// private or wildcard address of the container, because tailscaled is not
// in the container's loopback; it is reachable only from the internal
// network, where only the forwarder connects. The main listener's rules
// (-allow-lan, -tls, an administrator) are unchanged.

// containerSetting reports WITHALBUM_CONTAINER=1.
func containerSetting(getenv func(string) string) bool {
	return getenv("WITHALBUM_CONTAINER") == "1"
}

// lanURLsSetting parses WITHALBUM_LAN_URL: the comma-separated https
// addresses phones on the home network use for this server, when they are
// not the addresses of its interfaces (in a container, the host's LAN
// address and published port). Each is https://<private IP>:<port>.
func lanURLsSetting(getenv func(string) string) ([]string, error) {
	v := strings.TrimSpace(getenv("WITHALBUM_LAN_URL"))
	if v == "" {
		return nil, nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		u, err := parseLANURL(strings.TrimSpace(s))
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func parseLANURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.ForceQuery || u.Port() == "" {
		return "", fmt.Errorf("WITHALBUM_LAN_URL %q must be https://<private IP>:<port> (no path or query)", s)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsPrivate() {
		return "", fmt.Errorf("WITHALBUM_LAN_URL %q: the host must be a private LAN IP address (RFC 1918 or fc00::/7)", s)
	}
	return "https://" + net.JoinHostPort(ip.String(), u.Port()), nil
}

// pairing is pairingURLs, with the configured LAN addresses in place of
// the interface addresses of a non-loopback listener.
func (c listenConfig) pairing(public string, interfaceAddrs func() ([]net.Addr, error)) []string {
	if scope, err := scopeOf(c.Addr); err == nil && scope != scopeLoopback && len(c.LANURLs) > 0 {
		return c.LANURLs
	}
	return pairingURLs(c.Addr, public, interfaceAddrs)
}

// forwardMaxConns bounds the connections one forward mapping serves at once.
const forwardMaxConns = 256

// runForward is `withalbum-server forward -map listen=target ...`: a plain
// TCP forwarder for the edge container of docs/DOCKER.md. It never reads
// or changes the bytes, and connects only to the targets given.
func runForward(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var maps mapFlags
	fs.Var(&maps, "map", "listen=target, e.g. 0.0.0.0:8787=withalbum:8787 (repeatable)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(maps) == 0 || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: withalbum-server forward -map <listen>=<target> [-map ...]")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var lns []net.Listener
	for _, m := range maps {
		ln, err := net.Listen("tcp", m.listen)
		if err != nil {
			fmt.Fprintf(stderr, "forward: %v\n", err)
			return exitProblems
		}
		lns = append(lns, ln)
		log.Printf("forward: %s -> %s", ln.Addr(), m.target)
	}
	var wg sync.WaitGroup
	for i, ln := range lns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveForward(ctx, ln, maps[i].target)
		}()
	}
	<-ctx.Done()
	for _, ln := range lns {
		ln.Close()
	}
	wg.Wait()
	return 0
}

type forwardMap struct{ listen, target string }

type mapFlags []forwardMap

func (m *mapFlags) String() string { return fmt.Sprint(*m) }

func (m *mapFlags) Set(v string) error {
	l, t, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("want listen=target")
	}
	for _, a := range []string{l, t} {
		if _, port, err := net.SplitHostPort(a); err != nil || port == "" {
			return fmt.Errorf("%q: want host:port", a)
		}
	}
	*m = append(*m, forwardMap{l, t})
	return nil
}

// serveForward copies each accepted connection to a new connection to
// target until ln is closed; open connections end when ctx does.
func serveForward(ctx context.Context, ln net.Listener, target string) {
	sem := make(chan struct{}, forwardMaxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			forwardConn(ctx, c, target)
		}()
	}
}

func forwardConn(ctx context.Context, c net.Conn, target string) {
	defer c.Close()
	d := net.Dialer{Timeout: 5 * time.Second}
	up, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		log.Printf("forward: %s: %v", target, err)
		return
	}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { c.Close(); up.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
}

// Reach results of runReach, as the menu bar app's isolation check names
// them.
const (
	reachBlocked = "blocked" // no route, or no answer: isolated
	reachRoute   = "route"   // something answered (refused): a route exists
	reachOpen    = "OPEN"    // connected
)

// reachTimeout bounds one connection attempt of runReach.
var reachTimeout = 2 * time.Second

// runReach is `withalbum-server reach <host:port> ...`: one TCP attempt to
// each address, to check from inside the container that the home network
// cannot be reached (docs/DOCKER.md). It sends nothing. Exit 0 only when
// every address is blocked.
func runReach(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: withalbum-server reach <host:port> [<host:port> ...]   e.g. reach 192.168.0.1:80 192.168.0.20:554")
		return exitUsage
	}
	code := 0
	for _, a := range args {
		if _, port, err := net.SplitHostPort(a); err != nil || port == "" {
			fmt.Fprintf(stderr, "reach: %q: want host:port\n", a)
			return exitUsage
		}
		r, detail := reach(a)
		if r != reachBlocked {
			code = exitProblems
		}
		fmt.Fprintf(stdout, "%-7s %s  (%s)\n", r, a, detail)
	}
	if code == 0 {
		fmt.Fprintln(stdout, "all blocked: this container cannot reach these addresses")
	} else {
		fmt.Fprintln(stdout, "NOT ISOLATED: a connection or an answer came back from at least one address")
	}
	return code
}

func reach(addr string) (string, string) {
	c, err := net.DialTimeout("tcp", addr, reachTimeout) // #nosec G704 -- an operator-typed address on the console, one connection, nothing sent
	if err == nil {
		c.Close()
		return reachOpen, "connected"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return reachRoute, "refused: the address answered"
	}
	return reachBlocked, err.Error()
}
