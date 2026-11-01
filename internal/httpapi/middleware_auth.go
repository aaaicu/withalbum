package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/identity"
	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/ratelimit"
	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/users"
)

// Token transport.
//
// Apps (ios, android, macos) send "Authorization: Bearer <access token>".
// Browsers (platform "web") never see the tokens: login and refresh set them
// as cookies, and the CSRF token is returned in the body and as a readable
// cookie. The cookies are:
//
//   - withalbum_access  HttpOnly, SameSite=Strict, Path=/api/, 15 minutes
//   - withalbum_refresh HttpOnly, SameSite=Strict, Path=/api/v1/auth/, 30 days
//   - withalbum_csrf    readable by the page, SameSite=Strict, Path=/, 30 days
//
// Secure is set when the request arrived over TLS, which every non-loopback
// listener requires (server/main.go). Plain HTTP is served on loopback only,
// where browsers treat http://localhost as a secure context.
//
// A request authenticated by cookie must send the CSRF token in X-CSRF-Token
// for every method except GET, HEAD and OPTIONS (403 csrf_failed otherwise).
// When an Authorization header is present, cookies are ignored entirely, so a
// Bearer request never needs (and never uses) the CSRF token.
const (
	cookieAccess  = "withalbum_access"
	cookieRefresh = "withalbum_refresh"
	cookieCSRF    = "withalbum_csrf"
	headerCSRF    = "X-CSRF-Token"
)

// Auth holds the account and session services of the HTTP API. Build it
// with NewAuth. Without it (Config.Auth nil) no auth route is registered.
type Auth struct {
	Sessions  *session.Service
	Users     *users.Store
	Passwords *password.Limiter
	Limiter   *ratelimit.Limiter
	// SignIn verifies Google and Apple ID tokens (M54); providers without
	// a client ID are off.
	SignIn     *oidc.Verifier
	Identities *identity.Store
	nonces     *nonceStore
	db         *sql.DB // the account database, for denial audit rows
}

// AuthOptions configures NewAuth. Zero values take the defaults.
type AuthOptions struct {
	PasswordParams  password.Params // zero: password.DefaultParams
	HashConcurrency int             // concurrent Argon2id computations; 0: 2
	HashWait        time.Duration   // longest wait for a hashing slot; 0: 5 s
	Session         session.Config
	RateLimit       ratelimit.Config
	SignIn          oidc.Config // Google/Apple client IDs (M54); empty: off
}

// DefaultHashConcurrency bounds the memory used by password hashing to
// 2 x 64 MiB with the default parameters.
const DefaultHashConcurrency = 2

// NewAuth builds the services on a migrated database.
func NewAuth(db *sql.DB, o AuthOptions) (*Auth, error) {
	if o.PasswordParams == (password.Params{}) {
		o.PasswordParams = password.DefaultParams
	}
	if o.HashConcurrency <= 0 {
		o.HashConcurrency = DefaultHashConcurrency
	}
	pw, err := password.NewLimiter(o.HashConcurrency, o.HashWait, o.PasswordParams)
	if err != nil {
		return nil, err
	}
	return &Auth{Sessions: session.New(db, pw, o.Session), Users: users.New(db), Passwords: pw, Limiter: ratelimit.New(o.RateLimit),
		SignIn: oidc.New(o.SignIn), Identities: identity.New(db), nonces: newNonceStore(), db: db}, nil
}

type identityKey struct{}

// IdentityFrom returns the session identity RequireSession stored in ctx.
func IdentityFrom(ctx context.Context) (session.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(session.Identity)
	return id, ok
}

// PrincipalFrom returns the authorization principal of an authenticated
// request. It is derived from the account as stored when the request
// arrived, never from request fields.
func PrincipalFrom(ctx context.Context) (authz.Principal, bool) {
	id, ok := IdentityFrom(ctx)
	if !ok {
		return authz.Principal{}, false
	}
	return principal(id), true
}

func principal(id session.Identity) authz.Principal {
	return authz.Principal{UserID: id.UserID, Active: true, ServerRole: authz.ServerRole(id.ServerRole), CanCreateAlbums: id.CanCreateAlbums}
}

// clientIP is the TCP peer address, also on a LAN listener. Forwarding
// headers (X-Forwarded-For, Forwarded, X-Real-IP) are never trusted: there
// is no reverse proxy mode, so a client cannot choose its rate-limit key.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// client is where a request comes from, for the audit log: the peer IP and,
// for a remote route (relay, funnel), that route.
func client(r *http.Request) session.Client {
	c := session.Client{IP: clientIP(r)}
	if rt := RouteFrom(r.Context()); rt.Remote() {
		c.Route = string(rt)
	}
	return c
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="withalbum"`)
	errorReply(w, 401, "unauthorized")
}

// bearerToken returns the token of an Authorization header, and whether the
// header was present at all.
func bearerToken(r *http.Request) (token string, present bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(tok), true
}

// RequireSession authenticates the request (Bearer token, or access cookie
// plus CSRF token for unsafe methods) and stores the identity in the request
// context. The account is re-read from the database on every request, so a
// disabled account, a revoked device or an expired token is refused at once
// with 401.
func (s *Server) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		token, viaHeader := bearerToken(r)
		viaCookie := false
		if !viaHeader {
			if c, err := r.Cookie(cookieAccess); err == nil {
				token, viaCookie = c.Value, true
			}
		}
		if token == "" {
			unauthorized(w)
			return
		}
		id, err := s.auth.Sessions.Authenticate(r.Context(), token)
		if errors.Is(err, session.ErrUnauthenticated) {
			unauthorized(w)
			return
		}
		if err != nil {
			// The database lives on the storage volume: with the disk gone
			// this is the same refusal as for writes, not a server bug.
			if s.vol.Check() != nil {
				log.Printf("authenticate failed, storage unavailable: %v", err)
				errorReply(w, 503, "storage_unavailable")
				return
			}
			internalError(w, "authenticate", err)
			return
		}
		if viaCookie && !safeMethod(r.Method) && !id.CheckCSRF(r.Header.Get(headerCSRF)) {
			errorReply(w, 403, "csrf_failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// internalError logs err (never a token or password: callers pass errors
// from the database or the session layer, which contain neither) and
// replies 500.
func internalError(w http.ResponseWriter, what string, err error) {
	log.Printf("%s failed: %v", what, err)
	errorReply(w, 500, "internal_error")
}
