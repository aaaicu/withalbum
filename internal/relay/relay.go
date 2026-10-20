package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Config sets the limits of a relay. Zero values take the defaults of
// DefaultConfig; a negative bandwidth means unlimited.
type Config struct {
	// MaxConns: connections of every kind at once (protects file
	// descriptors and memory).
	MaxConns int
	// MaxConnsPerIP: connections at once from one client IP (IPv6: /64).
	MaxConnsPerIP int
	// ConnRate, ConnBurst: new connections per second from one client IP.
	ConnRate  float64
	ConnBurst int
	// MaxTunnels: home servers connected at once; MaxTunnelsPerIP of them
	// from one IP (households behind one carrier NAT share an address).
	MaxTunnels      int
	MaxTunnelsPerIP int
	// MaxStreamsPerTunnel: app connections to one home server at once.
	MaxStreamsPerTunnel int
	// StreamRate, StreamBurst: new app connections per second to one home
	// server (from all clients together).
	StreamRate  float64
	StreamBurst int
	// TunnelBandwidth: bytes per second through one tunnel in each
	// direction (all its streams together). Negative: unlimited.
	TunnelBandwidth int64
	// HandshakeTimeout bounds the TLS handshake and the relay handshake.
	HandshakeTimeout time.Duration
	// StreamIdleTimeout: an app connection that moves no byte in either
	// direction for this long is closed.
	StreamIdleTimeout time.Duration
	// TunnelReadTimeout: a tunnel that sends nothing (not even a PING) for
	// this long is closed.
	TunnelReadTimeout time.Duration
	// MaxTrackedIPs bounds the per-IP table; when it is full of active
	// clients, connections from new IPs are refused.
	MaxTrackedIPs int
	// Logger receives structured events without content (nil: discard).
	Logger *slog.Logger
	// LogIPs adds client IPs to log events (default: not logged).
	LogIPs bool
}

// DefaultConfig is sized for a small public relay; see
// docs/RELAY_OPERATIONS.md.
var DefaultConfig = Config{
	MaxConns:            4096,
	MaxConnsPerIP:       128,
	ConnRate:            5,
	ConnBurst:           60,
	MaxTunnels:          2000,
	MaxTunnelsPerIP:     16,
	MaxStreamsPerTunnel: 32,
	StreamRate:          5,
	StreamBurst:         40,
	TunnelBandwidth:     8 << 20,
	HandshakeTimeout:    10 * time.Second,
	StreamIdleTimeout:   5 * time.Minute,
	TunnelReadTimeout:   75 * time.Second,
	MaxTrackedIPs:       100000,
}

func (c Config) withDefaults() Config {
	d := DefaultConfig
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&c.MaxConns, d.MaxConns)
	set(&c.MaxConnsPerIP, d.MaxConnsPerIP)
	set(&c.ConnBurst, d.ConnBurst)
	set(&c.MaxTunnels, d.MaxTunnels)
	set(&c.MaxTunnelsPerIP, d.MaxTunnelsPerIP)
	set(&c.MaxStreamsPerTunnel, d.MaxStreamsPerTunnel)
	set(&c.StreamBurst, d.StreamBurst)
	set(&c.MaxTrackedIPs, d.MaxTrackedIPs)
	if c.ConnRate <= 0 {
		c.ConnRate = d.ConnRate
	}
	if c.StreamRate <= 0 {
		c.StreamRate = d.StreamRate
	}
	if c.TunnelBandwidth == 0 {
		c.TunnelBandwidth = d.TunnelBandwidth
	}
	for _, p := range []struct{ v, def *time.Duration }{{&c.HandshakeTimeout, &d.HandshakeTimeout},
		{&c.StreamIdleTimeout, &d.StreamIdleTimeout}, {&c.TunnelReadTimeout, &d.TunnelReadTimeout}} {
		if *p.v <= 0 {
			*p.v = *p.def
		}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// Relay accepts home-server tunnels and app connections. It keeps no state
// beyond memory: the registry of connected tunnels and the limit counters.
type Relay struct {
	cfg Config
	tls *tls.Config
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	tunnels   map[string]*tunnel
	ips       map[string]*ipState
	conns     map[net.Conn]struct{}
	listeners []net.Listener
	draining  bool

	nconns   atomic.Int64
	nstreams atomic.Int64
	wg       sync.WaitGroup // connection handlers
	splices  sync.WaitGroup // app connections being relayed
}

type ipState struct {
	conns   int
	tunnels int
	rate    *bucket
}

type tunnel struct {
	rid      string
	sess     *Session
	since    time.Time
	streams  *bucket
	up, down *bucket // server -> app, app -> server
	ipKey    string
}

// New returns a relay serving with the outer TLS configuration tc (the
// relay host's certificate). TLS 1.3 is required and ALPN is added.
func New(cfg Config, tc *tls.Config) *Relay {
	cfg = cfg.withDefaults()
	tc = tc.Clone()
	tc.MinVersion = tls.VersionTLS13
	if !slices.Contains(tc.NextProtos, ALPN) {
		tc.NextProtos = append([]string{ALPN}, tc.NextProtos...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Relay{cfg: cfg, tls: tc, log: cfg.Logger, ctx: ctx, cancel: cancel, tunnels: map[string]*tunnel{},
		ips: map[string]*ipState{}, conns: map[net.Conn]struct{}{}}
}

// Stats is the relay's state for the health endpoint.
type Stats struct {
	Tunnels  int  `json:"tunnels"`
	Streams  int  `json:"streams"`
	Conns    int  `json:"connections"`
	Draining bool `json:"draining"`
}

// Stats reports counts only (no IDs, no addresses).
func (r *Relay) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{Tunnels: len(r.tunnels), Streams: int(r.nstreams.Load()), Conns: int(r.nconns.Load()), Draining: r.draining}
}

// HealthHandler answers GET /healthz with Stats as JSON (200, or 503 while
// draining). Serve it on a private address.
func (r *Relay) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		st := r.Stats()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		status := "ok"
		if st.Draining {
			status = "draining"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			Stats
		}{status, st})
	})
	return mux
}

