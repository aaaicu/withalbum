package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/session"
)

// TestRefreshGraceAfterLostResponse: the app sent a refresh, the server
// rotated, the response was lost; the app sends the same token again within
// the grace window and gets a working token set. The tokens of the lost
// response are dead and count as used; the grace works only once.
func TestRefreshGraceAfterLostResponse(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first := e.mustLogin(t)
	lost, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Add(10 * time.Second)
	retry, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
	if err != nil {
		t.Fatalf("retry within grace: %v", err)
	}
	if retry.SessionID != first.SessionID || retry.RefreshToken == lost.RefreshToken || retry.AccessToken == lost.AccessToken {
		t.Fatal("grace did not rotate the same session")
	}
	if _, err := e.svc.Authenticate(ctx, lost.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("access token of the lost response still valid: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, retry.AccessToken); err != nil {
		t.Fatal(err)
	}
	if e.auditCount(t, "auth.refresh_grace", "allowed") != 1 {
		t.Fatal("grace not audited")
	}
	// The grace is spent: a third presentation of the first token is reuse.
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); !errors.Is(err, session.ErrRefreshReuse) {
		t.Fatalf("second grace: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, retry.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("session survived reuse: %v", err)
	}
}

// TestRefreshGraceLimits: no grace for the dropped token of the lost
// response, after the window, after a later rotation, or for browsers.
func TestRefreshGraceLimits(t *testing.T) {
	ctx := context.Background()
	t.Run("lost token presented later is reuse", func(t *testing.T) {
		e := setup(t)
		first := e.mustLogin(t)
		lost, _, _ := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: lost.RefreshToken}); !errors.Is(err, session.ErrRefreshReuse) {
			t.Fatalf("dropped token: %v", err)
		}
	})
	t.Run("after the window", func(t *testing.T) {
		e := setup(t)
		first := e.mustLogin(t)
		e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
		e.clk.Add(session.DefaultRefreshGrace + time.Millisecond)
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); !errors.Is(err, session.ErrRefreshReuse) {
			t.Fatalf("after window: %v", err)
		}
	})
	t.Run("older than the latest exchange", func(t *testing.T) {
		e := setup(t)
		first := e.mustLogin(t)
		second, _, _ := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
		e.clk.Add(time.Second)
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: second.RefreshToken}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); !errors.Is(err, session.ErrRefreshReuse) {
			t.Fatalf("two exchanges back: %v", err)
		}
	})
	t.Run("browser", func(t *testing.T) {
		e := setup(t)
		first, err := e.login(t, "alice", adminPW, "web")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken, RequireCSRF: true, CSRFToken: first.CSRFToken}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken, RequireCSRF: true, CSRFToken: first.CSRFToken}); !errors.Is(err, session.ErrRefreshReuse) {
			t.Fatalf("browser replay: %v", err)
		}
	})
}

// TestRefreshAbsoluteExpiry: refreshing keeps a session alive for at most
// DefaultAbsoluteTTL after its login.
func TestRefreshAbsoluteExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	login := e.clk.Now()
	tok := e.mustLogin(t)
	end := login.Add(session.DefaultAbsoluteTTL)
	if !tok.SessionExpires.Equal(end) {
		t.Fatalf("session end at login %v, want %v", tok.SessionExpires, end)
	}
	for e.clk.Now().Add(20 * 24 * time.Hour).Before(end) {
		e.clk.Add(20 * 24 * time.Hour)
		next, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: tok.RefreshToken})
		if err != nil {
			t.Fatalf("refresh at %v: %v", e.clk.Now().Sub(login), err)
		}
		if next.RefreshExpires.After(end) {
			t.Fatalf("refresh window %v ends after the absolute end %v", next.RefreshExpires, end)
		}
		if !next.SessionExpires.Equal(end) {
			t.Fatalf("session end after a refresh %v, want the fixed absolute end %v", next.SessionExpires, end)
		}
		tok = next
	}
	if !tok.RefreshExpires.Equal(end) {
		t.Fatalf("last window ends %v, want the absolute end %v", tok.RefreshExpires, end)
	}
	e.clk.Add(end.Sub(e.clk.Now()))
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: tok.RefreshToken}); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("refresh at the absolute end: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, tok.AccessToken); !errors.Is(err, session.ErrUnauthenticated) {
		t.Fatalf("access token at the absolute end: %v", err)
	}
}

