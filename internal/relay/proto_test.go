package relay

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func newKey(t testing.TB) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k, spki
}

func TestRendezvousID(t *testing.T) {
	var pin [32]byte
	rid := RendezvousID(pin)
	if !ValidRendezvousID(rid) || rid != RendezvousID(pin) {
		t.Fatalf("rid %q", rid)
	}
	// Fixed vector: apps compute the same ID from the pin (docs/RELAY_DESIGN.md).
	h := sha256.Sum256(append([]byte("withalbum-relay/1 rendezvous id\x00"), pin[:]...))
	if want := strings.ToLower(ridEncoding.EncodeToString(h[:20])); rid != want {
		t.Fatalf("rid %q want %q", rid, want)
	}
	// Independent test vector (Python hashlib/base64), also in the design doc.
	if rid != "po5gfj7zpfgccc6nxpldqm3mjf5go5le" {
		t.Fatalf("test vector: %s", rid)
	}
	var one [32]byte
	one[31] = 1
	if got := RendezvousID(one); got != "qvqz3bl57oarddduegea4k7n6yapugfm" {
		t.Fatalf("test vector 2: %s", got)
	}
	pin[0] = 1
	if RendezvousID(pin) == rid {
		t.Fatal("different pins, same rid")
	}
	for _, bad := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32), strings.Repeat("1", 32), strings.Repeat("a", 31) + "="} {
		if ValidRendezvousID(bad) {
			t.Errorf("ValidRendezvousID(%q)", bad)
		}
	}
}

func TestProof(t *testing.T) {
	k, spki := newKey(t)
	other, otherSPKI := newKey(t)
	nonce := bytes.Repeat([]byte{7}, NonceLen)
	exp := bytes.Repeat([]byte{9}, 32)
	sig, err := SignProof(k, nonce, exp)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := VerifyProof(spki, nonce, exp, sig)
	if err != nil || pin != sha256.Sum256(spki) {
		t.Fatalf("valid proof: %v", err)
	}
	otherSig, _ := SignProof(other, nonce, exp)
	exp2 := bytes.Repeat([]byte{8}, 32)
	for name, c := range map[string]struct{ spki, nonce, exp, sig []byte }{
		"another key's signature (squatting)": {spki, nonce, exp, otherSig},
		"signature for another key":           {otherSPKI, nonce, exp, sig},
		"another nonce (replay)":              {spki, exp, exp, sig},
		"another connection (relay MITM)":     {spki, nonce, exp2, sig},
		"garbage signature":                   {spki, nonce, exp, []byte{1, 2, 3}},
		"garbage key":                         {[]byte{1, 2, 3}, nonce, exp, sig},
	} {
		if _, err := VerifyProof(c.spki, c.nonce, c.exp, c.sig); !errors.Is(err, ErrBadProof) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Only P-256.
	ek, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384, _ := x509.MarshalPKIXPublicKey(&ek.PublicKey)
	s384, _ := SignProof(ek, nonce, exp)
	if _, err := VerifyProof(p384, nonce, exp, s384); err == nil {
		t.Error("P-384 accepted")
	}
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := SignProof(edPriv, nonce, exp); err == nil {
		t.Error("Ed25519 signer accepted")
	}
}

func TestParseRequest(t *testing.T) {
	rid := RendezvousID([32]byte{})
	for _, c := range []struct {
		line, verb, code string
	}{
		{Version + " CONNECT " + rid, "CONNECT", ""},
		{Version + " SERVE", "SERVE", ""},
		{"WITHALBUM-RELAY/2 SERVE", "", ErrCodeVersion},
		{"GET / HTTP/1.1", "", ErrCodeVersion},
		{Version + " CONNECT", "", ErrCodeBadRequest},
		{Version + " CONNECT " + rid + " x", "", ErrCodeBadRequest},
		{Version + " CONNECT " + strings.ToUpper(rid), "", ErrCodeBadRequest},
		{Version + "  SERVE", "", ErrCodeBadRequest},
		{Version + " SERVE x", "", ErrCodeBadRequest},
		{Version, "", ErrCodeBadRequest},
		{"", "", ErrCodeBadRequest},
	} {
		req, code := parseRequest(c.line)
		if req.verb != c.verb || code != c.code {
			t.Errorf("parseRequest(%q) = %q %q, want %q %q", c.line, req.verb, code, c.verb, c.code)
		}
	}
}

func TestReadLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("OK\r\nnext\nrest"))
	if l, err := readLine(br); l != "OK" || err != nil {
		t.Fatalf("%q %v", l, err)
	}
	if l, err := readLine(br); l != "next" || err != nil {
		t.Fatalf("%q %v", l, err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != "rest" {
		t.Fatalf("rest %q", rest)
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(strings.Repeat("a", MaxLineLen) + "\n"))); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("long line: %v", err)
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(strings.Repeat("a", MaxLineLen-1) + "\n"))); err != nil {
		t.Fatalf("longest line: %v", err)
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("a\x00b\n"))); err == nil {
		t.Fatal("NUL accepted")
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("abc"))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unterminated: %v", err)
	}
}

