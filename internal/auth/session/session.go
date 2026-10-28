// Package session issues and checks login sessions bound to one device.
//
// A successful login creates a devices row and an auth_sessions row with two
// opaque tokens of 256 random bits each:
//
//   - access token, valid AccessTTL (15 minutes), sent on every request;
//   - refresh token, valid RefreshTTL (30 days), exchanged at Refresh for a
//     new pair. The refresh window slides: each exchange starts a new one,
//     but never past AbsoluteTTL (90 days) after the login; then the device
//     has to log in again.
//
// Refresh rotates both tokens; the previous ones stop working at once. A
// refresh token that was already exchanged and is presented again was copied
// by someone: the whole session is revoked (reuse detection), so the thief
// and the legitimate device both have to log in again. One exception covers
// a response lost on a mobile network: an app (not browser) refresh token
// presented again within RefreshGrace (30 seconds) of its exchange, while no
// other exchange happened since, rotates the session once more instead; the
// tokens issued by the first exchange are dropped and count as used.
//
// Browser sessions (platform "web") additionally get a CSRF token; the HTTP
// layer requires it on every state-changing cookie request.
//
// Only SHA-256 hashes of the tokens are stored. A token cannot be recovered
// from the database, and lookups by hash do not leak the token through
// comparison timing. Tokens are never logged or written to the audit log.
//
// Every Authenticate re-reads the account, so disabling it, demoting it or
// revoking the device takes effect on the next request. Resetting a password
// or disabling an account also revokes its sessions (RevokeUserTx, called by
// internal/users in the same transaction).
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aaaicu/withalbum/internal/audit"
	"github.com/aaaicu/withalbum/internal/auth/password"
)

// Default lifetimes (roadmap D10).
const (
	DefaultAccessTTL  = 15 * time.Minute
	DefaultRefreshTTL = 30 * 24 * time.Hour
	// DefaultAbsoluteTTL ends every session this long after its login,
	// however often it is refreshed.
	DefaultAbsoluteTTL = 90 * 24 * time.Hour
	// DefaultRefreshGrace is how long an exchanged app refresh token may be
	// presented again (lost response) without revoking the session.
	DefaultRefreshGrace = 30 * time.Second
	// SessionRetention is how long revoked or expired session rows are kept
	// (for the device history) before Prune deletes them.
	SessionRetention = 30 * 24 * time.Hour
)

// Platforms accepted for a device (devices.platform).
var Platforms = []string{"ios", "android", "macos", "web"}

// PlatformWeb is the browser platform: cookie transport and CSRF token.
const PlatformWeb = "web"

// Revocation reasons (auth_sessions.revoke_reason).
const (
	ReasonLogout         = "logout"
	ReasonDeviceRevoked  = "device_revoked"
	ReasonRefreshReuse   = "refresh_reuse"
	ReasonPasswordChange = "password_change"
	ReasonPasswordReset  = "password_reset"
	ReasonAccountDisable = "account_disabled"
	ReasonRestore        = "restore"
	ReasonAdminRevoke    = "admin_revoked"
)

var (
	// ErrInvalidCredentials: unknown account, wrong password, disabled or
	// locked account. Deliberately one error: the client must not learn
	// which.
	ErrInvalidCredentials = errors.New("invalid login name or password")
	// ErrUnauthenticated: missing, malformed, unknown, expired or revoked
	// token, or the account can no longer log in.
	ErrUnauthenticated = errors.New("not authenticated")
	// ErrRefreshReuse: a refresh token was presented a second time; the
	// session has been revoked. Treat like ErrUnauthenticated towards the
	// client.
	ErrRefreshReuse = errors.New("refresh token reused; session revoked")
	// ErrCSRF: the CSRF token of a browser session is missing or wrong.
	ErrCSRF = errors.New("missing or invalid CSRF token")
	// ErrDeviceNotFound: no active device with this id belongs to the user.
	ErrDeviceNotFound = errors.New("device not found")
	// ErrInvalidDevice: device name or platform not acceptable.
	ErrInvalidDevice = errors.New("device_name must be 1-64 characters without control characters and platform one of ios, android, macos, web")
)

