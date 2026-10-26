package session_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/users"
)

// testParams keeps tests fast. Production uses password.DefaultParams.
var testParams = password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const adminPW = "correct horse battery"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	db    *sql.DB
	users *users.Store
	svc   *session.Service
	clk   *clock
	admin users.User
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "withalbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitestore.EnsureBootstrapOwnerTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	us := users.New(sqlDB)
	h, err := password.Hash(adminPW, testParams)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := us.ClaimBootstrap(ctx, users.Claim{LoginName: "alice", DisplayName: "Alice", PasswordHash: h}, users.Console)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := password.NewLimiter(2, time.Second, testParams)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	return env{db: sqlDB, users: us, svc: session.New(sqlDB, pw, session.Config{Now: clk.Now}), clk: clk, admin: admin}
}

func (e env) login(t *testing.T, login, pw, platform string) (session.Tokens, error) {
	t.Helper()
	tok, _, err := e.svc.Login(context.Background(), session.LoginRequest{LoginName: login, Password: pw,
		DeviceName: "Test phone", Platform: platform, Client: session.Client{IP: "127.0.0.1"}})
	return tok, err
}

func (e env) mustLogin(t *testing.T) session.Tokens {
	t.Helper()
	tok, err := e.login(t, "alice", adminPW, "ios")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e env) auditCount(t *testing.T, action, result string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow("SELECT count(*) FROM audit_log WHERE action = ? AND result = ?", action, result).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginIssuesHashedTokens(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tok := e.mustLogin(t)
	if tok.CSRFToken != "" {
		t.Fatal("app session got a CSRF token")
	}
	if len(tok.AccessToken) != 43 || len(tok.RefreshToken) != 43 || tok.AccessToken == tok.RefreshToken {
		t.Fatalf("unexpected token shape %d/%d", len(tok.AccessToken), len(tok.RefreshToken))
	}
	if got := tok.AccessExpires.Sub(e.clk.Now()); got != 15*time.Minute {
		t.Fatalf("access ttl %v", got)
	}
	if got := tok.RefreshExpires.Sub(e.clk.Now()); got != 30*24*time.Hour {
		t.Fatalf("refresh ttl %v", got)
	}
	// Only hashes are stored: no column of any table contains a token.
	for _, table := range []string{"auth_sessions", "devices", "audit_log"} {
		rows, err := e.db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if strings.Contains(v.String, tok.AccessToken) || strings.Contains(v.String, tok.RefreshToken) || strings.Contains(v.String, adminPW) {
					t.Fatalf("%s.%s contains a secret", table, cols[i])
				}
			}
		}
		rows.Close()
	}
	var stored string
	if err := e.db.QueryRow("SELECT access_token_sha256 FROM auth_sessions").Scan(&stored); err != nil || stored != session.HashToken(tok.AccessToken) {
		t.Fatalf("stored access hash %q, %v", stored, err)
	}
	id, err := e.svc.Authenticate(ctx, tok.AccessToken)
	if err != nil || id.UserID != e.admin.ID || id.ServerRole != "SERVER_ADMIN" || id.DeviceID != tok.DeviceID || id.IsBrowser() {
		t.Fatalf("Authenticate = %+v, %v", id, err)
	}
	if _, err := e.svc.Authenticate(ctx, tok.RefreshToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("refresh token accepted as access token: %v", err)
	}
	if e.auditCount(t, "auth.login", "allowed") != 1 {
		t.Fatal("login success not audited")
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	h, _ := password.Hash("bob password long", testParams)
	bob, err := e.users.Create(ctx, users.NewUser{LoginName: "bob", DisplayName: "Bob", PasswordHash: h, Role: users.RoleUser}, users.Console)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := password.Hash("owner password long", testParams)
	if _, err := e.users.Create(ctx, users.NewUser{LoginName: "carol", DisplayName: "Carol", PasswordHash: h2, Role: users.RoleUser}, users.Console); err != nil {
		t.Fatal(err)
	}
	if err := e.users.SetStatus(ctx, bob.ID, users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"unknown account":           {"nobody", "whatever password"},
		"wrong password":            {"carol", "not the password"},
		"disabled (right password)": {"bob", "bob password long"},
		"empty password":            {"carol", ""},
		"too long":                  {"carol", strings.Repeat("x", 2000)},
	}
	for name, c := range cases {
		if _, err := e.login(t, c[0], c[1], "android"); !errors.Is(err, session.ErrInvalidCredentials) || err.Error() != session.ErrInvalidCredentials.Error() {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := e.auditCount(t, "auth.login", "denied"); n != len(cases) {
		t.Fatalf("%d denied logins audited, want %d", n, len(cases))
	}
	var details string
	if err := e.db.QueryRow("SELECT details_json FROM audit_log WHERE action = 'auth.login' AND result = 'denied' AND details_json LIKE '%unknown_account%'").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, "whatever password") {
		t.Fatal("password in audit log")
	}
	var devices int
	e.db.QueryRow("SELECT count(*) FROM devices").Scan(&devices)
	if devices != 0 {
		t.Fatalf("%d devices created by failed logins", devices)
	}
}

func TestLockedBootstrapCannotLogIn(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "withalbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	tx, _ := sqlDB.Begin()
	if _, err := sqlitestore.EnsureBootstrapOwnerTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	pw, _ := password.NewLimiter(1, time.Second, testParams)
	svc := session.New(sqlDB, pw, session.Config{})
	for _, p := range []string{"", "!locked", "locked", "admin"} {
		if _, _, err := svc.Login(ctx, session.LoginRequest{LoginName: "admin", Password: p, DeviceName: "x", Platform: "macos"}); !errors.Is(err, session.ErrInvalidCredentials) {
			t.Fatalf("password %q: %v", p, err)
		}
	}
}

func TestRefreshRotatesAndDetectsReuse(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first := e.mustLogin(t)
	e.clk.Add(time.Minute)
	second, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID || second.DeviceID != first.DeviceID ||
		second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh did not rotate within the same session")
	}
	if !second.RefreshExpires.After(first.RefreshExpires) {
		t.Fatal("refresh window did not slide")
	}
	if _, err := e.svc.Authenticate(ctx, first.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("old access token still valid: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, second.AccessToken); err != nil {
		t.Fatal(err)
	}
	// Replaying the exchanged refresh token after the grace window revokes
	// the whole session.
	e.clk.Add(session.DefaultRefreshGrace + time.Second)
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); !errors.Is(err, session.ErrRefreshReuse) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, second.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("session survived reuse: %v", err)
	}
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: second.RefreshToken}); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("current refresh token survived reuse: %v", err)
	}
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("second replay: %v", err)
	}
	if e.auditCount(t, "auth.refresh_reuse", "denied") != 1 {
		t.Fatal("reuse not audited once")
	}
	var reason string
	e.db.QueryRow("SELECT revoke_reason FROM auth_sessions WHERE session_id = ?", first.SessionID).Scan(&reason)
	if reason != session.ReasonRefreshReuse {
		t.Fatalf("revoke reason %q", reason)
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 43), strings.Repeat("a", 500)} {
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: bad}); !errors.Is(err, session.ErrUnauthenticated) {
			t.Fatalf("token %q: %v", bad, err)
		}
	}
}

