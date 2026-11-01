package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/identity"
	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/session"
)

// Sign in with Google or Apple (M54). An account first links an identity
// with its password (POST /me/sign-ins); afterwards that identity signs in
// instead of the password (POST /auth/sign-in). The server verifies each ID
// token itself (internal/auth/oidc) against a nonce it issued for that one
// attempt, and never creates an account from a sign-in.

func (s *Server) signInRoutes(rt *router) {
	rt.handle("POST /api/v1/auth/sign-in/nonce", s.signInNonce)
	rt.handle("POST /api/v1/auth/sign-in", s.signIn)
	rt.handle("GET /api/v1/me/sign-ins", s.listSignIns)
	rt.handle("POST /api/v1/me/sign-ins", s.linkSignIn)
	rt.handle("DELETE /api/v1/me/sign-ins/{provider}", s.unlinkSignIn)
	rt.handle("GET /api/v1/admin/users/{userID}/sign-ins", s.adminListSignIns)
	rt.handle("DELETE /api/v1/admin/users/{userID}/sign-ins/{provider}", s.adminUnlinkSignIn)
}

// nonceTTL is how long an issued nonce can be used.
const nonceTTL = 10 * time.Minute

// Nonce bookkeeping bounds: per client key, and in all. When either is
// full the oldest nonce is dropped (its attempt then fails), so issuing
// never stops working for everyone.
const (
	noncesPerKey = 32
	noncesTotal  = 8192
)

// nonceStore holds issued nonces (their SHA-256) in memory. A restart
// drops them; the app asks again.
type nonceStore struct {
	mu    sync.Mutex
	exp   map[string]time.Time // hash → expiry
	byKey map[string][]string  // client key → hashes, oldest first
	order []string             // all hashes, oldest first
	now   func() time.Time
}

func newNonceStore() *nonceStore {
	return &nonceStore{exp: map[string]time.Time{}, byKey: map[string][]string{}, now: time.Now}
}

func nonceHash(n string) string {
	sum := sha256.Sum256([]byte(n))
	return string(sum[:])
}

func (ns *nonceStore) issue(key string) (string, time.Time, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	n := base64.RawURLEncoding.EncodeToString(b[:])
	h := nonceHash(n)
	ns.mu.Lock()
	defer ns.mu.Unlock()
	now := ns.now()
	ns.pruneLocked(now)
	if q := ns.byKey[key]; len(q) >= noncesPerKey {
		delete(ns.exp, q[0])
		ns.byKey[key] = q[1:]
	}
	if len(ns.exp) >= noncesTotal {
		for len(ns.order) > 0 && len(ns.exp) >= noncesTotal {
			delete(ns.exp, ns.order[0])
			ns.order = ns.order[1:]
		}
	}
	exp := now.Add(nonceTTL)
	ns.exp[h] = exp
	ns.byKey[key] = append(ns.byKey[key], h)
	ns.order = append(ns.order, h)
	return n, exp, nil
}

// take consumes a nonce: true once, if it was issued and has not expired.
func (ns *nonceStore) take(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	h := nonceHash(n)
	ns.mu.Lock()
	defer ns.mu.Unlock()
	exp, ok := ns.exp[h]
	delete(ns.exp, h)
	return ok && ns.now().Before(exp)
}

// pruneLocked drops expired nonces and the bookkeeping of dropped ones.
func (ns *nonceStore) pruneLocked(now time.Time) {
	live := func(h string) bool { e, ok := ns.exp[h]; return ok && now.Before(e) }
	for h, e := range ns.exp {
		if !now.Before(e) {
			delete(ns.exp, h)
		}
	}
	keep := ns.order[:0]
	for _, h := range ns.order {
		if live(h) {
			keep = append(keep, h)
		}
	}
	ns.order = keep
	for k, q := range ns.byKey {
		kq := q[:0]
		for _, h := range q {
			if live(h) {
				kq = append(kq, h)
			}
		}
		if len(kq) == 0 {
			delete(ns.byKey, k)
		} else {
			ns.byKey[k] = kq
		}
	}
}

func providersJSON(v *oidc.Verifier) []string {
	out := []string{}
	for _, p := range v.Enabled() {
		out = append(out, string(p))
	}
	return out
}