// Config configures a Service. Zero values take the defaults.
type Config struct {
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
	AbsoluteTTL time.Duration
	// RefreshGrace: zero takes the default, a negative value disables it.
	RefreshGrace time.Duration
	Now          func() time.Time // tests; nil means time.Now
}

// Service manages sessions in a migrated database.
type Service struct {
	db          *sql.DB
	pw          *password.Limiter
	accessTTL   time.Duration
	refreshTTL  time.Duration
	absoluteTTL time.Duration
	grace       time.Duration
	now         func() time.Time
}

// New returns a Service. pw verifies passwords at login (bounded
// concurrency, constant-cost for unknown accounts).
func New(db *sql.DB, pw *password.Limiter, cfg Config) *Service {
	s := &Service{db: db, pw: pw, accessTTL: cfg.AccessTTL, refreshTTL: cfg.RefreshTTL, absoluteTTL: cfg.AbsoluteTTL,
		grace: cfg.RefreshGrace, now: cfg.Now}
	if s.accessTTL <= 0 {
		s.accessTTL = DefaultAccessTTL
	}
	if s.refreshTTL <= 0 {
		s.refreshTTL = DefaultRefreshTTL
	}
	if s.absoluteTTL <= 0 {
		s.absoluteTTL = DefaultAbsoluteTTL
	}
	if s.grace == 0 {
		s.grace = DefaultRefreshGrace
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// timeLayout has a fixed width, so stored times also compare correctly as
// text in SQL.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// tokenBytes is the entropy of every token.
const tokenBytes = 32

// maxTokenLen bounds a presented token before it is hashed.
const maxTokenLen = 128

func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashToken returns the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// wellFormed reports whether a presented token can be one of ours.
func wellFormed(token string) bool {
	if token == "" || len(token) > maxTokenLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == tokenBytes
}

// hashEqual compares two stored hashes in constant time.
func hashEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ValidateDevice checks a device name and platform from a login request.
func ValidateDevice(name, platform string) error {
	ok := false
	for _, p := range Platforms {
		ok = ok || p == platform
	}
	if !ok || name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 64 || strings.TrimSpace(name) != name {
		return ErrInvalidDevice
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return ErrInvalidDevice
		}
	}
	return nil
}

// Tokens are the credentials of a session. They exist in memory only: the
// caller sends them to the client once.
type Tokens struct {
	SessionID      string
	DeviceID       string
	AccessToken    string
	AccessExpires  time.Time
	RefreshToken   string
	RefreshExpires time.Time
	// SessionExpires is the absolute end of the session (login +
	// DefaultAbsoluteTTL): the same on every refresh of the session.
	SessionExpires time.Time
	CSRFToken      string // browser sessions only
}

// Identity is an authenticated request: the session and the account as
// currently stored.
type Identity struct {
	SessionID       string
	DeviceID        string
	Platform        string
	UserID          string
	LoginName       string
	DisplayName     string
	ServerRole      string
	CanCreateAlbums bool
	csrfHash        string
}

// IsBrowser reports whether the session belongs to a browser (CSRF applies).
func (id Identity) IsBrowser() bool { return id.csrfHash != "" }

// CheckCSRF reports whether token is the CSRF token of this session.
func (id Identity) CheckCSRF(token string) bool {
	return id.csrfHash != "" && wellFormed(token) && hashEqual(HashToken(token), id.csrfHash)
}

// Client describes where a request comes from, for the audit log.
type Client struct {
	IP string
	// Route is "relay" or "funnel" for a request from outside the home
	// network, empty otherwise.
	Route string
}

// details adds where the request came from to audit details d.
func (c Client) details(d map[string]any) map[string]any {
	d["ip"], d["via"] = c.IP, "http"
	if c.Route != "" {
		d["route"] = c.Route
	}
	return d
}

// LoginRequest is a login attempt.
type LoginRequest struct {
	LoginName  string
	Password   string
	DeviceName string
	Platform   string
	Client     Client
}

// FailureReason is why a login failed, for the audit log only.
type FailureReason string

const (
	FailUnknownAccount FailureReason = "unknown_account"
	FailWrongPassword  FailureReason = "wrong_password"
	FailDisabled       FailureReason = "account_disabled"
	FailLocked         FailureReason = "password_locked"
)

// auditLoginLen bounds the typed login name kept in a failure entry.
const auditLoginLen = 64

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// Login checks the credentials and, when they are valid and the account is
// active, creates a device and its session. Any failure is
// ErrInvalidCredentials (after the same password work as a success) and is
// recorded in the audit log; password.ErrBusy means no hashing slot was
// free before ctx ended.
func (s *Service) Login(ctx context.Context, req LoginRequest) (Tokens, Identity, error) {
	if err := ValidateDevice(req.DeviceName, req.Platform); err != nil {
		return Tokens{}, Identity{}, err
	}
	var userID, hash, status string
	err := s.db.QueryRowContext(ctx, "SELECT user_id, password_hash, status FROM users WHERE login_name = ?", req.LoginName).
		Scan(&userID, &hash, &status)
	if errors.Is(err, sql.ErrNoRows) {
		userID, hash = "", ""
	} else if err != nil {
		return Tokens{}, Identity{}, err
	}
	// Always one Argon2id verification, also for unknown and locked
	// accounts (the limiter then uses its dummy hash).
	ok, err := s.pw.Verify(ctx, req.Password, hash)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	var reason FailureReason
	switch {
	case userID == "":
		reason = FailUnknownAccount
	case password.IsLocked(hash):
		reason = FailLocked
	case !ok:
		reason = FailWrongPassword
	case status != "active":
		reason = FailDisabled
	}
	if reason != "" {
		if err := s.auditFailure(ctx, userID, req, reason); err != nil {
			return Tokens{}, Identity{}, err
		}
		return Tokens{}, Identity{}, ErrInvalidCredentials
	}
	tok, err := s.create(ctx, userID, req)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	if password.NeedsRehash(hash, s.pw.Params()) {
		// Best effort: the login has succeeded either way.
		if err := s.rehash(ctx, userID, hash, req.Password); err != nil {
			log.Printf("password rehash after login failed: %v", err)
		}
	}
	id, err := s.Authenticate(ctx, tok.AccessToken)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	return tok, id, nil
}

// rehash replaces a verified password's hash made with older parameters by
// one with the current parameters, unless the hash changed meanwhile.
func (s *Service) rehash(ctx context.Context, userID, old, pw string) error {
	h, err := s.pw.Rehash(ctx, pw)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE user_id = ? AND password_hash = ?", h, userID, old)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return err // changed concurrently (reset, change): keep that one
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "account.password_rehash", TargetType: "user", TargetID: userID,
		Result: audit.Allowed, Details: map[string]any{"via": "login"}}); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditPasswordChangeFailure records a refused own password change (wrong
// current password).
func (s *Service) AuditPasswordChangeFailure(ctx context.Context, userID string, c Client) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "account.password_change", TargetType: "user", TargetID: userID,
		Result: audit.Denied, Details: c.details(map[string]any{"reason": string(FailWrongPassword)})}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) auditFailure(ctx context.Context, userID string, req LoginRequest, reason FailureReason) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "auth.login", TargetType: "user", TargetID: userID,
		Result: audit.Denied, Details: req.Client.details(map[string]any{"login": truncate(req.LoginName, auditLoginLen), "reason": string(reason),
			"platform": req.Platform})}); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditThrottled records that login attempts for login from c started to be
