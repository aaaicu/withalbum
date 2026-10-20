package relay

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const relayHost = "relay.test"

// testPKI is a private CA and a certificate for relayHost (the relay's
// outer TLS identity in tests; a real relay uses a public CA).
type testPKI struct {
	pool *x509.CertPool
	cert tls.Certificate
}

func newPKI(t testing.TB) testPKI {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test relay CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: relayHost}, DNSNames: []string{relayHost},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return testPKI{pool: pool, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
}

func (p testPKI) clientTLS() *tls.Config { return &tls.Config{RootCAs: p.pool, ServerName: relayHost} }

// logBuffer collects the relay's JSON log lines.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type testRelay struct {
	*Relay
	addr string
	pki  testPKI
	logs *logBuffer
}

func startRelay(t *testing.T, cfg Config) *testRelay {
	t.Helper()
	pki := newPKI(t)
	logs := &logBuffer{}
	cfg.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := New(cfg, &tls.Config{Certificates: []tls.Certificate{pki.cert}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go r.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.Shutdown(ctx)
	})
	return &testRelay{Relay: r, addr: ln.Addr().String(), pki: pki, logs: logs}
}

type testServer struct {
	client *Client
	key    *ecdsa.PrivateKey
	spki   []byte
	cancel context.CancelFunc
}

// startServer connects a home server with a new key to tr and echoes every
// stream (uppercased) until the test ends.
func startServer(t *testing.T, tr *testRelay, key *ecdsa.PrivateKey) *testServer {
	t.Helper()
	if key == nil {
		key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	c, err := NewClient(ClientConfig{Addr: tr.addr, TLS: tr.pki.clientTLS(), Key: key, SPKI: spki,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	go func() {
		for {
			conn, err := c.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := io.WriteString(conn, strings.ToUpper(line)); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); c.Close() })
	s := &testServer{client: c, key: key, spki: spki, cancel: cancel}
	waitFor(t, "tunnel up", func() bool { return c.Status().Connected && tr.lookup(c.RendezvousID()) != nil })
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dialApp(t *testing.T, tr *testRelay, rid string) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Dial(ctx, tr.addr, rid, tr.pki.clientTLS())
}

func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSuffix(line, "\n")
}

func refusedCode(err error) string {
	var re *RefusedError
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

func TestRelayConnectsAppToServer(t *testing.T) {
	tr := startRelay(t, Config{})
	s := startServer(t, tr, nil)
	if rid := s.client.RendezvousID(); rid != RendezvousID(sha256.Sum256(s.spki)) {
		t.Fatalf("rid %s", rid)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dialApp(t, tr, s.client.RendezvousID())
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			msg := strings.Repeat("hello ", i+1)
			if got := roundTrip(t, c, msg); got != strings.ToUpper(msg) {
				t.Errorf("got %q", got)
			}
		}()
	}
	wg.Wait()
	// Unknown rendezvous ID: offline.
	if _, err := dialApp(t, tr, RendezvousID([32]byte{1})); refusedCode(err) != ErrCodeOffline {
		t.Fatalf("unknown rid: %v", err)
	}
	// Logs carry IDs and counts, never content or IPs.
	waitFor(t, "stream_end logs", func() bool { return strings.Count(tr.logs.String(), `"stream_end"`) == 8 })
	logs := tr.logs.String()
	if strings.Contains(strings.ToLower(logs), "hello") || strings.Contains(logs, "127.0.0.1") {
		t.Fatalf("logs leak content or IPs:\n%s", logs)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %q", line)
		}
	}
	// Health shows counts only.
	w := httptest.NewRecorder()
	tr.HealthHandler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tunnels":1`) || strings.Contains(w.Body.String(), s.client.RendezvousID()) {
		t.Fatalf("health %d %s", w.Code, w.Body)
	}
}

// TestRelayRendezvousCannotBeSquatted: the ID is the relay's own
// computation from a proven key; a server can only ever register its own.
func TestRelayRendezvousCannotBeSquatted(t *testing.T) {
	tr := startRelay(t, Config{})
	victim := startServer(t, tr, nil)
	rid := victim.client.RendezvousID()

	// An attacker presents the victim's public key with its own signature.
	attacker, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	err := rawServe(t, tr, func(nonce, exporter []byte) string {
		sig, _ := SignProof(attacker, nonce, exporter)
		return "PROVE " + b64.EncodeToString(victim.spki) + " " + b64.EncodeToString(sig)
	})
	if refusedCode(err) != ErrCodeUnauthorized {
		t.Fatalf("forged proof: %v", err)
	}
	// A signature captured from another connection is useless (exporter).
	captured, _ := SignProof(victim.key, make([]byte, NonceLen), make([]byte, 32))
	err = rawServe(t, tr, func(nonce, exporter []byte) string {
		return "PROVE " + b64.EncodeToString(victim.spki) + " " + b64.EncodeToString(captured)
	})
	if refusedCode(err) != ErrCodeUnauthorized {
		t.Fatalf("replayed proof: %v", err)
	}
	// With its own valid key, the attacker gets a different ID.
	other := startServer(t, tr, attacker)
	if other.client.RendezvousID() == rid {
		t.Fatal("same rid for another key")
	}
	// The victim still answers its ID.
	c, err := dialApp(t, tr, rid)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "mine"); got != "MINE" {
		t.Fatalf("victim: %q", got)
	}
}