// Serve accepts connections on ln (plain TCP; the relay does TLS itself)
// until ln fails or Shutdown is called.
func (r *Relay) Serve(ln net.Listener) error {
	r.mu.Lock()
	if r.draining {
		r.mu.Unlock()
		return http.ErrServerClosed
	}
	r.listeners = append(r.listeners, ln)
	r.mu.Unlock()
	var delay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			r.mu.Lock()
			draining := r.draining
			r.mu.Unlock()
			if draining {
				return http.ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, errTemporary) {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handle(c)
		}()
	}
}

var errTemporary = errors.New("temporary")

// Shutdown stops accepting, lets running app connections finish until ctx
// ends, then closes every connection.
func (r *Relay) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.draining = true
	for _, ln := range r.listeners {
		ln.Close()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.splices.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	r.cancel()
	r.mu.Lock()
	for c := range r.conns {
		c.Close()
	}
	for _, t := range r.tunnels {
		t.sess.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return err
}

// admit applies the connection limits before any TLS work.
func (r *Relay) admit(key string, c net.Conn) (string, bool) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return "shutting_down", false
	}
	if len(r.conns) >= r.cfg.MaxConns {
		return "max_conns", false
	}
	st := r.ips[key]
	if st == nil {
		if len(r.ips) >= r.cfg.MaxTrackedIPs {
			r.sweepLocked(now)
		}
		if len(r.ips) >= r.cfg.MaxTrackedIPs {
			return "max_tracked_ips", false
		}
		st = &ipState{rate: newBucket(r.cfg.ConnRate, float64(r.cfg.ConnBurst), now)}
		r.ips[key] = st
	}
	if st.conns >= r.cfg.MaxConnsPerIP {
		return "max_conns_per_ip", false
	}
	if !st.rate.allow(now) {
		return "conn_rate", false
	}
	st.conns++
	r.conns[c] = struct{}{}
	r.nconns.Add(1)
	return "", true
}

func (r *Relay) release(key string, c net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.ips[key]; st != nil {
		st.conns--
	}
	delete(r.conns, c)
	r.nconns.Add(-1)
	if len(r.ips) > r.cfg.MaxTrackedIPs/2 {
		r.sweepLocked(time.Now())
	}
}

// sweepLocked forgets clients without connections whose rate bucket has
// refilled (they are indistinguishable from new ones).
func (r *Relay) sweepLocked(now time.Time) {
	for k, st := range r.ips {
		if st.conns == 0 && st.tunnels == 0 && st.rate.full(now) {
			delete(r.ips, k)
		}
	}
}

func (r *Relay) event(msg string, attrs ...any) { r.log.Info(msg, attrs...) }

func (r *Relay) ipAttr(c net.Conn) []any {
	if !r.cfg.LogIPs {
		return nil
	}
	return []any{"ip", ipOf(c.RemoteAddr())}
}

func writeLine(c net.Conn, s string) error {
	_, err := io.WriteString(c, s+"\n")
	return err
}

