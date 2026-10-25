// Package proxyproto reads an optional PROXY protocol header (HAProxy
// PROXY protocol v1 and v2) at the start of a TCP connection, as sent by
// `tailscale funnel --tcp=443 --proxy-protocol=2 ...` (docs/FUNNEL.md), so
// that the server can rate-limit logins per app instead of per forwarder.
//
// The header is optional: a connection that starts with anything else (a
// TLS ClientHello) is passed through untouched with an unknown client.
// The address in a header is trusted only as far as the listener is: use
// it on a loopback listener only, where just local processes (tailscaled)
// can connect. It is only ever a rate-limit and audit key, never an
// authorization input.
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// v2sig is the 12-byte signature of a version 2 header.
var v2sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

const (
	v1Max = 107 // longest v1 line including CRLF
	// v2Max bounds the address and TLV block of a v2 header; Tailscale
	// sends addresses only. Longer headers are refused.
	v2Max = 512
)

// ErrHeader is a malformed PROXY header.
var ErrHeader = errors.New("proxyproto: malformed PROXY header")

// Read consumes a PROXY header at the start of br, if there is one. src is
// the client address it names (invalid for none, UNKNOWN, LOCAL, or an
// unspecified/loopback address). err is ErrHeader for a header that
// started but is malformed, or a read error.
func Read(br *bufio.Reader) (src netip.AddrPort, err error) {
	first, err := br.Peek(1)
	if err != nil {
		return netip.AddrPort{}, err
	}
	switch first[0] {
	case '\r':
		return readV2(br)
	case 'P':
		return readV1(br)
	}
	return netip.AddrPort{}, nil // no header (e.g. a TLS ClientHello, 0x16)
}

func readV1(br *bufio.Reader) (netip.AddrPort, error) {
	head, err := br.Peek(6)
	if err != nil || string(head) != "PROXY " {
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			err = ErrHeader
		}
		return netip.AddrPort{}, err
	}
	var line []byte
	for len(line) < v1Max {
		b, err := br.ReadByte()
		if err != nil {
			return netip.AddrPort{}, err
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return netip.AddrPort{}, ErrHeader
	}
	f := strings.Split(string(line[:len(line)-2]), " ")
	if len(f) >= 2 && f[1] == "UNKNOWN" {
		return netip.AddrPort{}, nil
	}
	if len(f) != 6 || (f[1] != "TCP4" && f[1] != "TCP6") {
		return netip.AddrPort{}, ErrHeader
	}
	ip, err := netip.ParseAddr(f[2])
	if err != nil || ip.Zone() != "" || (f[1] == "TCP4") != ip.Is4() {
		return netip.AddrPort{}, ErrHeader
	}
	if _, err := netip.ParseAddr(f[3]); err != nil {
		return netip.AddrPort{}, ErrHeader
	}
	port, err := strconv.ParseUint(f[4], 10, 16)
	if err != nil || strconv.FormatUint(port, 10) != f[4] {
		return netip.AddrPort{}, ErrHeader
	}
	if _, err := strconv.ParseUint(f[5], 10, 16); err != nil {
		return netip.AddrPort{}, ErrHeader
	}
	return usable(netip.AddrPortFrom(ip, uint16(port))), nil
}

func readV2(br *bufio.Reader) (netip.AddrPort, error) {
	var h [16]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return netip.AddrPort{}, err
	}
	if !bytes.Equal(h[:12], v2sig) || h[12]>>4 != 2 {
		return netip.AddrPort{}, ErrHeader
	}
	cmd, fam := h[12]&0x0f, h[13]
	n := int(binary.BigEndian.Uint16(h[14:16]))
	if cmd > 1 || n > v2Max {
		return netip.AddrPort{}, ErrHeader
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return netip.AddrPort{}, err
	}
	if cmd == 0 { // LOCAL: a health check of the forwarder itself
		return netip.AddrPort{}, nil
	}
	switch fam {
	case 0x11: // TCP over IPv4
		if n < 12 {
			return netip.AddrPort{}, ErrHeader
		}
		ip := netip.AddrFrom4([4]byte(body[0:4]))
		return usable(netip.AddrPortFrom(ip, binary.BigEndian.Uint16(body[8:10]))), nil
	case 0x21: // TCP over IPv6
		if n < 36 {
			return netip.AddrPort{}, ErrHeader
		}
		ip := netip.AddrFrom16([16]byte(body[0:16])).Unmap()
		return usable(netip.AddrPortFrom(ip, binary.BigEndian.Uint16(body[32:34]))), nil
	case 0x00: // UNSPEC
		return netip.AddrPort{}, nil
	}
	return netip.AddrPort{}, ErrHeader
}

// usable drops addresses that cannot be an internet client.
func usable(a netip.AddrPort) netip.AddrPort {
	if ip := a.Addr(); !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return netip.AddrPort{}
	}
	return a
}

// Addr is the RemoteAddr of a connection whose client is not known.
type Addr struct{ S string }

func (a Addr) Network() string { return "tcp" }
func (a Addr) String() string  { return a.S }

// Listener wraps a listener whose peers may send a PROXY header. Accept
// does not read: the header is read on the connection's first Read or
// RemoteAddr (in the server's per-connection goroutine), within
// HeaderTimeout. RemoteAddr is the client the header names, else Unknown.
type Listener struct {
	net.Listener
	// Unknown is the RemoteAddr of connections without a usable client
	// address, e.g. "funnel-unknown:0".
	Unknown string
	// HeaderTimeout bounds the wait for the first bytes (default 10 s).
	HeaderTimeout time.Duration
}

// Accept returns the next connection, wrapped.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t := l.HeaderTimeout
	if t <= 0 {
		t = 10 * time.Second
	}
	return &Conn{Conn: c, br: bufio.NewReaderSize(c, 4096), unknown: Addr{l.Unknown}, timeout: t}, nil
}

// Conn is a connection with its PROXY header (if any) consumed.
type Conn struct {
	net.Conn
	br      *bufio.Reader
	unknown net.Addr
	timeout time.Duration
	once    sync.Once
	remote  net.Addr
	err     error
}

// init reads the header once. net/http asks for RemoteAddr before it sets
// any deadline of its own, so the temporary deadline here does not undo one.
func (c *Conn) init() {
	c.once.Do(func() {
		c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
		src, err := Read(c.br)
		c.Conn.SetReadDeadline(time.Time{})
		c.remote, c.err = c.unknown, err
		if err == nil && src.IsValid() {
			c.remote = net.TCPAddrFromAddrPort(src)
		}
	})
}

// Read reads after the header. A malformed header fails every read.
func (c *Conn) Read(p []byte) (int, error) {
	c.init()
	if c.err != nil {
		return 0, c.err
	}
	return c.br.Read(p)
}

// RemoteAddr is the client from the header, or the Unknown address.
func (c *Conn) RemoteAddr() net.Addr {
	c.init()
	return c.remote
}