// refused by the rate limiter (once per block, not per refused attempt).
func (s *Service) AuditThrottled(ctx context.Context, login string, c Client) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := audit.Record(ctx, tx, audit.Entry{Action: "auth.login_throttled", TargetType: "login", Result: audit.Denied,
		Details: c.details(map[string]any{"login": truncate(login, auditLoginLen)})}); err != nil {
		return err
	}
	return tx.Commit()
}

// newCredentials makes a fresh token set (csrf only for browsers) whose
// refresh window ends no later than end (the session's absolute end).
func (s *Service) newCredentials(browser bool, end time.Time) (Tokens, error) {
	var t Tokens
	var err error
	if t.AccessToken, err = newToken(); err != nil {
		return Tokens{}, err
	}
	if t.RefreshToken, err = newToken(); err != nil {
		return Tokens{}, err
	}
	if browser {
		if t.CSRFToken, err = newToken(); err != nil {
			return Tokens{}, err
		}
	}
	now := s.now()
	t.SessionExpires = end
	t.AccessExpires = now.Add(s.accessTTL)
	t.RefreshExpires = now.Add(s.refreshTTL)
	if t.RefreshExpires.After(end) {
		t.RefreshExpires = end
	}
	if t.AccessExpires.After(t.RefreshExpires) {
		t.AccessExpires = t.RefreshExpires
	}
	return t, nil
}