// rawServe runs the SERVE handshake by hand with the PROVE line from prove.
func rawServe(t *testing.T, tr *testRelay, prove func(nonce, exporter []byte) string) error {
	t.Helper()
	tc, err := tls.Dial("tcp", tr.addr, tr.pki.clientTLS())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(tc, Version+" SERVE\n")
	br := bufio.NewReader(tc)
	line, err := readLine(br)
	if err != nil {
		return err
	}
	nonce, _ := b64.DecodeString(strings.TrimPrefix(line, "CHALLENGE "))
	cs := tc.ConnectionState()
	exp, _ := cs.ExportKeyingMaterial(ExporterLabel, nil, 32)
	io.WriteString(tc, prove(nonce, exp)+"\n")
	line, err = readLine(br)
	if err != nil {
		return err
	}
	_, err = parseAnswer(line)
	return err
}

// TestRelayReconnect: a dropped tunnel comes back by itself, and a second
// connection of the same server replaces the first.
func TestRelayReconnect(t *testing.T) {
	tr := startRelay(t, Config{})
	s := startServer(t, tr, nil)
	rid := s.client.RendezvousID()
	first := tr.lookup(rid)
	first.sess.conn.Close() // the network drops the tunnel
	waitFor(t, "reconnect", func() bool {
		cur := tr.lookup(rid)
		return cur != nil && cur != first && s.client.Status().Connected
	})
	c, err := dialApp(t, tr, rid)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, c, "back"); got != "BACK" {
		t.Fatalf("after reconnect: %q", got)
	}
	c.Close()
	// The same key from a second process: the newest tunnel wins.
	second := startServer(t, tr, s.key)
	waitFor(t, "replacement", func() bool { return strings.Contains(tr.logs.String(), `"tunnel_replaced"`) })
	_ = second
	if tr.Stats().Tunnels != 1 {
		t.Fatalf("tunnels %d", tr.Stats().Tunnels)
	}
}

// TestRelayRestart: the relay goes away and comes back on the same
// address; the server reconnects with backoff.
func TestRelayRestart(t *testing.T) {
	pki := newPKI(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	r1 := New(Config{}, &tls.Config{Certificates: []tls.Certificate{pki.cert}})
	go r1.Serve(ln)
	tr := &testRelay{Relay: r1, addr: addr, pki: pki, logs: &logBuffer{}}
	s := startServer(t, tr, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	r1.Shutdown(ctx)
	cancel()
	waitFor(t, "disconnect", func() bool { return !s.client.Status().Connected })
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s again: %v", addr, err)
	}
	r2 := New(Config{}, &tls.Config{Certificates: []tls.Certificate{pki.cert}})
	go r2.Serve(ln2)
	defer r2.Shutdown(context.Background())
	tr2 := &testRelay{Relay: r2, addr: addr, pki: pki}
	waitFor(t, "reconnect to the new relay", func() bool { return tr2.lookup(s.client.RendezvousID()) != nil })
	c, err := dialApp(t, tr2, s.client.RendezvousID())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "again"); got != "AGAIN" {
		t.Fatalf("%q", got)
	}
}

