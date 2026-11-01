package httpapi_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/ratelimit"
	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

var fastParams = password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const (
	adminPassword = "admin password 123"
	userPassword  = "user password 1234"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type authEnv struct {
	srv    *httptest.Server
	db     *sql.DB
	users  *users.Store
	clock  *testClock
	admin  users.User
	member users.User
}

func newAuthEnv(t *testing.T, rl ratelimit.Config) *authEnv {
	t.Helper()
	return newAuthEnvOpts(t, httpapi.AuthOptions{RateLimit: rl})
}

// newAuthEnvOpts: like newAuthEnv with more auth options (fast password
// parameters and the test clock are always set).
func newAuthEnvOpts(t *testing.T, o httpapi.AuthOptions) *authEnv {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.New(vol)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &authEnv{db: store.DB(), users: users.New(store.DB()), clock: &testClock{t: time.Now().UTC()}}
	ah, _ := password.Hash(adminPassword, fastParams)
	if e.admin, err = e.users.ClaimBootstrap(t.Context(), users.Claim{LoginName: "root", DisplayName: "Root", PasswordHash: ah}, users.Console); err != nil {
		t.Fatal(err)
	}
	uh, _ := password.Hash(userPassword, fastParams)
	if e.member, err = e.users.Create(t.Context(), users.NewUser{LoginName: "member", DisplayName: "Member", PasswordHash: uh, Role: users.RoleUser}, users.Console); err != nil {
		t.Fatal(err)
	}
	o.PasswordParams, o.Session = fastParams, session.Config{Now: e.clock.Now}
	auth, err := httpapi.NewAuth(store.DB(), o)
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(httpapi.Config{Volume: vol, Meta: store, Blobs: blobs, Version: "test", Auth: auth})
	e.srv = httptest.NewServer(api.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

// call sends a request. hdr holds extra headers; a value for "Cookie" adds
// cookies verbatim.
func (e *authEnv) call(t *testing.T, method, path string, body any, hdr map[string]string) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, header: res.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func (e *authEnv) login(t *testing.T, login, pw, platform string) resp {
	t.Helper()
	return e.call(t, "POST", "/api/v1/auth/login", map[string]string{"login_name": login, "password": pw,
		"device_name": "Test device", "platform": platform}, nil)
}

func (e *authEnv) mustLogin(t *testing.T, login, pw string) (access, refresh string) {
	t.Helper()
	r := e.login(t, login, pw, "ios")
	if r.status != 200 {
		t.Fatalf("login %s: %d %s", login, r.status, r.raw)
	}
	return r.body["access_token"].(string), r.body["refresh_token"].(string)
}

func wantStatus(t *testing.T, what string, r resp, status int, code string) {
	t.Helper()
	if r.status != status || (code != "" && r.body["error"] != code) {
		t.Fatalf("%s: got %d %s; want %d %s", what, r.status, r.raw, status, code)
	}
}

func TestLoginAndSessionLifecycle(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	r := e.login(t, "member", userPassword, "android")
	wantStatus(t, "login", r, 200, "")
	if r.body["token_type"] != "Bearer" || r.header.Get("Cache-Control") != "no-store" || r.header.Get("Set-Cookie") != "" {
		t.Fatalf("login reply: %v %v", r.body, r.header)
	}
	user := r.body["user"].(map[string]any)
	if user["user_id"] != e.member.ID || user["server_role"] != "USER" || user["can_create_albums"] != false {
		t.Fatalf("user %v", user)
	}
	if strings.Contains(r.raw, "password") || strings.Contains(r.raw, "argon2") {
		t.Fatalf("secret material in reply: %s", r.raw)
	}
	access, refresh := r.body["access_token"].(string), r.body["refresh_token"].(string)

	me := e.call(t, "GET", "/api/v1/me", nil, bearer(access))
	wantStatus(t, "me", me, 200, "")
	if me.body["device_id"] != r.body["device_id"] {
		t.Fatalf("me %v", me.body)
	}
	wantStatus(t, "me without token", e.call(t, "GET", "/api/v1/me", nil, nil), 401, "unauthorized")
	wantStatus(t, "me with garbage", e.call(t, "GET", "/api/v1/me", nil, bearer("garbage")), 401, "unauthorized")
	wantStatus(t, "me with Basic", e.call(t, "GET", "/api/v1/me", nil, map[string]string{"Authorization": "Basic " + access}), 401, "unauthorized")

	// Refresh rotates: the old refresh and access tokens stop working.
	rot := e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh}, nil)
	wantStatus(t, "refresh", rot, 200, "")
	access2, refresh2 := rot.body["access_token"].(string), rot.body["refresh_token"].(string)
	wantStatus(t, "old access after refresh", e.call(t, "GET", "/api/v1/me", nil, bearer(access)), 401, "unauthorized")
	wantStatus(t, "new access", e.call(t, "GET", "/api/v1/me", nil, bearer(access2)), 200, "")

	// Replaying the old refresh token after the grace window revokes the
	// session.
	e.clock.Add(session.DefaultRefreshGrace + time.Second)
	wantStatus(t, "refresh reuse", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh}, nil), 401, "unauthorized")
	wantStatus(t, "access after reuse", e.call(t, "GET", "/api/v1/me", nil, bearer(access2)), 401, "unauthorized")
	wantStatus(t, "refresh after reuse", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": refresh2}, nil), 401, "unauthorized")

	// Logout.
	a3, r3 := e.mustLogin(t, "member", userPassword)
	wantStatus(t, "logout", e.call(t, "POST", "/api/v1/auth/logout", nil, bearer(a3)), 204, "")
	wantStatus(t, "access after logout", e.call(t, "GET", "/api/v1/me", nil, bearer(a3)), 401, "unauthorized")
	wantStatus(t, "refresh after logout", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": r3}, nil), 401, "unauthorized")

	// Expiry.
	a4, r4 := e.mustLogin(t, "member", userPassword)
	e.clock.Add(16 * time.Minute)
	wantStatus(t, "expired access", e.call(t, "GET", "/api/v1/me", nil, bearer(a4)), 401, "unauthorized")
	rot4 := e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": r4}, nil)
	wantStatus(t, "refresh after access expiry", rot4, 200, "")
	e.clock.Add(31 * 24 * time.Hour)
	wantStatus(t, "expired refresh", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{"refresh_token": rot4.body["refresh_token"].(string)}, nil), 401, "unauthorized")
}