func nullableHash(token string) sql.NullString {
	if token == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: HashToken(token), Valid: true}
}

func (s *Service) create(ctx context.Context, userID string, req LoginRequest) (Tokens, error) {
	// The stored creation time decides the absolute end on every refresh
	// (rotate), so the end reported now is computed from the same value.
	created, err := parseTime(fmtTime(s.now()))
	if err != nil {
		return Tokens{}, err
	}
	t, err := s.newCredentials(req.Platform == PlatformWeb, created.Add(s.absoluteTTL))
	if err != nil {
		return Tokens{}, err
	}
	if t.DeviceID, err = newID(); err != nil {
		return Tokens{}, err
	}
	if t.SessionID, err = newID(); err != nil {
		return Tokens{}, err
	}
	now := fmtTime(created)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO devices (device_id, user_id, platform, device_name, created_at) VALUES (?, ?, ?, ?, ?)",
		t.DeviceID, userID, req.Platform, req.DeviceName, now); err != nil {
		return Tokens{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions
		(session_id, device_id, refresh_token_sha256, expires_at, access_token_sha256, access_expires_at, csrf_token_sha256, created_at, refreshed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.SessionID, t.DeviceID, HashToken(t.RefreshToken), fmtTime(t.RefreshExpires), HashToken(t.AccessToken),
		fmtTime(t.AccessExpires), nullableHash(t.CSRFToken), now, now); err != nil {
		return Tokens{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: userID, Action: "auth.login", TargetType: "device", TargetID: t.DeviceID,
		Result: audit.Allowed, Details: req.Client.details(map[string]any{"platform": req.Platform})}); err != nil {
		return Tokens{}, err
	}
	return t, tx.Commit()
}

// Authenticate returns the identity of a valid access token. Every failure
// is ErrUnauthenticated.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (Identity, error) {
	if !wellFormed(accessToken) {
		return Identity{}, ErrUnauthenticated
	}
	h := HashToken(accessToken)
	var id Identity
	var stored, accessExp, refreshExp, status string
	var csrf sql.NullString
	var canCreate, locked bool
	err := s.db.QueryRowContext(ctx, `SELECT s.session_id, s.device_id, s.access_token_sha256, s.access_expires_at, s.expires_at,
		s.csrf_token_sha256, d.platform, u.user_id, u.login_name, u.display_name, u.server_role, u.can_create_albums, u.status,
		substr(u.password_hash, 1, 1) = ?
		FROM auth_sessions s JOIN devices d ON d.device_id = s.device_id JOIN users u ON u.user_id = d.user_id
		WHERE s.access_token_sha256 = ? AND s.revoked_at IS NULL AND d.revoked_at IS NULL`, password.LockedPrefix, h).
		Scan(&id.SessionID, &id.DeviceID, &stored, &accessExp, &refreshExp, &csrf, &id.Platform, &id.UserID, &id.LoginName,
			&id.DisplayName, &id.ServerRole, &canCreate, &status, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	now := s.now()
	if !hashEqual(stored, h) || status != "active" || locked || !s.unexpired(accessExp, now) || !s.unexpired(refreshExp, now) {
		return Identity{}, ErrUnauthenticated
	}
	id.CanCreateAlbums = canCreate
	id.csrfHash = csrf.String
	return id, nil
}

// unexpired reports whether the stored expiry is after now (unparsable:
// expired, fail closed).
func (s *Service) unexpired(stored string, now time.Time) bool {
	t, err := parseTime(stored)
	return err == nil && now.Before(t)
}

// RefreshRequest is a token exchange.
type RefreshRequest struct {
	RefreshToken string
	// RequireCSRF: the refresh token came from a cookie, so CSRFToken must
	// be the session's CSRF token.
	RequireCSRF bool
	CSRFToken   string
	Client      Client
}

// sessionRow is a session with its device and account, read inside the
// refresh transaction.
type sessionRow struct {
	id                                 Identity
	refreshHash, refreshExp, createdAt string
	refreshedAt, status                string
	csrf, revoked, devRevoked          sql.NullString
	locked                             bool
}

func (s *Service) sessionRow(ctx context.Context, tx *sql.Tx, where string, arg string) (sessionRow, error) {
	var r sessionRow
	err := tx.QueryRowContext(ctx, `SELECT s.session_id, s.device_id, s.refresh_token_sha256, s.expires_at, s.created_at,
		COALESCE(s.refreshed_at, ''), s.csrf_token_sha256, s.revoked_at, d.revoked_at, d.platform, u.user_id, u.login_name,
		u.display_name, u.server_role, u.can_create_albums, u.status, substr(u.password_hash, 1, 1) = ?
		FROM auth_sessions s JOIN devices d ON d.device_id = s.device_id JOIN users u ON u.user_id = d.user_id
		WHERE `+where, password.LockedPrefix, arg).
		Scan(&r.id.SessionID, &r.id.DeviceID, &r.refreshHash, &r.refreshExp, &r.createdAt, &r.refreshedAt, &r.csrf, &r.revoked,
			&r.devRevoked, &r.id.Platform, &r.id.UserID, &r.id.LoginName, &r.id.DisplayName, &r.id.ServerRole, &r.id.CanCreateAlbums,
			&r.status, &r.locked)
	return r, err
}

// usable reports whether the session may still be refreshed.
func (s *Service) usable(r sessionRow, now time.Time) bool {
	return !r.revoked.Valid && !r.devRevoked.Valid && r.status == "active" && !r.locked && s.unexpired(r.refreshExp, now)
}

// Refresh exchanges a refresh token for a new token set. Errors:
// ErrUnauthenticated (unknown, expired, revoked, past the absolute end,
// account unusable), ErrRefreshReuse (already exchanged and not within the
// grace window: the session is now revoked) and ErrCSRF (nothing changed).
func (s *Service) Refresh(ctx context.Context, req RefreshRequest) (Tokens, Identity, error) {
	if !wellFormed(req.RefreshToken) {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	h := HashToken(req.RefreshToken)
	// Refresh is reachable without a session from anywhere the server is
	// (also the relay and the funnel) and is not rate limited. A token this
	// server never issued must not take the database write lock (every
	// transaction here is BEGIN IMMEDIATE), or a flood of random tokens
	// would hold up uploads and logins: look it up read-only first. A token
	// is only ever moved from auth_sessions to auth_refresh_used, so one
	// found in neither cannot appear in the transaction below.
	var known bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_sessions WHERE refresh_token_sha256 = ?1)
		OR EXISTS(SELECT 1 FROM auth_refresh_used WHERE token_sha256 = ?1)`, h).Scan(&known); err != nil {
		return Tokens{}, Identity{}, err
	}
	if !known {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	defer tx.Rollback()
	now := s.now()
	row, err := s.sessionRow(ctx, tx, "s.refresh_token_sha256 = ?", h)
	if errors.Is(err, sql.ErrNoRows) {
		return s.refreshUsed(ctx, tx, h, now, req)
	}
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	if !hashEqual(row.refreshHash, h) || !s.usable(row, now) {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	// The presented token may be presented once more within the grace
	// window (apps only: a browser gets new cookies with the response).
	return s.rotate(ctx, tx, row, now, req, h, !req.RequireCSRF && s.grace > 0)
}

// rotate replaces the session's tokens. consumed is the refresh token hash
// that stops being current (remembered in auth_refresh_used until it would
// have expired; grace allows one more exchange of it within the window).
func (s *Service) rotate(ctx context.Context, tx *sql.Tx, row sessionRow, now time.Time, req RefreshRequest, consumed string, grace bool) (Tokens, Identity, error) {
	id := row.id
	browser := row.csrf.Valid
	if req.RequireCSRF && !(browser && wellFormed(req.CSRFToken) && hashEqual(HashToken(req.CSRFToken), row.csrf.String)) {
		return Tokens{}, Identity{}, ErrCSRF
	}
	created, err := parseTime(row.createdAt)
	if err != nil {
		return Tokens{}, Identity{}, ErrUnauthenticated // fail closed
	}
	end := created.Add(s.absoluteTTL)
	if !now.Before(end) {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	t, err := s.newCredentials(browser, end)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	t.SessionID, t.DeviceID = id.SessionID, id.DeviceID
	sessionID := id.SessionID
	// Remember the exchanged token until it would have expired anyway, and
	// drop entries of this session that are past that point.
	if _, err := tx.ExecContext(ctx, "DELETE FROM auth_refresh_used WHERE session_id = ? AND expires_at <= ?", sessionID, fmtTime(now)); err != nil {
		return Tokens{}, Identity{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO auth_refresh_used (token_sha256, session_id, expires_at, used_at, grace) VALUES (?, ?, ?, ?, ?)",
		consumed, sessionID, row.refreshExp, fmtTime(now), grace); err != nil {
		return Tokens{}, Identity{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET refresh_token_sha256 = ?, expires_at = ?, access_token_sha256 = ?,
		access_expires_at = ?, csrf_token_sha256 = ?, refreshed_at = ? WHERE session_id = ? AND refresh_token_sha256 = ? AND revoked_at IS NULL`,
		HashToken(t.RefreshToken), fmtTime(t.RefreshExpires), HashToken(t.AccessToken), fmtTime(t.AccessExpires),
		nullableHash(t.CSRFToken), fmtTime(now), sessionID, row.refreshHash)
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Tokens{}, Identity{}, err
	} else if n != 1 {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	if err := tx.Commit(); err != nil {
		return Tokens{}, Identity{}, err
	}
	id.csrfHash = nullableHash(t.CSRFToken).String
	return t, id, nil
}