// TestLoginRehashesOldParameters: a password stored with other Argon2id
// parameters is re-hashed with the current ones at a successful login.
func TestLoginRehashesOldParameters(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	newer := testParams
	newer.Time = 2
	pw, err := password.NewLimiter(2, time.Second, newer)
	if err != nil {
		t.Fatal(err)
	}
	svc := session.New(e.db, pw, session.Config{Now: e.clk.Now})
	var before string
	e.db.QueryRow("SELECT password_hash FROM users WHERE user_id = ?", e.admin.ID).Scan(&before)
	if !password.NeedsRehash(before, newer) {
		t.Fatal("test setup: hash already current")
	}
	if _, _, err := svc.Login(ctx, session.LoginRequest{LoginName: "alice", Password: "wrong password!!", DeviceName: "x", Platform: "ios"}); err == nil {
		t.Fatal("wrong password accepted")
	}
	var after string
	e.db.QueryRow("SELECT password_hash FROM users WHERE user_id = ?", e.admin.ID).Scan(&after)
	if after != before {
		t.Fatal("failed login changed the hash")
	}
	if _, _, err := svc.Login(ctx, session.LoginRequest{LoginName: "alice", Password: adminPW, DeviceName: "x", Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	e.db.QueryRow("SELECT password_hash FROM users WHERE user_id = ?", e.admin.ID).Scan(&after)
	if after == before || password.NeedsRehash(after, newer) {
		t.Fatalf("hash not upgraded: %q", after)
	}
	if ok, err := password.Verify(adminPW, after); !ok || err != nil {
		t.Fatalf("upgraded hash does not verify: %v", err)
	}
	if e.auditCount(t, "account.password_rehash", "allowed") != 1 {
		t.Fatal("rehash not audited")
	}
	// Still current: no second rehash.
	if _, _, err := svc.Login(ctx, session.LoginRequest{LoginName: "alice", Password: adminPW, DeviceName: "x", Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	if e.auditCount(t, "account.password_rehash", "allowed") != 1 {
		t.Fatal("rehashed again")
	}
}

// TestPrune deletes only rows that are long revoked or expired.
func TestPrune(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	live := e.mustLogin(t)
	gone := e.mustLogin(t)
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: live.RefreshToken}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeDevice(ctx, e.admin.ID, gone.DeviceID, session.Client{}); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if r, err := e.svc.Prune(ctx); err != nil || r.Sessions != 0 || r.UsedRefresh != 0 {
		t.Fatalf("prune right away: %+v %v", r, err)
	}
	// 31 days later: the revoked session is past retention; the live one's
	// refresh window (30 days) has ended, but not 30 days ago.
	e.clk.Add(31 * 24 * time.Hour)
	r, err := e.svc.Prune(ctx)
	if err != nil || r.Sessions != 1 || r.UsedRefresh != 1 {
		t.Fatalf("prune after 31 days: %+v %v", r, err)
	}
	if count("SELECT count(*) FROM auth_sessions WHERE session_id = ?", gone.SessionID) != 0 ||
		count("SELECT count(*) FROM auth_sessions WHERE session_id = ?", live.SessionID) != 1 {
		t.Fatal("wrong sessions pruned")
	}
	if count("SELECT count(*) FROM devices WHERE device_id = ?", gone.DeviceID) != 1 {
		t.Fatal("device row deleted")
	}
	e.clk.Add(31 * 24 * time.Hour)
	if r, err := e.svc.Prune(ctx); err != nil || r.Sessions != 1 {
		t.Fatalf("prune expired session: %+v %v", r, err)
	}
}

// TestSessionExpiresFixedAcrossGrace: the absolute end reported by a refresh
// in the grace window (lost response) is still the one of the login.
func TestSessionExpiresFixedAcrossGrace(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	first := e.mustLogin(t)
	e.clk.Add(time.Hour)
	if _, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken}); err != nil {
		t.Fatal(err)
	}
	e.clk.Add(5 * time.Second)
	retry, _, err := e.svc.Refresh(ctx, session.RefreshRequest{RefreshToken: first.RefreshToken})
	if err != nil {
		t.Fatalf("retry within grace: %v", err)
	}
	if first.SessionExpires.IsZero() || !retry.SessionExpires.Equal(first.SessionExpires) {
		t.Fatalf("session end moved: login %v, grace refresh %v", first.SessionExpires, retry.SessionExpires)
	}
}