func (r *Relay) handle(raw net.Conn) {
	key := ipKey(raw.RemoteAddr())
	if why, ok := r.admit(key, raw); !ok {
		raw.Close()
		r.log.Warn("conn_refused", append([]any{"reason", why}, r.ipAttr(raw)...)...)
		return
	}
	defer r.release(key, raw)
	defer raw.Close()
	tc := tls.Server(raw, r.tls)
	tc.SetDeadline(time.Now().Add(r.cfg.HandshakeTimeout))
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.HandshakeTimeout)
	err := tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		r.log.Debug("tls_handshake_failed", "err", err.Error())
		return
	}
	br := bufio.NewReaderSize(tc, 4096)
	line, err := readLine(br)
	if err != nil {
		writeLine(tc, "ERR "+ErrCodeBadRequest)
		r.event("handshake_failed", "reason", "read")
		return
	}
	req, code := parseRequest(line)
	if code != "" {
		writeLine(tc, "ERR "+code)
		r.event("handshake_failed", "reason", code)
		return
	}
	switch req.verb {
	case "CONNECT":
		r.serveApp(tc, br, req.rid, raw)
	case "SERVE":
		r.serveTunnel(tc, br, key, raw)
	}
}

func (r *Relay) lookup(rid string) *tunnel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunnels[rid]
}

// serveApp relays one app connection to the tunnel of rid.
func (r *Relay) serveApp(tc *tls.Conn, br *bufio.Reader, rid string, raw net.Conn) {
	refuse := func(code string) {
		writeLine(tc, "ERR "+code)
		r.event("connect_refused", "rid", rid, "reason", code)
	}
	t := r.lookup(rid)
	if t == nil {
		refuse(ErrCodeOffline)
		return
	}
	if !t.streams.allow(time.Now()) {
		refuse(ErrCodeRateLimited)
		return
	}
	r.mu.Lock()
	draining := r.draining
	if !draining {
		r.splices.Add(1)
	}
	r.mu.Unlock()
	if draining {
		refuse(ErrCodeShutdown)
		return
	}
	defer r.splices.Done()
	st, err := t.sess.OpenStream(ipOf(raw.RemoteAddr()))
	if err != nil {
		if errors.Is(err, ErrTooManyStreams) {
			refuse(ErrCodeBusy)
		} else {
			refuse(ErrCodeOffline)
		}
		return
	}
	if err := writeLine(tc, "OK"); err != nil {
		st.Close()
		return
	}
	tc.SetDeadline(time.Time{})
	r.nstreams.Add(1)
	defer r.nstreams.Add(-1)
	start := time.Now()
	up, down, reason := r.splice(tc, br, st, t)
	r.event("stream_end", "rid", rid, "stream", st.ID(), "ms", time.Since(start).Milliseconds(),
		"bytes_to_server", up, "bytes_to_app", down, "reason", reason)
}

// splice copies bytes both ways until both directions end, the connection
// is idle for StreamIdleTimeout, or the tunnel goes away. It never looks
// at the bytes.
func (r *Relay) splice(app *tls.Conn, appReader io.Reader, st *Stream, t *tunnel) (up, down int64, reason string) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	var toServer, toApp atomic.Int64
	errs := make(chan error, 2)
	go func() {
		err := pipe(ctx, st, appReader, t.down, &last, &toServer)
		st.CloseWrite()
		errs <- err
	}()
	go func() {
		err := pipe(ctx, app, st, t.up, &last, &toApp)
		app.CloseWrite()
		errs <- err
	}()
	tick := time.NewTicker(min(r.cfg.StreamIdleTimeout/4, 15*time.Second))
	defer tick.Stop()
	reason = "closed"
	pending := 2
	tunnelDone, shutdown := t.sess.Done(), r.ctx.Done()
	for pending > 0 {
		select {
		case err := <-errs:
			pending--
			if err != nil && reason == "closed" {
				reason = "error"
				if errors.Is(err, ErrStreamReset) {
					reason = "reset"
				}
				// One side failed: end the other too.
				app.Close()
				st.Close()
			}
		case <-tick.C:
			if time.Since(time.Unix(0, last.Load())) > r.cfg.StreamIdleTimeout {
				reason = "idle"
				app.Close()
				st.Close()
			}
		case <-tunnelDone:
			tunnelDone = nil
			if reason == "closed" {
				reason = "tunnel_down"
			}
			app.Close()
		case <-shutdown:
			shutdown = nil
			if reason == "closed" {
				reason = "shutdown"
			}
			app.Close()
			st.Close()
		}
	}
	app.Close()
	st.Close()
	return toServer.Load(), toApp.Load(), reason
}