// refreshUsed handles a refresh token that is not current. A token
// exchanged less than the grace window ago, by the latest exchange of a
// session that is still usable, rotates the session again (the response of
// the first exchange was lost); the tokens of that first exchange stop
// working and count as used, and the grace is spent. Any other exchanged
// token revokes the session (ErrRefreshReuse); an unknown one is
// ErrUnauthenticated.
func (s *Service) refreshUsed(ctx context.Context, tx *sql.Tx, h string, now time.Time, req RefreshRequest) (Tokens, Identity, error) {
	var sessionID, usedAt string
	var grace bool
	err := tx.QueryRowContext(ctx, "SELECT session_id, COALESCE(used_at, ''), grace FROM auth_refresh_used WHERE token_sha256 = ?", h).
		Scan(&sessionID, &usedAt, &grace)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	row, err := s.sessionRow(ctx, tx, "s.session_id = ?", sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, Identity{}, err
	}
	if row.revoked.Valid {
		return Tokens{}, Identity{}, ErrUnauthenticated // already revoked (e.g. reuse seen before)
	}
	if used, err := parseTime(usedAt); err == nil && grace && !req.RequireCSRF && s.grace > 0 &&
		now.Sub(used) <= s.grace && !now.Before(used) && row.refreshedAt == usedAt && s.usable(row, now) {
		if _, err := tx.ExecContext(ctx, "UPDATE auth_refresh_used SET grace = 0 WHERE token_sha256 = ?", h); err != nil {
			return Tokens{}, Identity{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: row.id.UserID, Action: "auth.refresh_grace", TargetType: "device",
			TargetID: row.id.DeviceID, Result: audit.Allowed, Details: req.Client.details(map[string]any{"session": sessionID})}); err != nil {
			return Tokens{}, Identity{}, err
		}
		// The current token (issued by the first exchange) is dropped
		// without grace: presenting it later is reuse.
		return s.rotate(ctx, tx, row, now, req, row.refreshHash, false)
	}
	if err := revokeSessionTx(ctx, tx, sessionID, ReasonRefreshReuse, now); err != nil {
		return Tokens{}, Identity{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: row.id.UserID, Action: "auth.refresh_reuse", TargetType: "device", TargetID: row.id.DeviceID,
		Result: audit.Denied, Details: req.Client.details(map[string]any{"session": sessionID})}); err != nil {
		return Tokens{}, Identity{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tokens{}, Identity{}, err
	}
	return Tokens{}, Identity{}, ErrRefreshReuse
}

func revokeSessionTx(ctx context.Context, tx *sql.Tx, sessionID, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at = ?, revoke_reason = ? WHERE session_id = ? AND revoked_at IS NULL",
		fmtTime(now), reason, sessionID)
	return err
}