func TestLoginFailuresLookAlike(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{IPBurst: 1000, PairFailures: 1000, AccountFailures: 1000})
	dh, _ := password.Hash("disabled password 1", fastParams)
	d, err := e.users.Create(t.Context(), users.NewUser{LoginName: "gone", DisplayName: "Gone", PasswordHash: dh, Role: users.RoleUser}, users.Console)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.users.SetStatus(t.Context(), d.ID, users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, c := range [][2]string{{"nobody", "some password 12"}, {"member", "wrong password 12"}, {"gone", "disabled password 1"}, {"MEMBER", "wrong"}} {
		r := e.login(t, c[0], c[1], "ios")
		wantStatus(t, c[0], r, 401, "invalid_credentials")
		bodies = append(bodies, r.raw)
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("failure bodies differ: %q vs %q", b, bodies[0])
		}
	}
	// The login name is case-insensitive.
	wantStatus(t, "case-insensitive login", e.login(t, "MEMBER", userPassword, "ios"), 200, "")
	wantStatus(t, "not json", e.call(t, "POST", "/api/v1/auth/login", nil, map[string]string{"Content-Type": "text/plain"}), 415, "json_required")
	wantStatus(t, "bad platform", e.login(t, "member", userPassword, "windows"), 400, "invalid_device")
	var denied int
	e.db.QueryRow("SELECT count(*) FROM audit_log WHERE action = 'auth.login' AND result = 'denied'").Scan(&denied)
	if denied != 4 {
		t.Fatalf("%d failed logins audited, want 4", denied)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{IPBurst: 1000, PairFailures: 3, AccountFailures: 10, Window: time.Minute})
	for i := range 3 {
		wantStatus(t, "failure", e.login(t, "member", "wrong password!!", "ios"), 401, "invalid_credentials")
		_ = i
	}
	r := e.login(t, "member", userPassword, "ios")
	wantStatus(t, "throttled (even with the right password)", r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Unknown accounts are throttled the same way.
	for range 3 {
		e.login(t, "ghost", "wrong password!!", "ios")
	}
	wantStatus(t, "unknown throttled", e.login(t, "ghost", "wrong password!!", "ios"), 429, "rate_limited")
	wantStatus(t, "other account", e.login(t, "root", adminPassword, "ios"), 200, "")
	var throttled int
	e.db.QueryRow("SELECT count(*) FROM audit_log WHERE action = 'auth.login_throttled'").Scan(&throttled)
	if throttled != 2 {
		t.Fatalf("%d throttle entries, want 2 (one per block)", throttled)
	}

	ipEnv := newAuthEnv(t, ratelimit.Config{IPBurst: 2, IPInterval: time.Hour})
	ipEnv.login(t, "member", userPassword, "ios")
	ipEnv.login(t, "root", adminPassword, "ios")
	wantStatus(t, "ip burst", ipEnv.login(t, "member", userPassword, "ios"), 429, "rate_limited")
}

