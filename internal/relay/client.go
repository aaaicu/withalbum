package relay

import (
	"bufio"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"
)

// ClientConfig configures the home server's side of the tunnel.
type ClientConfig struct {
	// Addr is the relay, host:port.
	Addr string
	// TLS is the outer TLS configuration (nil: system roots, server name
	// from Addr). The relay's certificate is verified normally.
	TLS *tls.Config
	// Key is the server's TLS private key and SPKI the DER
	// SubjectPublicKeyInfo of its certificate: the proof that this server
	// owns its rendezvous ID.
	Key  crypto.Signer
	SPKI []byte
	// Dial opens the TCP connection (nil: net.Dialer).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// MinBackoff, MaxBackoff bound the wait between reconnection attempts
	// (default 1 s and 2 min, doubled per failure, with jitter).
	MinBackoff, MaxBackoff time.Duration
	// PingInterval keeps the tunnel alive through NATs (default 20 s).
	PingInterval time.Duration
	// ReadTimeout: no frame from the relay for this long means the tunnel
	// is dead (default 60 s; the relay answers every PING).
	ReadTimeout time.Duration
	// MaxStreams the relay may open at once (default 256).
	MaxStreams int
	// Logf receives connection state changes (nil: discard). Never content.
	Logf func(format string, args ...any)
}

// ClientStatus is the tunnel state shown to administrators.
type ClientStatus struct {
	Connected bool
	Since     time.Time // of the current state
	LastError string
}

// Client keeps a tunnel to a relay and hands the app connections it
// carries to Accept, like a net.Listener. Run it with Run.
type Client struct {
	cfg ClientConfig
	rid string

	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	status ClientStatus
}

// NewClient validates cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("relay: address %q must be host:port", cfg.Addr)
	}
	if cfg.Key == nil || len(cfg.SPKI) == 0 {
		return nil, errors.New("relay: server key required")
	}
	if cfg.TLS == nil {
		cfg.TLS = &tls.Config{}
	}
	cfg.TLS = cfg.TLS.Clone()
	cfg.TLS.MinVersion = tls.VersionTLS13
	if cfg.TLS.ServerName == "" {
		cfg.TLS.ServerName = host
	}
	cfg.TLS.NextProtos = []string{ALPN}
	if cfg.Dial == nil {
		var d net.Dialer
		cfg.Dial = d.DialContext
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 2 * time.Minute
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 20 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 60 * time.Second
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 256
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Client{cfg: cfg, rid: RendezvousID(sha256.Sum256(cfg.SPKI)), conns: make(chan net.Conn),
		closed: make(chan struct{}), status: ClientStatus{Since: time.Now()}}, nil
}

// RendezvousID is this server's ID at the relay.
func (c *Client) RendezvousID() string { return c.rid }

// Status reports the tunnel state.
func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Client) setStatus(connected bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.Connected != connected {
		c.status.Since = time.Now()
	}
	c.status.Connected = connected
	if err != nil {
		c.status.LastError = err.Error()
	}
}

// Accept returns the next app connection (net.Listener).
func (c *Client) Accept() (net.Conn, error) {
	select {
	case conn := <-c.conns:
		return conn, nil
	case <-c.closed:
		return nil, net.ErrClosed
	}
}

// Close stops Accept and Run.
func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// Addr is the listener address (net.Listener).
func (c *Client) Addr() net.Addr { return Addr{S: "relay:" + c.rid} }

// Run keeps the tunnel up until ctx ends or Close is called, reconnecting
// with exponential backoff and jitter.
func (c *Client) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	failures := 0
	for ctx.Err() == nil {
		start := time.Now()
		sess, err := c.connect(ctx)
		if err == nil {
			c.setStatus(true, nil)
			c.cfg.Logf("relay: connected to %s (rendezvous id %s)", c.cfg.Addr, c.rid)
			c.serve(ctx, sess)
			err = sess.Err()
			c.setStatus(false, err)
			if time.Since(start) > time.Minute {
				failures = 0
			}
		} else {
			c.setStatus(false, err)
		}
		if ctx.Err() != nil {
			return
		}
		failures++
		d := c.backoff(failures)
		c.cfg.Logf("relay: not connected to %s (%v); retrying in %s", c.cfg.Addr, err, d.Round(time.Millisecond))
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// backoff is MinBackoff * 2^(n-1), at most MaxBackoff, of which a random
// half is waited ("equal jitter"), so that many servers that lost the
// relay at once do not reconnect in step.
func (c *Client) backoff(n int) time.Duration {
	d := c.cfg.MinBackoff
	for i := 1; i < n && d < c.cfg.MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, c.cfg.MaxBackoff)
	return d/2 + rand.N(d/2+1) // #nosec G404 -- reconnect jitter, not a secret
}