// pipe copies src to dst, paced by lim, recording activity and the count.
// A clean EOF is nil.
func pipe(ctx context.Context, dst io.Writer, src io.Reader, lim *bucket, last, n *atomic.Int64) error {
	buf := make([]byte, MaxDataPayload)
	for {
		k, err := src.Read(buf)
		if k > 0 {
			last.Store(time.Now().UnixNano())
			if werr := lim.waitN(ctx, k); werr != nil {
				return werr
			}
			if _, werr := dst.Write(buf[:k]); werr != nil {
				return werr
			}
			n.Add(int64(k))
			last.Store(time.Now().UnixNano())
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// serveTunnel authenticates a home server by its key proof and keeps its
// tunnel until it ends.
func (r *Relay) serveTunnel(tc *tls.Conn, br *bufio.Reader, key string, raw net.Conn) {
	refuse := func(code, why string) {
		writeLine(tc, "ERR "+code)
		r.event("serve_refused", append([]any{"reason", why}, r.ipAttr(raw)...)...)
	}
	nonce := make([]byte, NonceLen)
	rand.Read(nonce)
	if err := writeLine(tc, "CHALLENGE "+b64.EncodeToString(nonce)); err != nil {
		return
	}
	line, err := readLine(br)
	if err != nil {
		refuse(ErrCodeBadRequest, "read")
		return
	}
	spki, sig, ok := parseProve(line)
	if !ok {
		refuse(ErrCodeBadRequest, "malformed_proof")
		return
	}
	cs := tc.ConnectionState()
	exporter, err := cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
	if err != nil {
		refuse(ErrCodeBadRequest, "exporter")
		return
	}
	pin, err := VerifyProof(spki, nonce, exporter, sig)
	if err != nil {
		refuse(ErrCodeUnauthorized, "bad_proof")
		return
	}
	rid := RendezvousID(pin)
	now := time.Now()
	t := &tunnel{rid: rid, since: now, ipKey: key,
		streams: newBucket(r.cfg.StreamRate, float64(r.cfg.StreamBurst), now)}
	if bw := r.cfg.TunnelBandwidth; bw > 0 {
		t.up = newBucket(float64(bw), float64(bw), now)
		t.down = newBucket(float64(bw), float64(bw), now)
	}
	// Check the limits, answer OK, then register: nothing may be written
	// on the connection before the OK line, and an app connecting in the
	// instant between is told "offline" and simply retries.
	if code, why := r.tunnelAllowed(rid, key); code != "" {
		refuse(code, why)
		return
	}
	if err := writeLine(tc, "OK "+rid); err != nil {
		return
	}
	tc.SetDeadline(time.Time{})
	r.mu.Lock()
	if code, why := r.tunnelAllowedLocked(rid, key); code != "" {
		r.mu.Unlock()
		r.event("serve_refused", "reason", why)
		return
	}
	t.sess = NewSession(tc, br, SessionConfig{Opener: true, MaxStreams: r.cfg.MaxStreamsPerTunnel, ReadTimeout: r.cfg.TunnelReadTimeout})
	old := r.tunnels[rid]
	r.tunnels[rid] = t
	if st := r.ips[key]; st != nil {
		st.tunnels++
	}
	if old != nil {
		if st := r.ips[old.ipKey]; st != nil && old.ipKey != "" {
			st.tunnels--
		}
		old.ipKey = ""
	}
	r.mu.Unlock()
	if old != nil {
		// Only the holder of the key can reach this point: it is the same
		// server reconnecting (for example after a network change) and the
		// old tunnel is most likely dead.
		old.sess.Close()
		r.event("tunnel_replaced", "rid", rid, "old_age_s", int(now.Sub(old.since).Seconds()))
	}
	r.event("tunnel_up", append([]any{"rid", rid}, r.ipAttr(raw)...)...)
	<-t.sess.Done()
	r.mu.Lock()
	if r.tunnels[rid] == t {
		delete(r.tunnels, rid)
		if st := r.ips[t.ipKey]; st != nil && t.ipKey != "" {
			st.tunnels--
		}
	}
	r.mu.Unlock()
	reason := "closed"
	if err := t.sess.Err(); err != nil && !errors.Is(err, ErrSessionClosed) {
		reason = err.Error()
		if errors.Is(err, ErrProtocol) {
			reason = "protocol_violation"
		} else if errors.Is(err, io.EOF) {
			reason = "eof"
		}
	}
	r.event("tunnel_down", "rid", rid, "age_s", int(time.Since(t.since).Seconds()), "reason", reason)
}

func (r *Relay) tunnelAllowed(rid, key string) (code, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunnelAllowedLocked(rid, key)
}

// tunnelAllowedLocked applies the tunnel limits. A server replacing its own
// tunnel from the same IP does not count twice.
func (r *Relay) tunnelAllowedLocked(rid, key string) (code, why string) {
	old := r.tunnels[rid]
	ips := r.ips[key]
	switch {
	case r.draining:
		return ErrCodeShutdown, "shutting_down"
	case old == nil && len(r.tunnels) >= r.cfg.MaxTunnels:
		return ErrCodeBusy, "max_tunnels"
	case ips != nil && ips.tunnels >= r.cfg.MaxTunnelsPerIP && (old == nil || old.ipKey != key):
		return ErrCodeBusy, "max_tunnels_per_ip"
	}
	return "", ""
}
