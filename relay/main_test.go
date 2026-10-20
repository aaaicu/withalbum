package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/relay"
)

func TestParseFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		err  string
	}{
		{nil, "certificate"},
		{[]string{"-cert", "c.pem"}, "certificate"},
		{[]string{"-cert", "c", "-key", "k", "-bandwidth", "0"}, "unlimited"},
		{[]string{"-cert", "c", "-key", "k", "-health", "0.0.0.0:9443"}, "loopback or private"},
		{[]string{"-cert", "c", "-key", "k", "-health", "203.0.113.1:9443"}, "loopback or private"},
		{[]string{"-cert", "c", "-key", "k", "extra"}, "unexpected"},
		{[]string{"-cert", "c", "-key", "k"}, ""},
		{[]string{"-cert", "c", "-key", "k", "-health", ""}, ""},
		{[]string{"-cert", "c", "-key", "k", "-bandwidth", "-1"}, ""},
	} {
		_, err := parseFlags(c.args, io.Discard)
		if (err == nil) != (c.err == "") || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("parseFlags(%q) = %v, want %q", c.args, err, c.err)
		}
	}
}

func writeCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"relay.test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0o600)
	return certFile, keyFile
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRunServesAndStops starts the binary's main loop with certificate
// files, checks TLS 1.3 with the relay ALPN and the handshake refusal, and stops it with SIGTERM.
func TestRunServesAndStops(t *testing.T) {
	certFile, keyFile := writeCert(t, t.TempDir())
	var stderr syncBuffer
	ready := make(chan string, 1)
	exit := make(chan int, 1)
	go func() {
		exit <- run([]string{"-listen", "127.0.0.1:0", "-health", "127.0.0.1:0", "-cert", certFile, "-key", keyFile, "-drain", "1s"}, &stderr, func(a string) { ready <- a })
	}()
	var addr string
	select {
	case addr = <-ready:
	case code := <-exit:
		t.Fatalf("run exited %d: %s", code, stderr.String())
	}
	tc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{relay.ALPN}})
	if err != nil {
		t.Fatal(err)
	}
	cs := tc.ConnectionState()
	if cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != relay.ALPN {
		t.Fatalf("version %x alpn %q", cs.Version, cs.NegotiatedProtocol)
	}
	io.WriteString(tc, relay.Version+" CONNECT "+relay.RendezvousID([32]byte{})+"\n")
	b := make([]byte, 64)
	n, _ := tc.Read(b)
	if string(b[:n]) != "ERR offline\n" {
		t.Fatalf("answer %q", b[:n])
	}
	tc.Close()
	// TLS 1.2 is refused.
	if _, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("TLS 1.2 accepted")
	}
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit after SIGTERM")
	}
	logs := stderr.String()
	for _, ev := range []string{"relay_start", "connect_refused", "relay_draining", "relay_stop"} {
		if !strings.Contains(logs, `"msg":"`+ev+`"`) {
			t.Errorf("no %s in log:\n%s", ev, logs)
		}
	}
	if strings.Contains(logs, "127.0.0.1:") && strings.Contains(logs, `"ip"`) {
		t.Errorf("client IPs logged without -log-ips:\n%s", logs)
	}
}

func TestHealthEndpoint(t *testing.T) {
	r := relay.New(relay.Config{}, &tls.Config{})
	w := httptest.NewRecorder()
	r.HealthHandler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) || !strings.Contains(w.Body.String(), `"tunnels":0`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