func (s *Server) signInNonce(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	n, exp, err := s.auth.nonces.issue(rateKey(clientIP(r)))
	if err != nil {
		internalError(w, "sign-in nonce", err)
		return
	}
	jsonReply(w, 200, map[string]any{"nonce": n, "expires_at": ts(exp), "providers": providersJSON(s.auth.SignIn)})
}

// verifyIdentity checks provider, nonce and token and writes the error
// reply when they fail: badStatus for a bad token or nonce (401 when
// signing in; 400 on a session route, where 401 means the session ended).
func (s *Server) verifyIdentity(w http.ResponseWriter, r *http.Request, provider, token, nonce string, badStatus int) (oidc.Claims, bool) {
	p := oidc.Provider(provider)
	if !p.Valid() || token == "" || nonce == "" {
		errorReply(w, 400, "invalid_request")
		return oidc.Claims{}, false
	}
	if !s.auth.nonces.take(nonce) {
		errorReply(w, badStatus, "invalid_identity_token")
		return oidc.Claims{}, false
	}
	c, err := s.auth.SignIn.Verify(r.Context(), p, token, nonce)
	switch {
	case err == nil:
		return c, true
	case errors.Is(err, oidc.ErrDisabled):
		errorReply(w, 400, "provider_disabled")
	case errors.Is(err, oidc.ErrKeys):
		w.Header().Set("Retry-After", "30")
		errorReply(w, 503, "provider_unavailable")
	default:
		errorReply(w, badStatus, "invalid_identity_token")
	}
	return oidc.Claims{}, false
}

// signInKey is the rate-limit "login" of a sign-in attempt: per provider
// and client address, so failures neither lock an account's password
// login nor stop sign-ins from other addresses.
func signInKey(provider, ipKey string) string { return "sign-in:" + provider + ":" + ipKey }

func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var req struct {
		Provider   string `json:"provider"`
		IDToken    string `json:"id_token"`
		Nonce      string `json:"nonce"`
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := session.ValidateDevice(req.DeviceName, req.Platform); err != nil {
		errorReply(w, 400, "invalid_device")
		return
	}
	c, sc := client(r), rateScope(r)
	key := signInKey(req.Provider, rateKey(c.IP))
	if ok, wait := s.auth.Limiter.AllowIn(sc, rateKey(c.IP), key); !ok {
		rateLimited(w, wait)
		return
	}
	claims, ok := s.verifyIdentity(w, r, req.Provider, req.IDToken, req.Nonce, 401)
	if !ok {
		s.auth.Limiter.FailureIn(sc, rateKey(c.IP), key)
		return
	}
	t, id, err := s.auth.Sessions.SignInLinked(r.Context(), string(claims.Provider), claims.Subject,
		session.LoginRequest{DeviceName: req.DeviceName, Platform: req.Platform, Client: c})
	switch {
	case err == nil:
		s.auth.Limiter.Success(rateKey(c.IP), key)
		s.tokenReply(w, r, t, id)
	case errors.Is(err, session.ErrNotLinked):
		s.auth.Limiter.FailureIn(sc, rateKey(c.IP), key)
		errorReply(w, 401, "identity_not_linked")
	case errors.Is(err, session.ErrInvalidCredentials):
		errorReply(w, 401, "invalid_credentials")
	default:
		internalError(w, "sign-in", err)
	}
}

type signInJSON struct {
	Provider   string  `json:"provider"`
	Email      *string `json:"email"`
	LinkedAt   string  `json:"linked_at"`
	LastUsedAt *string `json:"last_used_at"`
}

func signInLink(l identity.Link) signInJSON {
	j := signInJSON{Provider: l.Provider, LinkedAt: ts(l.LinkedAt)}
	if l.Email != "" {
		j.Email = &l.Email
	}
	if !l.LastUsedAt.IsZero() {
		u := ts(l.LastUsedAt)
		j.LastUsedAt = &u
	}
	return j
}

func (s *Server) listSignIns(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	links, err := s.auth.Identities.List(r.Context(), id.UserID)
	if err != nil {
		internalError(w, "list sign-ins", err)
		return
	}
	items := []signInJSON{}
	for _, l := range links {
		items = append(items, signInLink(l))
	}
	jsonReply(w, 200, map[string]any{"items": items, "providers": providersJSON(s.auth.SignIn)})
}