func TestAdminRoutes(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	adminTok, _ := e.mustLogin(t, "root", adminPassword)
	userTok, _ := e.mustLogin(t, "member", userPassword)
	for _, rt := range [][2]string{{"GET", "/api/v1/admin/users"}, {"POST", "/api/v1/admin/users"}, {"GET", "/api/v1/admin/users/" + e.admin.ID},
		{"PATCH", "/api/v1/admin/users/" + e.member.ID}, {"PATCH", "/api/v1/admin/users/" + e.member.ID + "/permissions"}} {
		wantStatus(t, "non-admin "+rt[0]+" "+rt[1], e.call(t, rt[0], rt[1], map[string]any{}, bearer(userTok)), 403, "forbidden")
		wantStatus(t, "anonymous "+rt[0]+" "+rt[1], e.call(t, rt[0], rt[1], map[string]any{}, nil), 401, "unauthorized")
	}
	list := e.call(t, "GET", "/api/v1/admin/users", nil, bearer(adminTok))
	wantStatus(t, "list", list, 200, "")
	if n := len(list.body["users"].([]any)); n != 2 || strings.Contains(list.raw, "argon2") {
		t.Fatalf("list: %s", list.raw)
	}
	cr := e.call(t, "POST", "/api/v1/admin/users", map[string]any{"login_name": "newbie", "password": "newbie password 1"}, bearer(adminTok))
	wantStatus(t, "create", cr, 201, "")
	if cr.body["server_role"] != "USER" || cr.body["can_create_albums"] != false || cr.body["status"] != "active" {
		t.Fatalf("defaults: %s", cr.raw)
	}
	newID := cr.body["user_id"].(string)
	wantStatus(t, "duplicate", e.call(t, "POST", "/api/v1/admin/users", map[string]any{"login_name": "NEWBIE", "password": "newbie password 1"}, bearer(adminTok)), 409, "login_taken")
	wantStatus(t, "weak", e.call(t, "POST", "/api/v1/admin/users", map[string]any{"login_name": "weak", "password": "short"}, bearer(adminTok)), 400, "invalid_password")
	wantStatus(t, "bad role", e.call(t, "POST", "/api/v1/admin/users", map[string]any{"login_name": "x1x", "password": "newbie password 1", "server_role": "ROOT"}, bearer(adminTok)), 400, "invalid_role")
	newTok, _ := e.mustLogin(t, "newbie", "newbie password 1")

	p := e.call(t, "PATCH", "/api/v1/admin/users/"+newID+"/permissions", map[string]any{"can_create_albums": true}, bearer(adminTok))
	wantStatus(t, "grant creation", p, 200, "")
	me := e.call(t, "GET", "/api/v1/me", nil, bearer(newTok))
	if me.body["user"].(map[string]any)["can_create_albums"] != true {
		t.Fatalf("permission not applied to the existing session: %s", me.raw)
	}
	wantStatus(t, "empty patch", e.call(t, "PATCH", "/api/v1/admin/users/"+newID+"/permissions", map[string]any{}, bearer(adminTok)), 400, "invalid_request")
	wantStatus(t, "unknown user", e.call(t, "PATCH", "/api/v1/admin/users/nope/permissions", map[string]any{"can_create_albums": true}, bearer(adminTok)), 404, "user_not_found")
	wantStatus(t, "demote last admin", e.call(t, "PATCH", "/api/v1/admin/users/"+e.admin.ID+"/permissions", map[string]any{"server_role": "USER"}, bearer(adminTok)), 409, "last_admin")
	wantStatus(t, "disable folder owner", e.call(t, "PATCH", "/api/v1/admin/users/"+e.admin.ID, map[string]any{"status": "disabled"}, bearer(adminTok)), 409, "")

	// Disabling an account cuts its existing session at once.
	wantStatus(t, "disable", e.call(t, "PATCH", "/api/v1/admin/users/"+e.member.ID, map[string]any{"status": "disabled"}, bearer(adminTok)), 200, "")
	wantStatus(t, "disabled user's token", e.call(t, "GET", "/api/v1/me", nil, bearer(userTok)), 401, "unauthorized")
	wantStatus(t, "disabled user login", e.login(t, "member", userPassword, "ios"), 401, "invalid_credentials")
	wantStatus(t, "re-enable", e.call(t, "PATCH", "/api/v1/admin/users/"+e.member.ID, map[string]any{"status": "active"}, bearer(adminTok)), 200, "")
	wantStatus(t, "old token stays revoked", e.call(t, "GET", "/api/v1/me", nil, bearer(userTok)), 401, "unauthorized")
	wantStatus(t, "bad status", e.call(t, "PATCH", "/api/v1/admin/users/"+e.member.ID, map[string]any{"status": "gone"}, bearer(adminTok)), 400, "invalid_status")

	// Demoting an administrator takes effect on the next request.
	wantStatus(t, "promote", e.call(t, "PATCH", "/api/v1/admin/users/"+newID+"/permissions", map[string]any{"server_role": "SERVER_ADMIN"}, bearer(adminTok)), 200, "")
	wantStatus(t, "new admin lists", e.call(t, "GET", "/api/v1/admin/users", nil, bearer(newTok)), 200, "")
	wantStatus(t, "demote", e.call(t, "PATCH", "/api/v1/admin/users/"+newID+"/permissions", map[string]any{"server_role": "USER"}, bearer(adminTok)), 200, "")
	wantStatus(t, "demoted admin", e.call(t, "GET", "/api/v1/admin/users", nil, bearer(newTok)), 403, "forbidden")
}