func TestFrameCodec(t *testing.T) {
	var b []byte
	b = AppendFrame(b, FrameData, 3, []byte("hello"))
	b = AppendFrame(b, FrameWindow, 3, binary.BigEndian.AppendUint32(nil, 1000))
	b = AppendFrame(b, FramePing, 0, make([]byte, 8))
	b = AppendFrame(b, FrameOpen, 4, []byte("192.0.2.1"))
	b = AppendFrame(b, FrameFin, 3, nil)
	fr := NewFrameReader(bytes.NewReader(b))
	for _, want := range []struct {
		typ byte
		id  uint32
		n   int
	}{{FrameData, 3, 5}, {FrameWindow, 3, 4}, {FramePing, 0, 8}, {FrameOpen, 4, 9}, {FrameFin, 3, 0}} {
		f, err := fr.ReadFrame()
		if err != nil || f.Type != want.typ || f.Stream != want.id || len(f.Payload) != want.n {
			t.Fatalf("frame %+v %v, want %+v", f, err, want)
		}
	}
	if _, err := fr.ReadFrame(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	for name, raw := range map[string][]byte{
		"data on stream 0":   AppendFrame(nil, FrameData, 0, []byte("x")),
		"empty data":         AppendFrame(nil, FrameData, 1, nil),
		"window zero":        AppendFrame(nil, FrameWindow, 1, make([]byte, 4)),
		"window short":       AppendFrame(nil, FrameWindow, 1, make([]byte, 3)),
		"ping on a stream":   AppendFrame(nil, FramePing, 1, make([]byte, 8)),
		"fin with payload":   AppendFrame(nil, FrameFin, 1, []byte("x")),
		"unknown type":       AppendFrame(nil, 9, 1, nil),
		"open too long":      AppendFrame(nil, FrameOpen, 1, make([]byte, MaxOpenPayload+1)),
		"huge length header": {FrameData, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff},
		"data too long":      AppendFrame(nil, FrameData, 1, make([]byte, MaxDataPayload+1)),
	} {
		if _, err := NewFrameReader(bytes.NewReader(raw)).ReadFrame(); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Truncated payload.
	raw := AppendFrame(nil, FrameData, 1, []byte("hello"))
	if _, err := NewFrameReader(bytes.NewReader(raw[:HeaderLen+2])).ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
}

// FuzzReadFrame: the parser never panics, never returns a payload above
// its type's bound, and what it accepts re-encodes to the same bytes.
func FuzzReadFrame(f *testing.F) {
	f.Add(AppendFrame(nil, FrameData, 1, []byte("abc")))
	f.Add(AppendFrame(nil, FramePing, 0, make([]byte, 8)))
	f.Add(AppendFrame(nil, FrameWindow, 2, []byte{0, 0, 1, 0}))
	f.Add([]byte{FrameData, 0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		fr := NewFrameReader(bytes.NewReader(b))
		off := 0
		for {
			fm, err := fr.ReadFrame()
			if err != nil {
				return
			}
			if len(fm.Payload) > MaxDataPayload {
				t.Fatalf("payload %d", len(fm.Payload))
			}
			enc := AppendFrame(nil, fm.Type, fm.Stream, fm.Payload)
			if !bytes.Equal(enc, b[off:off+len(enc)]) {
				t.Fatalf("re-encoding differs")
			}
			off += len(enc)
		}
	})
}

// FuzzHandshake: handshake parsing never panics and accepts only lines
// of the documented forms.
func FuzzHandshake(f *testing.F) {
	f.Add(Version + " CONNECT " + RendezvousID([32]byte{}) + "\n")
	f.Add(Version + " SERVE\r\n")
	f.Add("PROVE abc def\n")
	f.Fuzz(func(t *testing.T, s string) {
		line, err := readLine(bufio.NewReader(strings.NewReader(s)))
		if err != nil {
			return
		}
		if len(line) >= MaxLineLen {
			t.Fatalf("line of %d bytes", len(line))
		}
		if req, code := parseRequest(line); code == "" {
			if (req.verb == "CONNECT") != (line == Version+" CONNECT "+req.rid) || (req.verb == "SERVE" && line != Version+" SERVE") {
				t.Fatalf("accepted %q as %+v", line, req)
			}
		}
		parseProve(line)
		parseAnswer(line)
	})
}
