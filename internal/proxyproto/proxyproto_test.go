package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func v2(cmd, fam byte, body []byte) []byte {
	b := append([]byte{}, v2sig...)
	b = append(b, 0x20|cmd, fam, 0, 0)
	binary.BigEndian.PutUint16(b[14:], uint16(len(body)))
	return append(b, body...)
}

func v2tcp4(src [4]byte, sport uint16) []byte {
	body := make([]byte, 12)
	copy(body, src[:])
	copy(body[4:], []byte{127, 0, 0, 1})
	binary.BigEndian.PutUint16(body[8:], sport)
	binary.BigEndian.PutUint16(body[10:], 443)
	return v2(1, 0x11, body)
}

func TestRead(t *testing.T) {
	ip6 := netip.MustParseAddr("2001:db8::7").As16()
	body6 := make([]byte, 36+5) // with a TLV block after the addresses
	copy(body6, ip6[:])
	binary.BigEndian.PutUint16(body6[32:], 40000)
	tls := []byte{0x16, 3, 1, 0, 5}
	for _, c := range []struct {
		name string
		in   []byte
		want string // "" = no client
		err  bool
	}{
		{"tls without header", tls, "", false},
		{"v2 tcp4", v2tcp4([4]byte{203, 0, 113, 7}, 5555), "203.0.113.7:5555", false},
		{"v2 tcp6", v2(1, 0x21, body6), "[2001:db8::7]:40000", false},
		{"v2 local", v2(0, 0x00, nil), "", false},
		{"v2 unspec", v2(1, 0x00, nil), "", false},
		{"v2 loopback client", v2tcp4([4]byte{127, 0, 0, 1}, 1), "", false},
		{"v2 short body", v2(1, 0x11, make([]byte, 4)), "", true},
		{"v2 bad family", v2(1, 0x31, make([]byte, 12)), "", true},
		{"v2 bad command", v2(2, 0x11, make([]byte, 12)), "", true},
		{"v2 bad version", append(append([]byte{}, v2sig...), 0x11, 0x11, 0, 0), "", true},
		{"v2 too long", v2(1, 0x11, make([]byte, v2Max+1)), "", true},
		{"v2 bad signature", append([]byte("\r\n\r\nxxxxxxxx"), 0x21, 0x11, 0, 0), "", true},
		{"v1 tcp4", []byte("PROXY TCP4 198.51.100.1 127.0.0.1 1234 443\r\n"), "198.51.100.1:1234", false},
		{"v1 tcp6", []byte("PROXY TCP6 2001:db8::1 ::1 1234 443\r\n"), "[2001:db8::1]:1234", false},
		{"v1 unknown", []byte("PROXY UNKNOWN\r\n"), "", false},
		{"v1 family mismatch", []byte("PROXY TCP4 2001:db8::1 ::1 1234 443\r\n"), "", true},
		{"v1 no crlf", []byte("PROXY TCP4 198.51.100.1 127.0.0.1 1234 443\n"), "", true},
		{"v1 bad port", []byte("PROXY TCP4 198.51.100.1 127.0.0.1 01234 443\r\n"), "", true},
		{"v1 too long", []byte("PROXY TCP4 " + strings.Repeat("1", 200) + "\r\n"), "", true},
		{"not proxy", []byte("PRI * HTTP/2.0\r\n"), "", true},
	} {
		br := bufio.NewReader(bytes.NewReader(append(c.in, tls...)))
		src, err := Read(br)
		if c.err {
			if err == nil {
				t.Errorf("%s: no error (src %v)", c.name, src)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got := ""
		if src.IsValid() {
			got = src.String()
		}
		if got != c.want {
			t.Errorf("%s: src %q, want %q", c.name, got, c.want)
		}
		// The rest of the stream is untouched.
		rest, _ := io.ReadAll(br)
		if c.name == "tls without header" {
			if !bytes.Equal(rest, append(append([]byte{}, tls...), tls...)) {
				t.Errorf("%s: rest %x", c.name, rest)
			}
		} else if !bytes.Equal(rest, tls) && !(c.name == "v2 tcp6" && bytes.Equal(rest, tls)) {
			t.Errorf("%s: rest %x", c.name, rest)
		}
	}
}

func FuzzRead(f *testing.F) {
	f.Add(v2tcp4([4]byte{203, 0, 113, 7}, 5555))
	f.Add([]byte("PROXY TCP4 198.51.100.1 127.0.0.1 1234 443\r\n"))
	f.Add([]byte{0x16, 3, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		br := bufio.NewReaderSize(bytes.NewReader(b), 4096)
		src, err := Read(br)
		if err == nil && src.IsValid() && (src.Addr().IsLoopback() || src.Addr().IsUnspecified()) {
			t.Fatalf("unusable client %v accepted", src)
		}
	})
}

// TestListener: through a real loopback socket, the header sets
// RemoteAddr and the bytes after it arrive unchanged; without a header
// the peer is the Unknown address; a malformed header fails reads.
func TestListener(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &Listener{Listener: raw, Unknown: "funnel-unknown:0", HeaderTimeout: 2 * time.Second}
	defer ln.Close()
	try := func(send []byte) (string, []byte, error) {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write(send)
		c.(*net.TCPConn).CloseWrite()
		defer c.Close()
		sc, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer sc.Close()
		addr := sc.RemoteAddr().String()
		got, err := io.ReadAll(sc)
		return addr, got, err
	}
	if addr, got, err := try(append(v2tcp4([4]byte{203, 0, 113, 9}, 777), "hello"...)); err != nil || addr != "203.0.113.9:777" || string(got) != "hello" {
		t.Fatalf("v2: %s %q %v", addr, got, err)
	}
	if addr, got, err := try([]byte("\x16hello")); err != nil || addr != "funnel-unknown:0" || string(got) != "\x16hello" {
		t.Fatalf("none: %s %q %v", addr, got, err)
	}
	if addr, _, err := try([]byte("PROXY garbage\r\nhello")); !errors.Is(err, ErrHeader) || addr != "funnel-unknown:0" {
		t.Fatalf("malformed: %s %v", addr, err)
	}
}