func TestDevicesAndPasswordChange(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	a1, _ := e.mustLogin(t, "member", userPassword)
	a2, _ := e.mustLogin(t, "member", userPassword)
	adminTok, _ := e.mustLogin(t, "root", adminPassword)
	list := e.call(t, "GET", "/api/v1/me/devices", nil, bearer(a1))
	wantStatus(t, "devices", list, 200, "")
	devs := list.body["devices"].([]any)
	if len(devs) != 2 {
		t.Fatalf("devices: %s", list.raw)
	}
	var otherID string
	current := 0
	for _, d := range devs {
		m := d.(map[string]any)
		if m["current"] == true {
			current++
		} else {
			otherID = m["device_id"].(string)
		}
	}
	if current != 1 || otherID == "" {
		t.Fatalf("current flags: %s", list.raw)
	}
	wantStatus(t, "revoke someone else's device", e.call(t, "DELETE", "/api/v1/me/devices/"+otherID, nil, bearer(adminTok)), 404, "device_not_found")
	wantStatus(t, "revoke own device", e.call(t, "DELETE", "/api/v1/me/devices/"+otherID, nil, bearer(a1)), 204, "")
	wantStatus(t, "revoked device token", e.call(t, "GET", "/api/v1/me", nil, bearer(a2)), 401, "unauthorized")
	wantStatus(t, "revoke again", e.call(t, "DELETE", "/api/v1/me/devices/"+otherID, nil, bearer(a1)), 404, "device_not_found")

	// Password change: wrong current password refused, other sessions revoked.
	a3, _ := e.mustLogin(t, "member", userPassword)
	wantStatus(t, "wrong current", e.call(t, "POST", "/api/v1/me/password", map[string]string{"current_password": "nope nope nope", "new_password": "brand new password"}, bearer(a1)), 403, "invalid_credentials")
	wantStatus(t, "weak new", e.call(t, "POST", "/api/v1/me/password", map[string]string{"current_password": userPassword, "new_password": "short"}, bearer(a1)), 400, "invalid_password")
	wantStatus(t, "change", e.call(t, "POST", "/api/v1/me/password", map[string]string{"current_password": userPassword, "new_password": "brand new password"}, bearer(a1)), 204, "")
	wantStatus(t, "current session kept", e.call(t, "GET", "/api/v1/me", nil, bearer(a1)), 200, "")
	wantStatus(t, "other session revoked", e.call(t, "GET", "/api/v1/me", nil, bearer(a3)), 401, "unauthorized")
	wantStatus(t, "old password", e.login(t, "member", userPassword, "ios"), 401, "invalid_credentials")
	wantStatus(t, "new password", e.login(t, "member", "brand new password", "ios"), 200, "")
	wantStatus(t, "admin unaffected", e.call(t, "GET", "/api/v1/me", nil, bearer(adminTok)), 200, "")
}

