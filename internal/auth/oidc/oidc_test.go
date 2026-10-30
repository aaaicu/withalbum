package oidc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/oidc/oidctest"
)

const (
	googleAud = "123-ios.apps.googleusercontent.com"
	appleAud  = "aaaicu.withalbum.app"
	nonce     = "server-issued-nonce-0123456789"
)

func verifier(ti *oidctest.TestIssuer, now func() time.Time) *oidc.Verifier {
	return oidc.New(oidc.Config{GoogleAudiences: []string{googleAud, " "}, AppleAudiences: []string{appleAud},
		KeysURL: map[oidc.Provider]string{oidc.Google: ti.Server.URL, oidc.Apple: ti.Server.URL}, Now: now})
}

func TestVerifyAcceptsGoodTokens(t *testing.T) {
	ti := oidctest.NewTestIssuer()
	defer ti.Close()
	v := verifier(ti, nil)
	ctx := context.Background()

	c, err := v.Verify(ctx, oidc.Google, ti.Sign(oidc.Google, oidctest.TokenFields{Sub: "g-1", Aud: googleAud, Nonce: nonce, Email: "a@example.com"}), nonce)
	if err != nil || c.Subject != "g-1" || c.Email != "a@example.com" || c.Provider != oidc.Google {
		t.Fatalf("google: %+v, %v", c, err)
	}
	// Google's issuer without the scheme is valid too.
	if _, err := v.Verify(ctx, oidc.Google, ti.Sign(oidc.Google, oidctest.TokenFields{Iss: "accounts.google.com", Sub: "g-1", Aud: googleAud, Nonce: nonce}), nonce); err != nil {
		t.Fatalf("google short issuer: %v", err)
	}
	// Apple: the app hands Apple the SHA-256 of the issued nonce.
	sum := sha256.Sum256([]byte(nonce))
	if _, err := v.Verify(ctx, oidc.Apple, ti.Sign(oidc.Apple, oidctest.TokenFields{Sub: "a-1", Aud: appleAud, Nonce: hex.EncodeToString(sum[:])}), nonce); err != nil {
		t.Fatalf("apple hashed nonce: %v", err)
	}
	if got := ti.Fetches.Load(); got != 2 {
		t.Fatalf("key fetches = %d, want 2 (one per provider, then cached)", got)
	}
	if got := v.Enabled(); len(got) != 2 || got[0] != oidc.Google || got[1] != oidc.Apple {
		t.Fatalf("enabled = %v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	ti := oidctest.NewTestIssuer()
	defer ti.Close()
	other := oidctest.NewTestIssuer() // same kid, different key
	defer other.Close()
	v := verifier(ti, nil)
	ctx := context.Background()
	ok := oidctest.TokenFields{Sub: "g-1", Aud: googleAud, Nonce: nonce}
	with := func(f func(*oidctest.TokenFields)) string {
		x := ok
		f(&x)
		return ti.Sign(oidc.Google, x)
	}
	good := ti.Sign(oidc.Google, ok)
	tampered := strings.Split(good, ".")
	tampered[1] = strings.Split(with(func(f *oidctest.TokenFields) { f.Sub = "someone-else" }), ".")[1]
	cases := map[string]struct {
		p     oidc.Provider
		token string
		nonce string
	}{
		"other app":             {oidc.Google, with(func(f *oidctest.TokenFields) { f.Aud = "other.apps.googleusercontent.com" }), nonce},
		"wrong nonce":           {oidc.Google, good, "another-nonce"},
		"no nonce in token":     {oidc.Google, with(func(f *oidctest.TokenFields) { f.Nonce = "" }), nonce},
		"empty issued nonce":    {oidc.Google, good, ""},
		"expired":               {oidc.Google, with(func(f *oidctest.TokenFields) { f.Exp = time.Now().Add(-10 * time.Minute) }), nonce},
		"issued in future":      {oidc.Google, with(func(f *oidctest.TokenFields) { f.Iat = time.Now().Add(time.Hour) }), nonce},
		"other issuer":          {oidc.Google, with(func(f *oidctest.TokenFields) { f.Iss = "https://evil.example" }), nonce},
		"apple token as google": {oidc.Google, ti.Sign(oidc.Apple, oidctest.TokenFields{Sub: "a", Aud: googleAud, Nonce: nonce}), nonce},
		"no subject":            {oidc.Google, with(func(f *oidctest.TokenFields) { f.Sub = "" }), nonce},
		"alg none":              {oidc.Google, with(func(f *oidctest.TokenFields) { f.Alg = "none" }), nonce},
		"HS256":                 {oidc.Google, with(func(f *oidctest.TokenFields) { f.Alg = "HS256" }), nonce},
		"other key":             {oidc.Google, other.Sign(oidc.Google, ok), nonce},
		"tampered claims":       {oidc.Google, strings.Join(tampered, "."), nonce},
		"garbage":               {oidc.Google, "not.a.token", nonce},
		"two parts":             {oidc.Google, "a.b", nonce},
		"huge":                  {oidc.Google, strings.Repeat("a", 9000), nonce},
	}
	for name, c := range cases {
		if _, err := v.Verify(ctx, c.p, c.token, c.nonce); !errors.Is(err, oidc.ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
}

func TestVerifyDisabledAndKeys(t *testing.T) {
	ti := oidctest.NewTestIssuer()
	ctx := context.Background()
	off := oidc.New(oidc.Config{AppleAudiences: []string{appleAud}, KeysURL: map[oidc.Provider]string{oidc.Apple: ti.Server.URL}})
	if _, err := off.Verify(ctx, oidc.Google, "x.y.z", nonce); !errors.Is(err, oidc.ErrDisabled) {
		t.Fatalf("google not configured: %v", err)
	}
	if got := off.Enabled(); len(got) != 1 || got[0] != oidc.Apple {
		t.Fatalf("enabled = %v", got)
	}
	var nilV *oidc.Verifier
	if _, err := nilV.Verify(ctx, oidc.Apple, "x.y.z", nonce); !errors.Is(err, oidc.ErrDisabled) || nilV.Enabled() != nil {
		t.Fatalf("nil verifier: %v", err)
	}

	// Provider unreachable before any key was fetched: ErrKeys.
	now := time.Now()
	clock := func() time.Time { return now }
	v := verifier(ti, clock)
	tok := ti.Sign(oidc.Apple, oidctest.TokenFields{Sub: "a", Aud: appleAud, Nonce: nonce})
	if _, err := v.Verify(ctx, oidc.Apple, tok, nonce); err != nil {
		t.Fatalf("first: %v", err)
	}
	ti.Close()
	// Keys fetched recently stay usable while the provider is down.
	now = now.Add(7 * time.Hour)
	if _, err := v.Verify(ctx, oidc.Apple, tok, nonce); err != nil && !errors.Is(err, oidc.ErrInvalidToken) {
		t.Fatalf("stale keys, provider down: %v", err)
	}
	down := verifier(ti, nil)
	if _, err := down.Verify(ctx, oidc.Apple, tok, nonce); !errors.Is(err, oidc.ErrKeys) {
		t.Fatalf("never fetched, provider down: %v", err)
	}
	if _, err := down.Verify(ctx, oidc.Apple, tok, nonce); !errors.Is(err, oidc.ErrKeys) {
		t.Fatalf("second try within a minute: %v", err)
	}
}

func TestUnknownKidRefetchesOncePerMinute(t *testing.T) {
	ti := oidctest.NewTestIssuer()
	defer ti.Close()
	now := time.Now()
	v := verifier(ti, func() time.Time { return now })
	ctx := context.Background()
	unknown := ti.Sign(oidc.Google, oidctest.TokenFields{Sub: "g", Aud: googleAud, Nonce: nonce, Kid: "rotated"})
	for range 5 {
		if _, err := v.Verify(ctx, oidc.Google, unknown, nonce); !errors.Is(err, oidc.ErrInvalidToken) {
			t.Fatalf("unknown kid: %v", err)
		}
	}
	if got := ti.Fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
	// The provider rotates to that key; a minute later it is fetched.
	ti.SetKid("rotated")
	now = now.Add(61 * time.Second)
	if _, err := v.Verify(ctx, oidc.Google, unknown, nonce); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

func TestNonceMatches(t *testing.T) {
	sum := sha256.Sum256([]byte("n"))
	h := hex.EncodeToString(sum[:])
	for _, c := range []struct {
		claim, issued string
		want          bool
	}{{"n", "n", true}, {h, "n", true}, {strings.ToUpper(h), "n", true}, {"m", "n", false}, {"", "", false}, {"n", "", false}, {h, h + "x", false}} {
		if got := oidc.NonceMatches(c.claim, c.issued); got != c.want {
			t.Errorf("NonceMatches(%q, %q) = %v", c.claim, c.issued, got)
		}
	}
}