func TestConcurrentRefreshOneWins(t *testing.T) {
	e := setup(t)
	tok := e.mustLogin(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Go(func() {
			_, _, err := e.svc.Refresh(context.Background(), session.RefreshRequest{RefreshToken: tok.RefreshToken})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, session.ErrRefreshReuse), errors.Is(err, session.ErrUnauthenticated):
			default:
				t.Errorf("refresh: %v", err)
			}
		})
	}
	wg.Wait()
	// One exchange wins and the grace allows exactly one more (as for a
	// lost response); every further replay of the same token revokes the
	// session (by design: apps must serialize refreshes).
	if ok != 2 {
		t.Fatalf("%d concurrent refreshes succeeded, want 2", ok)
	}
	var revoked sql.NullString
	e.db.QueryRow("SELECT revoke_reason FROM auth_sessions WHERE session_id = ?", tok.SessionID).Scan(&revoked)
	if revoked.String != session.ReasonRefreshReuse {
		t.Fatalf("session after concurrent replays: %q", revoked.String)
	}
}

func TestExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tok := e.mustLogin(t)
	e.clk.Add(15*time.Minute + time.Second)
	if _, err := e.svc.Authenticate(ctx, tok.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("expired access token: %v", err)
	}
	next, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: tok.RefreshToken})
	if err != nil {
		t.Fatalf("refresh after access expiry: %v", err)
	}
	e.clk.Add(30*24*time.Hour + time.Second)
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: next.RefreshToken}); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("expired refresh token: %v", err)
	}
	if list, _ := e.svc.ListDevices(ctx, e.admin.ID); len(list) != 0 {
		t.Fatalf("expired session listed: %+v", list)
	}
}

func TestBrowserSessionCSRF(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tok, err := e.login(t, "alice", adminPW, "web")
	if err != nil {
		t.Fatal(err)
	}
	if tok.CSRFToken == "" {
		t.Fatal("no CSRF token for web")
	}
	id, err := e.svc.Authenticate(ctx, tok.AccessToken)
	if err != nil || !id.IsBrowser() || !id.CheckCSRF(tok.CSRFToken) || id.CheckCSRF("") || id.CheckCSRF(tok.AccessToken) {
		t.Fatalf("csrf checks wrong: %v", err)
	}
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: tok.RefreshToken, RequireCSRF: true, CSRFToken: "nope"}); !errors.Is(err, session.ErrCSRF) {
		t.Fatalf("refresh without CSRF: %v", err)
	}
	// A CSRF failure changes nothing: the refresh token still works.
	next, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: tok.RefreshToken, RequireCSRF: true, CSRFToken: tok.CSRFToken})
	if err != nil || next.CSRFToken == "" || next.CSRFToken == tok.CSRFToken {
		t.Fatalf("browser refresh: %v", err)
	}
	// An app session never satisfies a CSRF check.
	app := e.mustLogin(t)
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: app.RefreshToken, RequireCSRF: true, CSRFToken: tok.CSRFToken}); !errors.Is(err, session.ErrCSRF) {
		t.Fatalf("app session via cookie: %v", err)
	}
}