// serve hands the streams of sess to Accept until it ends.
func (c *Client) serve(ctx context.Context, sess *Session) {
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	defer sess.Close()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		st.SetRemoteAddr(clientAddr(st.Remote()))
		select {
		case c.conns <- st:
		case <-ctx.Done():
			st.Close()
			return
		}
	}
}

// clientAddr is the RemoteAddr of an app connection: the app's IP as the
// relay reported it, in host:port form so that the server's per-IP login
// limits apply per app. The relay is not trusted with more than that: a
// loopback, unspecified or unparsable value becomes "relay-unknown".
func clientAddr(remote string) net.Addr {
	ip := net.ParseIP(remote)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return Addr{S: "relay-unknown:0"}
	}
	return Addr{S: net.JoinHostPort(ip.String(), "0")}
}

// connect opens the outer TLS connection and proves the server key.
func (c *Client) connect(ctx context.Context) (*Session, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := c.cfg.Dial(dctx, "tcp", c.cfg.Addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, c.cfg.TLS)
	deadline, _ := dctx.Deadline()
	tc.SetDeadline(deadline)
	fail := func(err error) (*Session, error) {
		tc.Close()
		return nil, err
	}
	if err := tc.HandshakeContext(dctx); err != nil {
		return fail(err)
	}
	if _, err := io.WriteString(tc, Version+" SERVE\n"); err != nil {
		return fail(err)
	}
	br := bufio.NewReaderSize(tc, 4096)
	line, err := readLine(br)
	if err != nil {
		return fail(err)
	}
	enc, ok := strings.CutPrefix(line, "CHALLENGE ")
	if !ok {
		if _, err := parseAnswer(line); err != nil {
			return fail(err)
		}
		return fail(fmt.Errorf("relay: expected CHALLENGE, got %.40q", line))
	}
	nonce, err := b64.DecodeString(enc)
	if err != nil || len(nonce) != NonceLen {
		return fail(errors.New("relay: malformed CHALLENGE"))
	}
	cs := tc.ConnectionState()
	exporter, err := cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
	if err != nil {
		return fail(err)
	}
	sig, err := SignProof(c.cfg.Key, nonce, exporter)
	if err != nil {
		return fail(err)
	}
	if _, err := io.WriteString(tc, "PROVE "+b64.EncodeToString(c.cfg.SPKI)+" "+b64.EncodeToString(sig)+"\n"); err != nil {
		return fail(err)
	}
	line, err = readLine(br)
	if err != nil {
		return fail(err)
	}
	rid, err := parseAnswer(line)
	if err != nil {
		return fail(err)
	}
	if rid != c.rid {
		return fail(fmt.Errorf("relay: registered under %.40q, want %s", rid, c.rid))
	}
	tc.SetDeadline(time.Time{})
	return NewSession(tc, br, SessionConfig{MaxStreams: c.cfg.MaxStreams, PingInterval: c.cfg.PingInterval, ReadTimeout: c.cfg.ReadTimeout}), nil
}

// Dial connects to the server registered as rid through the relay at addr
// and returns the raw stream, on which the caller runs its own pinned TLS
// session (tlsid.PinnedClientConfig). It is what an app does, written in
// Go for tests and tools; outer may be nil (system roots).
func Dial(ctx context.Context, addr, rid string, outer *tls.Config) (net.Conn, error) {
	if !ValidRendezvousID(rid) {
		return nil, errors.New("relay: invalid rendezvous id")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if outer == nil {
		outer = &tls.Config{}
	}
	outer = outer.Clone()
	outer.MinVersion = tls.VersionTLS13
	if outer.ServerName == "" {
		outer.ServerName = host
	}
	outer.NextProtos = []string{ALPN}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, outer)
	if dl, ok := ctx.Deadline(); ok {
		tc.SetDeadline(dl)
	}
	if err := tc.HandshakeContext(ctx); err != nil {
		tc.Close()
		return nil, err
	}
	if _, err := io.WriteString(tc, Version+" CONNECT "+rid+"\n"); err != nil {
		tc.Close()
		return nil, err
	}
	// Read the answer byte by byte so that nothing after it is consumed.
	var line []byte
	var b [1]byte
	for len(line) < MaxLineLen {
		if _, err := io.ReadFull(tc, b[:]); err != nil {
			tc.Close()
			return nil, err
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if _, err := parseAnswer(strings.TrimSuffix(string(line), "\r")); err != nil {
		tc.Close()
		return nil, err
	}
	tc.SetDeadline(time.Time{})
	return tc, nil
}
