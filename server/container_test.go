package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLANURLsSetting(t *testing.T) {
	get := func(v string) func(string) string {
		return func(k string) string {
			if k == "WITHALBUM_LAN_URL" {
				return v
			}
			return ""
		}
	}
	if got, err := lanURLsSetting(get("")); err != nil || got != nil {
		t.Fatalf("unset: %v %v", got, err)
	}
	got, err := lanURLsSetting(get(" https://192.168.0.10:8787 , https://[fd00::5]:9443/ "))
	if err != nil || len(got) != 2 || got[0] != "https://192.168.0.10:8787" || got[1] != "https://[fd00::5]:9443" {
		t.Fatalf("two: %v %v", got, err)
	}
	for _, bad := range []string{
		"http://192.168.0.10:8787",      // plain HTTP
		"https://192.168.0.10",          // no port
		"https://8.8.8.8:8787",          // public
		"https://home.lan:8787",         // a name
		"https://127.0.0.1:8787",        // loopback: a phone cannot use it
		"https://192.168.0.10:8787/x",   // path
		"https://u@192.168.0.10:8787",   // user
		"https://192.168.0.10:8787?a=b", // query
		"https://192.168.0.10:8787,",    // empty entry
	} {
		if _, err := lanURLsSetting(get(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPairingUsesLANURLs(t *testing.T) {
	ifs := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("172.18.0.2"), Mask: net.CIDRMask(16, 32)}}, nil
	}
	lan := []string{"https://192.168.0.10:8787"}
	if got := (listenConfig{Addr: "0.0.0.0:8787", LANURLs: lan}).pairing("", ifs); len(got) != 1 || got[0] != lan[0] {
		t.Fatalf("wildcard with LAN URL: %v", got)
	}
	if got := (listenConfig{Addr: "0.0.0.0:8787"}).pairing("", ifs); len(got) != 1 || got[0] != "https://172.18.0.2:8787" {
		t.Fatalf("wildcard without LAN URL: %v", got)
	}
	// A loopback listener is not reachable from the network: the LAN URL
	// does not apply (the public route or loopback does).
	if got := (listenConfig{Addr: "127.0.0.1:8787", LANURLs: lan}).pairing("", ifs); len(got) != 1 || got[0] != "https://127.0.0.1:8787" {
		t.Fatalf("loopback: %v", got)
	}
}

func TestCheckFunnelContainer(t *testing.T) {
	pub := "https://home.example-tailnet.ts.net"
	lc := listenConfig{Addr: "0.0.0.0:8787", TLS: true, AllowLAN: true}
	if _, err := checkFunnel(lc, pub, "0.0.0.0:8788"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("outside a container a wildcard funnel listener is refused: %v", err)
	}
	lc.Container = true
	for _, ok := range []string{"0.0.0.0:8788", ":8788", "172.18.0.2:8788", "127.0.0.1:8788"} {
		if _, err := checkFunnel(lc, pub, ok); err != nil {
			t.Errorf("container %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"8.8.8.8:8788", "funnel.lan:8788", "localhost:8788", "0.0.0.0:8787"} {
		if _, err := checkFunnel(lc, pub, bad); err == nil {
			t.Errorf("container %q accepted", bad)
		}
	}
	ln, err := listenFunnel("0.0.0.0:0", true)
	if err != nil {
		t.Fatalf("container wildcard listen: %v", err)
	}
	ln.Close()
}

func TestContainerSetting(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "true": false, "1": true} {
		if got := containerSetting(func(string) string { return v }); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
}

// TestForward copies bytes both ways unchanged, including a PROXY header
// and a half-close, and connects only to the given target.
func TestForward(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	got := make(chan string, 1)
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c) // until the client's half-close
		got <- string(b)
		_, _ = c.Write([]byte("pong"))
	}()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { serveForward(ctx, front, up.Addr().String()); close(done) }()
	c, err := net.Dial("tcp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	msg := "PROXY TCP4 203.0.113.7 127.0.0.1 51000 443\r\n\x16\x03\x01binary\x00tls"
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, _ := io.ReadAll(bufio.NewReader(c))
	c.Close()
	if s := <-got; s != msg || string(reply) != "pong" {
		t.Fatalf("upstream got %q, client got %q", s, reply)
	}
	cancel()
	front.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not stop")
	}
}

func TestForwardFlags(t *testing.T) {
	var m mapFlags
	for _, bad := range []string{"8787", "0.0.0.0:8787", "0.0.0.0:8787=withalbum", "=withalbum:8787"} {
		if m.Set(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := m.Set("0.0.0.0:8787=withalbum:8787"); err != nil || len(m) != 1 || m[0].target != "withalbum:8787" {
		t.Fatalf("good map: %v %v", m, err)
	}
	if code := runForward(nil, io.Discard); code != exitUsage {
		t.Fatalf("no map: exit %d", code)
	}
}

func TestReach(t *testing.T) {
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	closed.Close()
	var out strings.Builder
	if code := runReach([]string{open.Addr().String(), closedAddr}, &out, io.Discard); code != exitProblems {
		t.Fatalf("exit %d", code)
	}
	if s := out.String(); !strings.Contains(s, "OPEN    "+open.Addr().String()) || !strings.Contains(s, "route   "+closedAddr) || !strings.Contains(s, "NOT ISOLATED") {
		t.Fatalf("output:\n%s", s)
	}
	// 192.0.2.0/24 (TEST-NET-1) is never routed: no answer within the timeout.
	old := reachTimeout
	reachTimeout = 300 * time.Millisecond
	defer func() { reachTimeout = old }()
	out.Reset()
	if code := runReach([]string{"192.0.2.1:9"}, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "all blocked") {
		t.Fatalf("unroutable: exit %d\n%s", code, out.String())
	}
	if code := runReach(nil, io.Discard, io.Discard); code != exitUsage {
		t.Fatalf("no args: %d", code)
	}
}
