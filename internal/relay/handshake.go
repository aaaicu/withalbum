package relay

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Handshake lines. Every line is ASCII, at most MaxLineLen bytes including
// the final "\n" (a "\r" before it is tolerated), fields separated by one
// space.

// MaxLineLen bounds a handshake line.
const MaxLineLen = 512

// Error codes a relay answers with "ERR <code>\n" before closing.
const (
	ErrCodeBadRequest   = "bad_request"         // malformed line
	ErrCodeVersion      = "unsupported_version" // first word is not Version
	ErrCodeOffline      = "offline"             // no tunnel for this rendezvous ID
	ErrCodeBusy         = "busy"                // a capacity limit is reached
	ErrCodeRateLimited  = "rate_limited"        // too many new connections
	ErrCodeUnauthorized = "unauthorized"        // the key proof failed
	ErrCodeShutdown     = "shutting_down"       // the relay is stopping
)

// ErrLineTooLong: a handshake line exceeded MaxLineLen.
var ErrLineTooLong = errors.New("relay: handshake line too long")

// RefusedError is an "ERR <code>" answer of the relay.
type RefusedError struct{ Code string }

func (e *RefusedError) Error() string { return "relay refused: " + e.Code }

// readLine reads one handshake line without its line end. It never reads
// more than MaxLineLen bytes, and leaves any following bytes in br.
func readLine(br *bufio.Reader) (string, error) {
	var b []byte
	for {
		c, err := br.ReadByte()
		if err != nil {
			if err == io.EOF && len(b) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
		if c == '\n' {
			break
		}
		if len(b) >= MaxLineLen-1 {
			return "", ErrLineTooLong
		}
		b = append(b, c)
	}
	s := strings.TrimSuffix(string(b), "\r")
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "", fmt.Errorf("relay: handshake line has a control or non-ASCII byte")
		}
	}
	return s, nil
}

// request is a parsed first line.
type request struct {
	verb string // "CONNECT" or "SERVE"
	rid  string // CONNECT only
}

// parseRequest parses the first line of a connection. The error is an
// ERR code.
func parseRequest(line string) (request, string) {
	f := strings.Split(line, " ")
	if len(f) < 2 {
		return request{}, ErrCodeBadRequest
	}
	if f[0] != Version {
		return request{}, ErrCodeVersion
	}
	switch {
	case f[1] == "CONNECT" && len(f) == 3 && ValidRendezvousID(f[2]):
		return request{verb: "CONNECT", rid: f[2]}, ""
	case f[1] == "SERVE" && len(f) == 2:
		return request{verb: "SERVE"}, ""
	}
	return request{}, ErrCodeBadRequest
}

var b64 = base64.RawURLEncoding

// parseProve parses "PROVE <spki> <sig>" (base64url without padding).
func parseProve(line string) (spki, sig []byte, ok bool) {
	f := strings.Split(line, " ")
	if len(f) != 3 || f[0] != "PROVE" {
		return nil, nil, false
	}
	spki, err1 := b64.DecodeString(f[1])
	sig, err2 := b64.DecodeString(f[2])
	if err1 != nil || err2 != nil || len(spki) == 0 || len(spki) > 200 || len(sig) == 0 || len(sig) > 80 {
		return nil, nil, false
	}
	return spki, sig, true
}

// parseAnswer interprets an "OK[ <arg>]" or "ERR <code>" line.
func parseAnswer(line string) (arg string, err error) {
	switch {
	case line == "OK":
		return "", nil
	case strings.HasPrefix(line, "OK "):
		return line[3:], nil
	case strings.HasPrefix(line, "ERR "):
		return "", &RefusedError{Code: line[4:]}
	}
	return "", fmt.Errorf("relay: unexpected answer %q", line)
}