// revokeDeviceTx revokes the device and all its sessions.
func revokeDeviceTx(ctx context.Context, tx *sql.Tx, deviceID, reason string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at = ?, revoke_reason = ? WHERE device_id = ? AND revoked_at IS NULL",
		fmtTime(now), reason, deviceID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE devices SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL", fmtTime(now), deviceID)
	return err
}

// Logout revokes the device of the authenticated session (and so the
// session).
func (s *Service) Logout(ctx context.Context, id Identity, c Client) error {
	return s.revokeDevice(ctx, id.UserID, id.UserID, id.DeviceID, ReasonLogout, "auth.logout", c)
}

// RevokeDevice revokes one of the user's own devices. ErrDeviceNotFound when
// it does not belong to userID or is already revoked.
func (s *Service) RevokeDevice(ctx context.Context, userID, deviceID string, c Client) error {
	return s.revokeDevice(ctx, userID, userID, deviceID, ReasonDeviceRevoked, "device.revoke", c)
}

// RevokeOtherDevices signs out every device of id's account except the
// one of id's session ("log out my other devices", e.g. after losing a
// phone), in one transaction with one audit entry. It returns how many
// devices were revoked (0 is not an error).
func (s *Service) RevokeOtherDevices(ctx context.Context, id Identity, c Client) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT device_id FROM devices WHERE user_id = ? AND device_id != ? AND revoked_at IS NULL ORDER BY device_id",
		id.UserID, id.DeviceID)
	if err != nil {
		return 0, err
	}
	var devices []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return 0, err
		}
		devices = append(devices, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := s.now()
	for _, d := range devices {
		if err := revokeDeviceTx(ctx, tx, d, ReasonDeviceRevoked, now); err != nil {
			return 0, err
		}
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: id.UserID, Action: "device.revoke_others", TargetType: "user", TargetID: id.UserID,
		Result: audit.Allowed, Details: c.details(map[string]any{"kept_device": id.DeviceID, "revoked": len(devices)})}); err != nil {
		return 0, err
	}
	return len(devices), tx.Commit()
}

