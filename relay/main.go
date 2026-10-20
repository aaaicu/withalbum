// Command withalbum-relay is the WithAlbum relay: a public meeting point
// that lets apps reach a home server that opens no inbound port. Home
// servers keep an outbound tunnel to it; apps connect with the server's
// rendezvous ID and the relay splices bytes. Apps and home servers run
// their own pinned TLS session through it, so the relay only ever handles
// ciphertext. See docs/RELAY_DESIGN.md and docs/RELAY_OPERATIONS.md.
//
// It keeps no state on disk and logs no content. The certificate for its
// host name comes from files (-cert, -key), for example from certbot; they
// are re-read when they change.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/relay"
)

// Set with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, nil))
}

// options are the parsed command line.
type options struct {
	listen, health    string
	certFile, keyFile string
	drain             time.Duration
	logLevel          string
	cfg               relay.Config
	tunnelBandwidth   int64
	showVersion       bool
}

func parseFlags(args []string, stderr io.Writer) (*options, error) {
	fs := flag.NewFlagSet("withalbum-relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	d := relay.DefaultConfig
	o := &options{}
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	fs.StringVar(&o.listen, "listen", env("WITHALBUM_RELAY_LISTEN", ":443"), "TCP address for apps and home servers (TLS)")
	fs.StringVar(&o.health, "health", env("WITHALBUM_RELAY_HEALTH", "127.0.0.1:9443"), "plain HTTP address of GET /healthz (keep it private; empty: off)")
	fs.StringVar(&o.certFile, "cert", env("WITHALBUM_RELAY_CERT", ""), "PEM certificate chain for the relay's host name (reloaded when the file changes)")
	fs.StringVar(&o.keyFile, "key", env("WITHALBUM_RELAY_KEY", ""), "PEM private key of -cert")
	fs.DurationVar(&o.drain, "drain", 30*time.Second, "on SIGTERM, how long running app connections may finish")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&o.cfg.LogIPs, "log-ips", false, "include client IP addresses in the log (off: IDs, timings and byte counts only)")
	fs.IntVar(&o.cfg.MaxConns, "max-conns", d.MaxConns, "connections at once")
	fs.IntVar(&o.cfg.MaxConnsPerIP, "max-conns-per-ip", d.MaxConnsPerIP, "connections at once from one IP (IPv6: /64)")
	fs.Float64Var(&o.cfg.ConnRate, "conn-rate", d.ConnRate, "new connections per second from one IP")
	fs.IntVar(&o.cfg.ConnBurst, "conn-burst", d.ConnBurst, "burst of new connections from one IP")
	fs.IntVar(&o.cfg.MaxTunnels, "max-tunnels", d.MaxTunnels, "home servers connected at once")
	fs.IntVar(&o.cfg.MaxTunnelsPerIP, "max-tunnels-per-ip", d.MaxTunnelsPerIP, "home servers from one IP")
	fs.IntVar(&o.cfg.MaxStreamsPerTunnel, "max-streams", d.MaxStreamsPerTunnel, "app connections to one home server at once")
	fs.Float64Var(&o.cfg.StreamRate, "stream-rate", d.StreamRate, "new app connections per second to one home server")
	fs.IntVar(&o.cfg.StreamBurst, "stream-burst", d.StreamBurst, "burst of new app connections to one home server")
	fs.Int64Var(&o.tunnelBandwidth, "bandwidth", d.TunnelBandwidth, "bytes per second through one home server's tunnel, each direction (-1: unlimited)")
	fs.DurationVar(&o.cfg.StreamIdleTimeout, "idle-timeout", d.StreamIdleTimeout, "close an app connection idle this long")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: withalbum-relay -cert FILE -key FILE [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if o.showVersion {
		return o, nil
	}
	o.cfg.TunnelBandwidth = o.tunnelBandwidth
	if o.tunnelBandwidth == 0 {
		return nil, errors.New("-bandwidth 0 would stop all traffic; use -1 for unlimited")
	}
	if o.certFile == "" || o.keyFile == "" {
		return nil, errors.New("a certificate for the relay's host name is required: -cert and -key (PEM)")
	}
	if o.health != "" {
		host, _, err := net.SplitHostPort(o.health)
		if ip := net.ParseIP(host); err != nil || ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
			return nil, fmt.Errorf("-health %q must be a loopback or private IP address and port", o.health)
		}
	}
	return o, nil
}

// certReloader serves -cert/-key, re-reading them at most once a minute
// when their modification time changed (certbot renewals need no restart).
type certReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	mtime             time.Time
	checked           time.Time
}

func (c *certReloader) load() error {
	fi, err := os.Stat(c.certFile)
	if err != nil {
		return err
	}
	ki, err := os.Stat(c.keyFile)
	if err != nil {
		return err
	}
	mt := fi.ModTime()
	if ki.ModTime().After(mt) {
		mt = ki.ModTime()
	}
	if c.cert != nil && mt.Equal(c.mtime) {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	c.cert, c.mtime = &cert, mt
	return nil
}

func (c *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) > time.Minute {
		c.checked = time.Now()
		if err := c.load(); err != nil && c.cert == nil {
			return nil, err
		}
	}
	return c.cert, nil
}

func run(args []string, stderr io.Writer, ready func(addr string)) int {
	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "withalbum-relay: %v\n", err)
		return 2
	}
	if o.showVersion {
		fmt.Fprintln(stderr, "withalbum-relay "+version)
		return 0
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(o.logLevel)); err != nil {
		fmt.Fprintf(stderr, "withalbum-relay: -log-level: %v\n", err)
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	o.cfg.Logger = logger

	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	cr := &certReloader{certFile: o.certFile, keyFile: o.keyFile}
	if err := cr.load(); err != nil {
		fmt.Fprintf(stderr, "withalbum-relay: certificate: %v\n", err)
		return 1
	}
	cr.checked = time.Now()
	tc.GetCertificate = cr.GetCertificate
	r := relay.New(o.cfg, tc)

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		fmt.Fprintf(stderr, "withalbum-relay: %v\n", err)
		return 1
	}
	var health *http.Server
	if o.health != "" {
		hln, err := net.Listen("tcp", o.health)
		if err != nil {
			ln.Close()
			fmt.Fprintf(stderr, "withalbum-relay: -health: %v\n", err)
			return 1
		}
		health = &http.Server{Handler: r.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
		go health.Serve(hln)
	}
	logger.Info("relay_start", "version", version, "listen", ln.Addr().String(), "health", o.health,
		"max_tunnels", o.cfg.MaxTunnels, "max_streams", o.cfg.MaxStreamsPerTunnel, "bandwidth", o.cfg.TunnelBandwidth)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if ready != nil {
		ready(ln.Addr().String())
	}
	errc := make(chan error, 1)
	go func() { errc <- r.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		logger.Error("relay_stopped", "err", err.Error())
		return 1
	}
	logger.Info("relay_draining", "drain", o.drain.String())
	dctx, cancel := context.WithTimeout(context.Background(), o.drain)
	defer cancel()
	if err := r.Shutdown(dctx); err != nil {
		logger.Warn("relay_drain_incomplete", "err", err.Error())
	}
	if health != nil {
		health.Close()
	}
	logger.Info("relay_stop")
	return 0
}