func cookieHeader(cs []*http.Cookie) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func TestBrowserCookiesAndCSRF(t *testing.T) {
	e := newAuthEnv(t, ratelimit.Config{})
	r := e.login(t, "member", userPassword, "web")
	wantStatus(t, "web login", r, 200, "")
	if r.body["token_type"] != "cookie" || r.body["access_token"] != nil || r.body["refresh_token"] != nil {
		t.Fatalf("web login exposed tokens: %s", r.raw)
	}
	csrf := r.body["csrf_token"].(string)
	cookies := (&http.Response{Header: r.header}).Cookies()
	byName := map[string]*http.Cookie{}
	for _, c := range cookies {
		byName[c.Name] = c
		if c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("cookie %s SameSite %v", c.Name, c.SameSite)
		}
	}
	if a := byName["withalbum_access"]; a == nil || !a.HttpOnly || a.Path != "/api/" {
		t.Fatalf("access cookie %+v", a)
	}
	if rf := byName["withalbum_refresh"]; rf == nil || !rf.HttpOnly || rf.Path != "/api/v1/auth/" {
		t.Fatalf("refresh cookie %+v", rf)
	}
	if c := byName["withalbum_csrf"]; c == nil || c.HttpOnly || c.Value != csrf {
		t.Fatalf("csrf cookie %+v", c)
	}
	jar := cookieHeader(cookies)
	wantStatus(t, "cookie GET", e.call(t, "GET", "/api/v1/me", nil, map[string]string{"Cookie": jar}), 200, "")
	wantStatus(t, "cookie POST without CSRF", e.call(t, "POST", "/api/v1/auth/logout", nil, map[string]string{"Cookie": jar}), 403, "csrf_failed")
	wantStatus(t, "cookie POST wrong CSRF", e.call(t, "POST", "/api/v1/auth/logout", nil, map[string]string{"Cookie": jar, "X-CSRF-Token": byName["withalbum_access"].Value}), 403, "csrf_failed")
	wantStatus(t, "cookie refresh without CSRF", e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{}, map[string]string{"Cookie": jar}), 403, "csrf_failed")

	// Refresh by cookie with the CSRF header rotates the cookies.
	rot := e.call(t, "POST", "/api/v1/auth/refresh", map[string]string{}, map[string]string{"Cookie": jar, "X-CSRF-Token": csrf})
	wantStatus(t, "cookie refresh", rot, 200, "")
	newCSRF := rot.body["csrf_token"].(string)
	jar2 := cookieHeader((&http.Response{Header: rot.header}).Cookies())
	wantStatus(t, "old cookies", e.call(t, "GET", "/api/v1/me", nil, map[string]string{"Cookie": jar}), 401, "unauthorized")
	wantStatus(t, "old CSRF", e.call(t, "POST", "/api/v1/auth/logout", nil, map[string]string{"Cookie": jar2, "X-CSRF-Token": csrf}), 403, "csrf_failed")

	// A Bearer request ignores cookies, so no CSRF token is needed and a
	// cookie cannot stand in for a bad Authorization header.
	app, _ := e.mustLogin(t, "member", userPassword)
	wantStatus(t, "bearer ignores cookies", e.call(t, "POST", "/api/v1/auth/logout", nil, map[string]string{"Cookie": jar2, "Authorization": "Bearer " + app}), 204, "")
	wantStatus(t, "bad bearer with cookies", e.call(t, "GET", "/api/v1/me", nil, map[string]string{"Cookie": jar2, "Authorization": "Bearer x"}), 401, "unauthorized")

	lo := e.call(t, "POST", "/api/v1/auth/logout", nil, map[string]string{"Cookie": jar2, "X-CSRF-Token": newCSRF})
	wantStatus(t, "cookie logout", lo, 204, "")
	if !strings.Contains(strings.Join(lo.header.Values("Set-Cookie"), "\n"), "withalbum_access=;") {
		t.Fatalf("logout did not clear cookies: %v", lo.header.Values("Set-Cookie"))
	}
	wantStatus(t, "after cookie logout", e.call(t, "GET", "/api/v1/me", nil, map[string]string{"Cookie": jar2}), 401, "unauthorized")
}