// RevokeDeviceAsAdmin revokes a device of userID on behalf of the server
// administrator actorID (the route checks the role). ErrDeviceNotFound as
// for RevokeDevice.
func (s *Service) RevokeDeviceAsAdmin(ctx context.Context, actorID, userID, deviceID string, c Client) error {
	return s.revokeDevice(ctx, actorID, userID, deviceID, ReasonAdminRevoke, "device.admin_revoke", c)
}

func (s *Service) revokeDevice(ctx context.Context, actorID, userID, deviceID, reason, action string, c Client) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM devices WHERE device_id = ? AND user_id = ? AND revoked_at IS NULL",
		deviceID, userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrDeviceNotFound
	}
	if err := revokeDeviceTx(ctx, tx, deviceID, reason, s.now()); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Entry{ActorUserID: actorID, Action: action, TargetType: "device", TargetID: deviceID,
		Result: audit.Allowed, Details: c.details(map[string]any{"user_id": userID})}); err != nil {
		return err
	}
	return tx.Commit()
}

// Device is a logged-in device as listed to its owner.
type Device struct {
	DeviceID   string
	DeviceName string
	Platform   string
	CreatedAt  time.Time
	LastUsedAt time.Time // last login or token refresh
	Expires    time.Time // end of the current refresh window
}

