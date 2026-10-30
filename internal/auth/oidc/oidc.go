// Package oidc verifies Google and Apple ID tokens for "sign in with"
// (M54). The server checks every token itself against the provider's
// published signing keys: signature (RS256), issuer, audience (this app's
// client IDs), expiry and the nonce the server handed out. No provider
// secret is needed or kept; the only outbound request is fetching the
// public keys (JWKS), cached and refreshed at most once a minute.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Provider is an identity provider.
type Provider string

const (
	Google Provider = "google"
	Apple  Provider = "apple"
)

// Providers in display order.
var Providers = []Provider{Google, Apple}

// Valid reports whether p is a known provider.
func (p Provider) Valid() bool { return p == Google || p == Apple }

// Published key sets and issuers.
const (
	GoogleKeysURL = "https://www.googleapis.com/oauth2/v3/certs"
	AppleKeysURL  = "https://appleid.apple.com/auth/keys"
)

var issuers = map[Provider][]string{
	Google: {"https://accounts.google.com", "accounts.google.com"},
	Apple:  {"https://appleid.apple.com"},
}

var (
	// ErrInvalidToken: malformed, wrongly signed, expired, for another
	// app, from another issuer, or with another nonce.
	ErrInvalidToken = errors.New("invalid identity token")
	// ErrDisabled: no client ID is configured for the provider.
	ErrDisabled = errors.New("sign-in with this provider is not configured")
	// ErrKeys: the provider's keys could not be fetched.
	ErrKeys = errors.New("identity provider keys unavailable")
)

// Config configures a Verifier. A provider without audiences is off.
type Config struct {
	// GoogleAudiences: the app's OAuth client IDs (iOS client, and the web
	// "server" client the Android app requests tokens for).
	GoogleAudiences []string
	// AppleAudiences: the app's bundle ID (native sign-in) and any
	// Services ID (web flow).
	AppleAudiences []string
	// HTTPClient fetches keys (nil: a client with a 10 s timeout).
	HTTPClient *http.Client
	// KeysURL overrides the key set URL per provider (tests).
	KeysURL map[Provider]string
	Now     func() time.Time // tests; nil means time.Now
}

// Claims are the verified facts of a token.
type Claims struct {
	Provider Provider
	Subject  string // stable per provider and account
	Email    string // may be empty (Apple relay addresses are fine too)
}

// Verifier checks ID tokens. Safe for concurrent use.
type Verifier struct {
	aud    map[Provider][]string
	urls   map[Provider]string
	client *http.Client
	now    func() time.Time

	mu   sync.Mutex
	sets map[Provider]*keySet
}

type keySet struct {
	keys    map[string]*rsa.PublicKey
	fetched time.Time // last fetch attempt
	ok      time.Time // last successful fetch
}

const (
	skew          = 2 * time.Minute
	refetchMin    = time.Minute
	keysMaxAge    = 6 * time.Hour
	maxKeysBody   = 64 << 10
	maxTokenBytes = 8 << 10
)

// New returns a Verifier for cfg.
func New(cfg Config) *Verifier {
	v := &Verifier{aud: map[Provider][]string{}, urls: map[Provider]string{Google: GoogleKeysURL, Apple: AppleKeysURL},
		client: cfg.HTTPClient, now: cfg.Now, sets: map[Provider]*keySet{}}
	for p, list := range map[Provider][]string{Google: cfg.GoogleAudiences, Apple: cfg.AppleAudiences} {
		for _, a := range list {
			if a = strings.TrimSpace(a); a != "" {
				v.aud[p] = append(v.aud[p], a)
			}
		}
	}
	for p, u := range cfg.KeysURL {
		v.urls[p] = u
	}
	if v.client == nil {
		v.client = &http.Client{Timeout: 10 * time.Second}
	}
	if v.now == nil {
		v.now = time.Now
	}
	return v
}