// linkSignIn links the identity of a fresh ID token to the caller's
// account. The current password is required: a session alone (a stolen
// one, an unlocked phone) must not be able to add a lasting way in.
func (s *Server) linkSignIn(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	var req struct {
		Provider        string `json:"provider"`
		IDToken         string `json:"id_token"`
		Nonce           string `json:"nonce"`
		CurrentPassword string `json:"current_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CurrentPassword == "" {
		errorReply(w, 400, "invalid_request")
		return
	}
	c, sc := client(r), rateScope(r)
	if ok, wait := s.auth.Limiter.AllowIn(sc, rateKey(c.IP), id.LoginName); !ok {
		rateLimited(w, wait)
		return
	}
	err := s.auth.Sessions.VerifyPassword(r.Context(), id.UserID, req.CurrentPassword)
	switch {
	case errors.Is(err, session.ErrInvalidCredentials):
		s.auth.Limiter.FailureIn(sc, rateKey(c.IP), id.LoginName)
		errorReply(w, 403, "invalid_credentials")
		return
	case errors.Is(err, password.ErrBusy):
		busy(w)
		return
	case err != nil:
		internalError(w, "verify password", err)
		return
	}
	s.auth.Limiter.Success(rateKey(c.IP), id.LoginName)
	claims, ok := s.verifyIdentity(w, r, req.Provider, req.IDToken, req.Nonce, 400)
	if !ok {
		return
	}
	d := identity.Details(c.Audit())
	l, err := s.auth.Identities.Add(r.Context(), id.UserID, string(claims.Provider), claims.Subject, claims.Email, d)
	switch {
	case err == nil:
		jsonReply(w, 201, signInLink(l))
	case errors.Is(err, identity.ErrInUse):
		errorReply(w, 409, "identity_in_use")
	case errors.Is(err, identity.ErrProviderLinked):
		errorReply(w, 409, "provider_linked")
	default:
		internalError(w, "link sign-in", err)
	}
}

func (s *Server) unlinkSignIn(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	p := oidc.Provider(r.PathValue("provider"))
	if !p.Valid() {
		errorReply(w, 404, "not_found")
		return
	}
	err := s.auth.Identities.Remove(r.Context(), id.UserID, id.UserID, string(p), identity.Details(client(r).Audit()))
	switch {
	case err == nil:
		w.WriteHeader(204)
	case errors.Is(err, identity.ErrNotFound):
		errorReply(w, 404, "not_found")
	default:
		internalError(w, "unlink sign-in", err)
	}
}

// adminListSignIns shows which providers an account has linked, for
// support ("why can't I sign in with Google?"). Same fields as the
// account's own list; never the provider's account identifier.
func (s *Server) adminListSignIns(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	if _, err := s.auth.Users.Get(r.Context(), userID); err != nil {
		s.usersError(w, "get user", err)
		return
	}
	links, err := s.auth.Identities.List(r.Context(), userID)
	if err != nil {
		internalError(w, "list sign-ins", err)
		return
	}
	items := []signInJSON{}
	for _, l := range links {
		items = append(items, signInLink(l))
	}
	jsonReply(w, 200, map[string]any{"items": items, "providers": providersJSON(s.auth.SignIn)})
}

// adminUnlinkSignIn removes a link of another account, for a lost or
// taken-over Google or Apple account. It cannot add one: linking stays
// with the account holder (their password and their own ID token).
func (s *Server) adminUnlinkSignIn(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	if _, err := s.auth.Users.Get(r.Context(), userID); err != nil {
		s.usersError(w, "get user", err)
		return
	}
	p := oidc.Provider(r.PathValue("provider"))
	if !p.Valid() {
		errorReply(w, 404, "not_found")
		return
	}
	id, _ := IdentityFrom(r.Context())
	err := s.auth.Identities.Remove(r.Context(), id.UserID, userID, string(p), identity.Details(client(r).Audit()))
	switch {
	case err == nil:
		w.WriteHeader(204)
	case errors.Is(err, identity.ErrNotFound):
		errorReply(w, 404, "not_found")
	default:
		internalError(w, "unlink sign-in", err)
	}
}