func TestRelayLimits(t *testing.T) {
	t.Run("streams per tunnel", func(t *testing.T) {
		tr := startRelay(t, Config{MaxStreamsPerTunnel: 2})
		s := startServer(t, tr, nil)
		var open []net.Conn
		for range 2 {
			c, err := dialApp(t, tr, s.client.RendezvousID())
			if err != nil {
				t.Fatal(err)
			}
			open = append(open, c)
		}
		if _, err := dialApp(t, tr, s.client.RendezvousID()); refusedCode(err) != ErrCodeBusy {
			t.Fatalf("third stream: %v", err)
		}
		for _, c := range open {
			c.Close()
		}
		waitFor(t, "streams released", func() bool {
			c, err := dialApp(t, tr, s.client.RendezvousID())
			if err == nil {
				c.Close()
			}
			return err == nil
		})
	})
	t.Run("new streams per second", func(t *testing.T) {
		tr := startRelay(t, Config{StreamRate: 0.001, StreamBurst: 3})
		s := startServer(t, tr, nil)
		for i := range 3 {
			c, err := dialApp(t, tr, s.client.RendezvousID())
			if err != nil {
				t.Fatalf("stream %d: %v", i, err)
			}
			c.Close()
		}
		if _, err := dialApp(t, tr, s.client.RendezvousID()); refusedCode(err) != ErrCodeRateLimited {
			t.Fatalf("fourth: %v", err)
		}
	})
	t.Run("connections per IP", func(t *testing.T) {
		tr := startRelay(t, Config{MaxConnsPerIP: 3})
		s := startServer(t, tr, nil) // 1 connection
		a, err := dialApp(t, tr, s.client.RendezvousID())
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		b, err := dialApp(t, tr, s.client.RendezvousID())
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		if c, err := dialApp(t, tr, s.client.RendezvousID()); err == nil {
			c.Close()
			t.Fatal("fourth connection from one IP accepted")
		}
		if !strings.Contains(tr.logs.String(), "max_conns_per_ip") {
			t.Fatalf("no log: %s", tr.logs)
		}
	})
	t.Run("connection rate per IP", func(t *testing.T) {
		tr := startRelay(t, Config{ConnRate: 0.001, ConnBurst: 2})
		s := startServer(t, tr, nil) // takes one token
		c, err := dialApp(t, tr, s.client.RendezvousID())
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if _, err := dialApp(t, tr, s.client.RendezvousID()); err == nil {
			t.Fatal("over the connection rate")
		}
	})
	t.Run("tunnels per IP", func(t *testing.T) {
		tr := startRelay(t, Config{MaxTunnelsPerIP: 1})
		startServer(t, tr, nil)
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		err := rawServe(t, tr, func(nonce, exporter []byte) string {
			spki, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
			sig, _ := SignProof(k, nonce, exporter)
			return "PROVE " + b64.EncodeToString(spki) + " " + b64.EncodeToString(sig)
		})
		if refusedCode(err) != ErrCodeBusy {
			t.Fatalf("second tunnel from one IP: %v", err)
		}
	})
	t.Run("idle stream", func(t *testing.T) {
		tr := startRelay(t, Config{StreamIdleTimeout: 200 * time.Millisecond})
		s := startServer(t, tr, nil)
		c, err := dialApp(t, tr, s.client.RendezvousID())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		roundTrip(t, c, "x")
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("idle connection still open: %v", err)
		}
		waitFor(t, "idle log", func() bool { return strings.Contains(tr.logs.String(), `"reason":"idle"`) })
	})
	t.Run("bandwidth", func(t *testing.T) {
		const rate = 256 << 10
		tr := startRelay(t, Config{TunnelBandwidth: rate})
		s := startServer(t, tr, nil)
		c, err := dialApp(t, tr, s.client.RendezvousID())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		// The first second is the burst; 3 x rate takes about 2 s more.
		msg := strings.Repeat("a", 3*rate-1)
		start := time.Now()
		if got := roundTrip(t, c, msg); len(got) != len(msg) {
			t.Fatalf("got %d bytes", len(got))
		}
		if el := time.Since(start); el < 1500*time.Millisecond {
			t.Fatalf("%d bytes each way at %d B/s took only %s", len(msg), rate, el)
		}
	})
	t.Run("handshake timeout and junk", func(t *testing.T) {
		tr := startRelay(t, Config{HandshakeTimeout: 200 * time.Millisecond})
		raw, err := net.Dial("tcp", tr.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := raw.Read(make([]byte, 1)); err == nil {
			t.Fatal("silent connection not closed")
		}
		tc, err := tls.Dial("tcp", tr.addr, tr.pki.clientTLS())
		if err != nil {
			t.Fatal(err)
		}
		defer tc.Close()
		io.WriteString(tc, "GET / HTTP/1.1\r\n\r\n")
		line, _ := bufio.NewReader(tc).ReadString('\n')
		if line != "ERR "+ErrCodeVersion+"\n" {
			t.Fatalf("junk answered %q", line)
		}
	})
}

// TestRelayShutdownDrains: Shutdown waits for running app connections.
func TestRelayShutdownDrains(t *testing.T) {
	tr := startRelay(t, Config{})
	s := startServer(t, tr, nil)
	c, err := dialApp(t, tr, s.client.RendezvousID())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- tr.Shutdown(ctx)
	}()
	waitFor(t, "draining", func() bool { return tr.Stats().Draining })
	// The running connection still works while draining.
	if got := roundTrip(t, c, "drain"); got != "DRAIN" {
		t.Fatalf("%q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned while a connection was open: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	c.Close()
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