// Enabled lists the configured providers in display order.
func (v *Verifier) Enabled() []Provider {
	if v == nil {
		return nil
	}
	var out []Provider
	for _, p := range Providers {
		if len(v.aud[p]) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// NonceMatches reports whether a token's nonce claim binds the nonce the
// server issued: the nonce itself (Google) or its SHA-256 in hex (the
// usual Sign in with Apple flow, where the app passes the hash to Apple).
func NonceMatches(claim, issued string) bool {
	if claim == "" || issued == "" {
		return false
	}
	sum := sha256.Sum256([]byte(issued))
	h := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(claim), []byte(issued)) == 1 ||
		subtle.ConstantTimeCompare([]byte(strings.ToLower(claim)), []byte(h)) == 1
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type payload struct {
	Iss   string          `json:"iss"`
	Sub   string          `json:"sub"`
	Aud   json.RawMessage `json:"aud"`
	Exp   json.Number     `json:"exp"`
	Iat   json.Number     `json:"iat"`
	Nonce string          `json:"nonce"`
	Email string          `json:"email"`
}

func (p payload) audiences() []string {
	var one string
	if json.Unmarshal(p.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(p.Aud, &many) == nil {
		return many
	}
	return nil
}

func unix(n json.Number) (time.Time, bool) {
	f, err := n.Float64()
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func segment(s string, v any) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	return dec.Decode(v) == nil
}

// Verify checks token for provider p and the issued nonce. Every token
// problem is ErrInvalidToken; ErrDisabled and ErrKeys are configuration
// and network problems.
func (v *Verifier) Verify(ctx context.Context, p Provider, token, nonce string) (Claims, error) {
	if v == nil || len(v.aud[p]) == 0 {
		return Claims{}, ErrDisabled
	}
	if len(token) > maxTokenBytes {
		return Claims{}, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	var h header
	var pl payload
	if !segment(parts[0], &h) || !segment(parts[1], &pl) || h.Alg != "RS256" || h.Kid == "" {
		return Claims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	key, err := v.key(ctx, p, h.Kid)
	if err != nil {
		return Claims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return Claims{}, ErrInvalidToken
	}
	now := v.now()
	exp, ok1 := unix(pl.Exp)
	iat, ok2 := unix(pl.Iat)
	switch {
	case !ok1 || !ok2, !now.Before(exp.Add(skew)), iat.After(now.Add(skew)):
		return Claims{}, ErrInvalidToken
	case !slices.Contains(issuers[p], pl.Iss):
		return Claims{}, ErrInvalidToken
	case pl.Sub == "" || len(pl.Sub) > 255:
		return Claims{}, ErrInvalidToken
	case !slices.ContainsFunc(pl.audiences(), func(a string) bool { return slices.Contains(v.aud[p], a) }):
		return Claims{}, ErrInvalidToken
	case !NonceMatches(pl.Nonce, nonce):
		return Claims{}, ErrInvalidToken
	}
	email := pl.Email
	if len(email) > 320 {
		email = ""
	}
	return Claims{Provider: p, Subject: pl.Sub, Email: email}, nil
}

// key returns the signing key kid of p, fetching the key set when it is
// unknown or stale (at most once a minute per provider).
func (v *Verifier) key(ctx context.Context, p Provider, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	ks := v.sets[p]
	if ks != nil && now.Sub(ks.ok) < keysMaxAge {
		if k := ks.keys[kid]; k != nil {
			return k, nil
		}
	}
	if ks != nil && now.Sub(ks.fetched) < refetchMin {
		if k := ks.keys[kid]; k != nil && now.Sub(ks.ok) < 2*keysMaxAge {
			return k, nil
		}
		if ks.ok.IsZero() {
			return nil, ErrKeys // the last attempt failed and none succeeded
		}
		return nil, ErrInvalidToken // unknown key, fetched just now
	}
	if ks == nil {
		ks = &keySet{}
		v.sets[p] = ks
	}
	ks.fetched = now
	keys, err := v.fetch(ctx, v.urls[p])
	if err != nil {
		if k := ks.keys[kid]; k != nil && now.Sub(ks.ok) < 2*keysMaxAge {
			return k, nil // provider unreachable: keep using recent keys
		}
		return nil, ErrKeys
	}
	ks.keys, ks.ok = keys, now
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, ErrInvalidToken
}

func (v *Verifier) fetch(ctx context.Context, url string) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keys: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeysBody)).Decode(&set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(eb) == 0 || len(eb) > 4 {
			continue
		}
		n := new(big.Int).SetBytes(nb)
		if n.BitLen() < 2048 {
			continue
		}
		e := int(new(big.Int).SetBytes(eb).Int64())
		if e < 3 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: n, E: e}
	}
	if len(out) == 0 {
		return nil, errors.New("keys: no usable RSA key")
	}
	return out, nil
}

// Issuer is the canonical issuer of p.
func Issuer(p Provider) string { return issuers[p][0] }
