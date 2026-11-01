// Package oidctest is a fake Google/Apple identity provider for tests (the
// oidc package's and the HTTP API's). The server binary does not import it.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
)

// TestIssuer is a fake identity provider for tests (this package's and the
// HTTP API's): it serves a key set and signs ID tokens. Not used by the
// server binary.
type TestIssuer struct {
	Server  *httptest.Server
	Key     *rsa.PrivateKey
	mu      sync.Mutex
	kid     string
	Fetches atomic.Int32
}

// NewTestIssuer starts a key set server with one fresh 2048-bit key.
func NewTestIssuer() *TestIssuer {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	ti := &TestIssuer{Key: k, kid: "test-key-1"}
	ti.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ti.Fetches.Add(1)
		pub := ti.Key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": ti.Kid(), "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	}))
	return ti
}

// Kid is the ID of the served key.
func (ti *TestIssuer) Kid() string {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	return ti.kid
}

// SetKid renames the served key (a key rotation).
func (ti *TestIssuer) SetKid(kid string) {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.kid = kid
}

// Close stops the key set server.
func (ti *TestIssuer) Close() { ti.Server.Close() }

// TokenFields are the claims of a test token; zero fields take defaults
// that pass Verify for provider p, audience aud and nonce.
type TokenFields struct {
	Iss, Sub, Aud, Nonce, Email, Kid, Alg string
	Exp, Iat                              time.Time
}

// Sign returns a signed ID token.
func (ti *TestIssuer) Sign(p oidc.Provider, f TokenFields) string {
	now := time.Now()
	if f.Iss == "" {
		f.Iss = oidc.Issuer(p)
	}
	if f.Exp.IsZero() {
		f.Exp = now.Add(time.Hour)
	}
	if f.Iat.IsZero() {
		f.Iat = now
	}
	if f.Kid == "" {
		f.Kid = ti.Kid()
	}
	if f.Alg == "" {
		f.Alg = "RS256"
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	claims := map[string]any{"iss": f.Iss, "sub": f.Sub, "aud": f.Aud, "exp": f.Exp.Unix(), "iat": f.Iat.Unix()}
	if f.Nonce != "" {
		claims["nonce"] = f.Nonce
	}
	if f.Email != "" {
		claims["email"] = f.Email
	}
	signed := enc(map[string]string{"alg": f.Alg, "kid": f.Kid, "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, ti.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}
