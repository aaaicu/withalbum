// Package oidc verifies Google and Apple ID tokens for "sign in with"
// (M54). The server checks every token itself against the provider's
// published signing keys: signature (RS256), issuer, audience (this app's
// client IDs), expiry and the nonce the server handed out. No provider
// secret is needed or kept; the only outbound request is fetching the
// public keys (JWKS), cached and refreshed at most once a minute.
//
// With Config.KeysDir set the server makes no request at all: it reads the
// key sets from files that a separate process (withalbum-server oidc-keys,
// the "keys" container of compose.yaml) fetches and writes, so the server
// can stay on a network with no route out (M62).
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
	"os"
	"path/filepath"
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

// invalid is ErrInvalidToken with the failed check, for the server log
// (errors.Is still matches). It never carries the token itself.
func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidToken, reason) }

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
	// KeysDir: read the key sets from <KeysDir>/<provider>.json (written
	// by WriteKeySet) instead of fetching them. A file older than
	// KeysFileMaxAge counts as unavailable.
	KeysDir string
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
	dir    string
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

// KeysFileMaxAge: a key file not rewritten for this long is not trusted
// (the fetcher refreshes every few hours; providers rotate keys over weeks).
const KeysFileMaxAge = 72 * time.Hour

// KeysURLs are the fixed key set addresses the fetcher may contact.
var KeysURLs = map[Provider]string{Google: GoogleKeysURL, Apple: AppleKeysURL}

// New returns a Verifier for cfg.
func New(cfg Config) *Verifier {
	v := &Verifier{aud: map[Provider][]string{}, urls: map[Provider]string{Google: GoogleKeysURL, Apple: AppleKeysURL},
		client: cfg.HTTPClient, now: cfg.Now, sets: map[Provider]*keySet{}, dir: cfg.KeysDir}
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
		return Claims{}, invalid("too_large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, invalid("not_a_jwt")
	}
	var h header
	var pl payload
	if !segment(parts[0], &h) || !segment(parts[1], &pl) || h.Alg != "RS256" || h.Kid == "" {
		return Claims{}, invalid("bad_header")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, invalid("bad_signature_encoding")
	}
	key, err := v.key(ctx, p, h.Kid)
	if err != nil {
		return Claims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return Claims{}, invalid("bad_signature")
	}
	now := v.now()
	exp, ok1 := unix(pl.Exp)
	iat, ok2 := unix(pl.Iat)
	switch {
	case !ok1 || !ok2, !now.Before(exp.Add(skew)), iat.After(now.Add(skew)):
		return Claims{}, invalid("expired_or_clock_skew")
	case !slices.Contains(issuers[p], pl.Iss):
		return Claims{}, invalid("issuer")
	case pl.Sub == "" || len(pl.Sub) > 255:
		return Claims{}, invalid("subject")
	case !slices.ContainsFunc(pl.audiences(), func(a string) bool { return slices.Contains(v.aud[p], a) }):
		// The audience is a public client ID: naming it tells the admin
		// which ID to add to WITHALBUM_*_CLIENT_IDS.
		aud := strings.Join(pl.audiences(), ",")
		if len(aud) > 200 {
			aud = aud[:200]
		}
		return Claims{}, invalid(fmt.Sprintf("audience %q", aud))
	case !NonceMatches(pl.Nonce, nonce):
		return Claims{}, invalid("nonce")
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
		return nil, invalid("unknown_key") // unknown key, fetched just now
	}
	if ks == nil {
		ks = &keySet{}
		v.sets[p] = ks
	}
	ks.fetched = now
	var keys map[string]*rsa.PublicKey
	var err error
	if v.dir != "" {
		keys, err = v.readFile(p, now)
	} else {
		keys, err = v.fetch(ctx, v.urls[p])
	}
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
	keys, _, err := ParseKeySet(resp.Body)
	return keys, err
}

// readFile reads <dir>/<p>.json; a missing, unreadable, invalid or stale
// file is an error (ErrKeys to the caller).
func (v *Verifier) readFile(p Provider, now time.Time) (map[string]*rsa.PublicKey, error) {
	f, err := os.Open(filepath.Join(v.dir, string(p)+".json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("keys: not a regular file")
	}
	if now.Sub(st.ModTime()) > KeysFileMaxAge {
		return nil, fmt.Errorf("keys: %s.json is older than %s", p, KeysFileMaxAge)
	}
	keys, _, err := ParseKeySet(f)
	return keys, err
}

// ParseKeySet reads a JWKS document (at most 64 KiB) and returns its usable
// RSA signing keys (2048 bits or more) and the document's bytes. A document
// without one usable key is an error.
func ParseKeySet(r io.Reader) (map[string]*rsa.PublicKey, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxKeysBody+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxKeysBody {
		return nil, nil, errors.New("keys: document too large")
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
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, nil, err
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
		return nil, nil, errors.New("keys: no usable RSA key")
	}
	return out, raw, nil
}

// FetchKeySet downloads the key set of p from its fixed address and checks
// it (the fetcher; the server itself only reads files when KeysDir is set).
func FetchKeySet(ctx context.Context, client *http.Client, p Provider) ([]byte, error) {
	url, ok := KeysURLs[p]
	if !ok {
		return nil, fmt.Errorf("keys: unknown provider %q", p)
	}
	return FetchKeySetFrom(ctx, client, url)
}

// FetchKeySetFrom is FetchKeySet for a given address (tests).
func FetchKeySetFrom(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keys: %s: HTTP %d", url, resp.StatusCode)
	}
	_, raw, err := ParseKeySet(resp.Body)
	return raw, err
}

// WriteKeySet atomically replaces <dir>/<p>.json with raw (checked again),
// readable by everyone (the server reads it with another user ID).
func WriteKeySet(dir string, p Provider, raw []byte) error {
	if _, _, err := ParseKeySet(strings.NewReader(string(raw))); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+string(p)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o444); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, string(p)+".json"))
}

// Issuer is the canonical issuer of p.
func Issuer(p Provider) string { return issuers[p][0] }
