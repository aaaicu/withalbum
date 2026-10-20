// Package relay implements the WithAlbum relay protocol (docs/RELAY_DESIGN.md):
// a home server keeps one outbound connection (the tunnel) to a public relay,
// apps connect to the relay with the server's rendezvous ID, and the relay
// splices each app connection to a stream of that tunnel. The app and the
// home server run their own pinned TLS session through the stream, so the
// relay forwards ciphertext only: it never holds the server key and never
// sees a request, a password, a token or a photo.
//
// Both relay connections (tunnel and app) are TLS 1.3 to the relay's own
// certificate (the outer layer). On top of it:
//
//	app:    WITHALBUM-RELAY/1 CONNECT <rid>\n       relay: OK\n | ERR <code>\n
//	        then raw bytes in both directions (the inner TLS session)
//	server: WITHALBUM-RELAY/1 SERVE\n               relay: CHALLENGE <nonce>\n
//	        PROVE <spki> <signature>\n              relay: OK <rid>\n | ERR <code>\n
//	        then multiplexing frames (frame.go, mux.go)
//
// The rendezvous ID is derived from the server's TLS public key and the
// relay computes it itself from the key the server proves it holds, so no
// one can register under another server's ID.
package relay

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
)

// Version is the protocol name sent first on every relay connection.
const Version = "WITHALBUM-RELAY/1"

// ALPN is the outer TLS application protocol clients should offer.
const ALPN = "withalbum-relay/1"

// ExporterLabel is the TLS exporter label (RFC 8446 7.5) that binds a
// server's proof to its outer TLS connection with the relay, so a proof
// cannot be replayed on another connection or by another relay.
const ExporterLabel = "EXPORTER-withalbum-relay-serve"

// NonceLen is the size of the relay's challenge.
const NonceLen = 32

const (
	ridContext   = "withalbum-relay/1 rendezvous id\x00"
	proofContext = "withalbum-relay/1 serve proof\x00"
)

var ridEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// RendezvousID is the relay address of the server whose TLS pin (SHA-256
// of the SubjectPublicKeyInfo) is pin: the first 160 bits of
// SHA-256(ridContext || pin), in lower-case base32 (32 characters a-z2-7).
// An app that knows the pin can compute it; the pairing payload carries it
// as well.
func RendezvousID(pin [32]byte) string {
	h := sha256.New()
	h.Write([]byte(ridContext))
	h.Write(pin[:])
	sum := h.Sum(nil)
	return toLower(ridEncoding.EncodeToString(sum[:20]))
}

var ridRE = regexp.MustCompile(`^[a-z2-7]{32}$`)

// ValidRendezvousID reports whether s has the form of a rendezvous ID.
func ValidRendezvousID(s string) bool { return ridRE.MatchString(s) }

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// proofDigest is what the server signs: SHA-256 of the context, the relay's
// nonce and the outer connection's exporter value.
func proofDigest(nonce, exporter []byte) []byte {
	h := sha256.New()
	h.Write([]byte(proofContext))
	h.Write(nonce)
	h.Write(exporter)
	return h.Sum(nil)
}

// SignProof signs the challenge with the server's TLS private key. The
// context string keeps it distinct from a TLS 1.3 CertificateVerify
// signature made with the same key.
func SignProof(key crypto.Signer, nonce, exporter []byte) ([]byte, error) {
	if _, ok := key.Public().(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("relay: unsupported server key %T (want ECDSA P-256)", key.Public())
	}
	return key.Sign(rand.Reader, proofDigest(nonce, exporter), crypto.SHA256)
}

// ErrBadProof: the key proof is malformed or its signature is wrong.
var ErrBadProof = errors.New("relay: invalid key proof")

// VerifyProof checks a server's proof and returns the pin of the proven key.
// Only ECDSA P-256 keys are accepted (internal/tlsid creates them).
func VerifyProof(spkiDER, nonce, exporter, sig []byte) ([32]byte, error) {
	pub, err := x509.ParsePKIXPublicKey(spkiDER)
	if err != nil {
		return [32]byte{}, ErrBadProof
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return [32]byte{}, ErrBadProof
	}
	if !ecdsa.VerifyASN1(ec, proofDigest(nonce, exporter), sig) {
		return [32]byte{}, ErrBadProof
	}
	return sha256.Sum256(spkiDER), nil
}