func TestDevicesLogoutAndRevocation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.mustLogin(t)
	b := e.mustLogin(t)
	list, err := e.svc.ListDevices(ctx, e.admin.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("devices %+v, %v", list, err)
	}
	h, _ := password.Hash("bob password long", testParams)
	bob, _ := e.users.Create(ctx, users.NewUser{LoginName: "bob", DisplayName: "Bob", PasswordHash: h, Role: users.RoleUser}, users.Console)
	if err := e.svc.RevokeDevice(ctx, bob.ID, a.DeviceID, session.Client{}); !errors.Is(err, session.ErrDeviceNotFound) {
		t.Fatalf("revoked another user's device: %v", err)
	}
	if err := e.svc.RevokeDevice(ctx, e.admin.ID, a.DeviceID, session.Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, a.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("revoked device still authenticates")
	}
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: a.RefreshToken}); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("revoked device still refreshes")
	}
	if err := e.svc.RevokeDevice(ctx, e.admin.ID, a.DeviceID, session.Client{}); !errors.Is(err, session.ErrDeviceNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	idB, _ := e.svc.Authenticate(ctx, b.AccessToken)
	if err := e.svc.Logout(ctx, idB, session.Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, b.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("logged-out session still authenticates")
	}
	if list, _ := e.svc.ListDevices(ctx, e.admin.ID); len(list) != 0 {
		t.Fatalf("devices after revocation: %+v", list)
	}
}

func TestAccountChangesRevokeOrApplyAtOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	h, _ := password.Hash("bob password long", testParams)
	bob, err := e.users.Create(ctx, users.NewUser{LoginName: "bob", DisplayName: "Bob", PasswordHash: h, Role: users.RoleServerAdmin}, users.Console)
	if err != nil {
		t.Fatal(err)
	}
	bobLogin := func() session.Tokens {
		tok, err := e.login(t, "bob", "bob password long", "ios")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	// Role changes apply to the next request without revocation.
	tok := bobLogin()
	if err := e.users.SetRole(ctx, bob.ID, users.RoleUser, users.Console); err != nil {
		t.Fatal(err)
	}
	if id, err := e.svc.Authenticate(ctx, tok.AccessToken); err != nil || id.ServerRole != "USER" {
		t.Fatalf("role not re-read: %+v %v", id, err)
	}
	// Disabling revokes every session.
	if err := e.users.SetStatus(ctx, bob.ID, users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, tok.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("disabled account authenticates")
	}
	if err := e.users.SetStatus(ctx, bob.ID, users.StatusActive, users.Console); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, tok.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("re-enabling resurrected a session")
	}
	// A reset revokes all sessions; changing one's own keeps the current one.
	t1, t2 := bobLogin(), bobLogin()
	h2, _ := password.Hash("bob new password", testParams)
	if err := e.users.ChangeOwnPassword(ctx, bob.ID, h2, t1.SessionID, users.Actor{UserID: bob.ID, Via: "http"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, t1.AccessToken); err != nil {
		t.Fatalf("current session lost on own password change: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, t2.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("other session kept after password change")
	}
	h3, _ := password.Hash("bob reset password", testParams)
	if err := e.users.SetPassword(ctx, bob.ID, h3, users.Console); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, t1.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatal("session kept after password reset")
	}
	if _, err := e.svc.Authenticate(ctx, e.mustLogin(t).AccessToken); err != nil {
		t.Fatal("other accounts affected")
	}
}

func TestValidateDevice(t *testing.T) {
	for _, c := range []struct {
		name, platform string
		ok             bool
	}{
		{"iPhone", "ios", true}, {"Pixel 9", "android", true}, {"Mac", "macos", true}, {"Safari", "web", true},
		{"", "ios", false}, {"x", "windows", false}, {" lead", "ios", false}, {"a\nb", "ios", false},
		{strings.Repeat("가", 64), "ios", true}, {strings.Repeat("가", 65), "ios", false},
	} {
		if err := session.ValidateDevice(c.name, c.platform); (err == nil) != c.ok {
			t.Errorf("%q/%q: %v", c.name, c.platform, err)
		}
	}
}

// TestUnknownRefreshTokenTakesNoWriteLock: refresh is reachable without a
// session from the internet (relay, funnel) and is not rate limited, so a
// token the server never issued must be refused without waiting for (or
// taking) the database write lock that uploads and logins need.
func TestUnknownRefreshTokenTakesNoWriteLock(t *testing.T) {
	e := setup(t)
	tok := e.mustLogin(t)
	ctx := context.Background()
	conn, err := e.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i * 7)
	}
	start := time.Now()
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, err = e.svc.Refresh(short, session.RefreshRequest{RefreshToken: base64.RawURLEncoding.EncodeToString(b)})
	if !errors.Is(err, session.ErrUnauthenticated) || time.Since(start) > time.Second {
		t.Fatalf("unknown token while another writer holds the lock: %v after %s", err, time.Since(start))
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: tok.RefreshToken}); err != nil {
		t.Fatalf("real token: %v", err)
	}
}