// ListDevices returns the user's devices with a live session, newest first.
func (s *Service) ListDevices(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.device_id, d.device_name, d.platform, d.created_at, s.refreshed_at, s.expires_at
		FROM devices d JOIN auth_sessions s ON s.device_id = d.device_id
		WHERE d.user_id = ? AND d.revoked_at IS NULL AND s.revoked_at IS NULL AND s.expires_at > ?
		ORDER BY d.created_at DESC, d.device_id`, userID, fmtTime(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		var d Device
		var created, used, exp string
		if err := rows.Scan(&d.DeviceID, &d.DeviceName, &d.Platform, &created, &used, &exp); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = parseTime(created)
		d.LastUsedAt, _ = parseTime(used)
		d.Expires, _ = parseTime(exp)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// VerifyPassword checks the current password of an account (for a password
// change) with the same bounded, constant-cost verification as Login.
// Returns ErrInvalidCredentials when it does not match.
func (s *Service) VerifyPassword(ctx context.Context, userID, pw string) error {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE user_id = ?", userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		hash = ""
	} else if err != nil {
		return err
	}
	ok, err := s.pw.Verify(ctx, pw, hash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}
	return nil
}

// RevokeUserTx revokes every session and device of userID except the
// session exceptSessionID ("" for none), inside the caller's transaction.
// internal/users calls it when a password is reset or changed and when an
// account is disabled. It returns the number of sessions revoked.
func RevokeUserTx(ctx context.Context, tx *sql.Tx, userID, exceptSessionID, reason string) (int64, error) {
	now := fmtTime(time.Now())
	res, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = ?, revoke_reason = ?
		WHERE revoked_at IS NULL AND session_id != ? AND device_id IN (SELECT device_id FROM devices WHERE user_id = ?)`,
		now, reason, exceptSessionID, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE revoked_at IS NULL AND user_id = ?
		AND device_id NOT IN (SELECT device_id FROM auth_sessions WHERE session_id = ?)`, now, userID, exceptSessionID)
	return n, err
}

// RevokeAllTx revokes every session and device of every account inside the
// caller's transaction and returns the number of sessions revoked. The
// restore command (internal/backup) calls it on the restored database: a
// backup may hold sessions that were revoked after it was taken (a lost
// phone), and they must not come back to life.
func RevokeAllTx(ctx context.Context, tx *sql.Tx, reason string) (int64, error) {
	now := fmtTime(time.Now())
	res, err := tx.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at = ?, revoke_reason = ? WHERE revoked_at IS NULL", now, reason)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE devices SET revoked_at = ? WHERE revoked_at IS NULL", now)
	return n, err
}

// PruneReport is what Prune deleted.
type PruneReport struct {
	Sessions    int64 // auth_sessions rows revoked or expired more than SessionRetention ago
	UsedRefresh int64 // auth_refresh_used rows past their expiry
}

// Prune deletes session rows that were revoked or expired more than
// SessionRetention ago (with their used refresh tokens) and used refresh
// tokens past their expiry, which would be refused as expired anyway.
// Devices are kept: they name the uploader of earlier uploads.
func (s *Service) Prune(ctx context.Context) (PruneReport, error) {
	now := s.now()
	cutoff := fmtTime(now.Add(-SessionRetention))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PruneReport{}, err
	}
	defer tx.Rollback()
	var r PruneReport
	const old = "(revoked_at IS NOT NULL AND revoked_at < ?) OR expires_at < ?"
	if _, err := tx.ExecContext(ctx, "DELETE FROM auth_refresh_used WHERE session_id IN (SELECT session_id FROM auth_sessions WHERE "+old+")",
		cutoff, cutoff); err != nil {
		return PruneReport{}, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM auth_sessions WHERE "+old, cutoff, cutoff)
	if err != nil {
		return PruneReport{}, err
	}
	if r.Sessions, err = res.RowsAffected(); err != nil {
		return PruneReport{}, err
	}
	if res, err = tx.ExecContext(ctx, "DELETE FROM auth_refresh_used WHERE expires_at <= ?", fmtTime(now)); err != nil {
		return PruneReport{}, err
	}
	if r.UsedRefresh, err = res.RowsAffected(); err != nil {
		return PruneReport{}, err
	}
	return r, tx.Commit()
}
