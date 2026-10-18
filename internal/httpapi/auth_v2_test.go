package httpapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/ratelimit"
)

func (e *authEnv) auditRows(t *testing.T, action, result string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow("SELECT count(*) FROM audit_log WHERE action = ? AND result = ?", action, result).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRefreshGraceOverHTTP: a refresh response lost on the way back; the app
// retries with the same refresh token and keeps its session.
func TestRefreshGraceOverHTTP(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	_, refresh := e.mustLogin(t, "member", userPassword)
	wantStatus(t, "refresh (response lost)", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh}, nil), 200, "")
	retry := e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh}, nil)
	wantStatus(t, "retry", retry, 200, "")
	wantStatus(t, "session works", e.call(t, "GET", "/api/v1/me", nil, bearer(retry.body["access_token"].(string))), 200, "")
	wantStatus(t, "third use", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh}, nil), 401, "unauthorized")
	wantStatus(t, "session revoked", e.call(t, "GET", "/api/v1/me", nil, bearer(retry.body["access_token"].(string))), 401, "unauthorized")
}

// TestSessionExpiresAt: login and every refresh report the same absolute end
// of the session (login + 90 days), for apps and browsers.
func TestSessionExpiresAt(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	loginAt := e.clock.Now()
	r := e.login(t, "member", userPassword, "ios")
	wantStatus(t, "login", r, 200, "")
	end, ok := r.body["session_expires_at"].(string)
	if !ok {
		t.Fatalf("login reply without session_expires_at: %s", r.raw)
	}
	got, err := time.Parse(time.RFC3339, end)
	if err != nil {
		t.Fatal(err)
	}
	if want := loginAt.Add(90 * 24 * time.Hour); got.After(want) || want.Sub(got) >= time.Second {
		t.Fatalf("session_expires_at %v, want %v", got, want)
	}
	refresh := r.body["refresh_token"].(string)
	for range 3 {
		e.clock.Add(20 * 24 * time.Hour)
		next := e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh}, nil)
		wantStatus(t, "refresh", next, 200, "")
		if next.body["session_expires_at"] != end {
			t.Fatalf("session_expires_at moved on refresh: %v, want %s", next.body["session_expires_at"], end)
		}
		if re, _ := time.Parse(time.RFC3339, next.body["refresh_expires_at"].(string)); re.After(got) {
			t.Fatalf("refresh_expires_at %v after the session end %v", re, got)
		}
		refresh = next.body["refresh_token"].(string)
	}
	web := e.login(t, "member", userPassword, "web")
	wantStatus(t, "web login", web, 200, "")
	if _, ok := web.body["session_expires_at"].(string); !ok {
		t.Fatalf("web login reply without session_expires_at: %s", web.raw)
	}
}

// TestAdminDeviceRoutes: a SERVER_ADMIN lists and revokes another account's
// devices; nobody else can, and the revoked token stops working at once.
func TestAdminDeviceRoutes(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	memberTok, _ := e.mustLogin(t, "member", userPassword)
	adminTok, _ := e.mustLogin(t, "root", adminPassword)
	base := "/api/v1/admin/users/" + e.member.ID + "/devices"
	wantStatus(t, "member lists", e.call(t, "GET", base, nil, bearer(memberTok)), 403, "forbidden")
	list := e.call(t, "GET", base, nil, bearer(adminTok))
	wantStatus(t, "admin lists", list, 200, "")
	devs := list.body["devices"].([]any)
	if len(devs) != 1 {
		t.Fatalf("devices %s", list.raw)
	}
	d := devs[0].(map[string]any)
	wantKeys(t, "device", d, "device_id", "device_name", "platform", "created_at", "last_used_at", "expires_at", "current")
	if d["current"] != false {
		t.Fatalf("another account's device marked current: %v", d)
	}
	devID := d["device_id"].(string)
	wantStatus(t, "unknown user", e.call(t, "GET", "/api/v1/admin/users/nobody/devices", nil, bearer(adminTok)), 404, "user_not_found")
	wantStatus(t, "member revokes", e.call(t, "DELETE", base+"/"+devID, nil, bearer(memberTok)), 403, "forbidden")
	wantStatus(t, "device under another account", e.call(t, "DELETE", "/api/v1/admin/users/"+e.admin.ID+"/devices/"+devID, nil, bearer(adminTok)), 404, "device_not_found")
	wantStatus(t, "still works", e.call(t, "GET", "/api/v1/me", nil, bearer(memberTok)), 200, "")
	wantStatus(t, "admin revokes", e.call(t, "DELETE", base+"/"+devID, nil, bearer(adminTok)), 204, "")
	wantStatus(t, "revoked token", e.call(t, "GET", "/api/v1/me", nil, bearer(memberTok)), 401, "unauthorized")
	wantStatus(t, "revoke again", e.call(t, "DELETE", base+"/"+devID, nil, bearer(adminTok)), 404, "device_not_found")
	if e.auditRows(t, "device.admin_revoke", "allowed") != 1 {
		t.Fatal("admin revocation not audited")
	}
	var actor string
	e.db.QueryRow("SELECT actor_user_id FROM audit_log WHERE action = 'device.admin_revoke'").Scan(&actor)
	if actor != e.admin.ID {
		t.Fatalf("audit actor %q", actor)
	}
	// The member's 403s were summarized in the audit log.
	if e.auditRows(t, "access.denied", "denied") != 2 {
		t.Fatalf("access denials: %d rows", e.auditRows(t, "access.denied", "denied"))
	}
}

// TestPasswordChangeFailureAudited: a wrong current password is recorded.
func TestPasswordChangeFailureAudited(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	tok, _ := e.mustLogin(t, "member", userPassword)
	wantStatus(t, "wrong current", e.call(t, "POST", "/api/v1/me/password", map[string]string{"current_password": "nope nope nope", "new_password": "brand new password"}, bearer(tok)), 403, "invalid_credentials")
	if e.auditRows(t, "account.password_change", "denied") != 1 {
		t.Fatal("failed password change not audited")
	}
	var details string
	e.db.QueryRow("SELECT details_json FROM audit_log WHERE action = 'account.password_change' AND result = 'denied'").Scan(&details)
	if details == "" || containsAny(details, "nope", "brand new") {
		t.Fatalf("details %q", details)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
