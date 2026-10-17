package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/users"
)

var setupFastParams = password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

// setupEnv is a setup handler with recorded callbacks and a movable clock.
type setupEnv struct {
	t         *testing.T
	srv       *httptest.Server
	code      string
	csrf      string
	cookie    *http.Cookie
	mu        sync.Mutex
	now       time.Time
	mounted   map[string]bool
	registers []string
	claims    []users.Claim
	activated int
	adminSet  bool
	regErr    error
}

func newSetupEnv(t *testing.T, mod func(*httpapi.SetupConfig)) *setupEnv {
	t.Helper()
	e := &setupEnv{t: t, now: time.Now(), mounted: map[string]bool{}}
	code, display, err := httpapi.NewSetupCode(e.now)
	if err != nil {
		t.Fatal(err)
	}
	e.code = display
	cfg := httpapi.SetupConfig{Code: code, PasswordParams: setupFastParams,
		Now: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now },
		IsMountPoint: func(p string) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.mounted[p], nil
		},
		Register: func(_ context.Context, root string) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.registers = append(e.registers, root)
			return e.adminSet, e.regErr
		},
		Claim: func(_ context.Context, c users.Claim) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.claims = append(e.claims, c)
			return nil
		},
		Activate: func() { e.mu.Lock(); e.activated++; e.mu.Unlock() },
	}
	if mod != nil {
		mod(&cfg)
	}
	e.srv = httptest.NewServer(httpapi.NewSetup(cfg).Handler())
	t.Cleanup(e.srv.Close)
	return e
}

type setupResp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    string
}

// call sends a same-origin request (as the setup page does) unless headers
// override it.
func (e *setupEnv) call(method, path string, body any, hdr map[string]string) setupResp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.csrf != "" {
		req.Header.Set("X-Setup-CSRF", e.csrf)
	}
	if e.cookie != nil {
		req.AddCookie(e.cookie)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := setupResp{Status: res.StatusCode, Header: res.Header, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	for _, c := range res.Cookies() {
		if c.Name == "withalbum_setup" {
			if c.MaxAge < 0 {
				e.cookie = nil
			} else {
				e.cookie = c
			}
		}
	}
	return out
}

func (e *setupEnv) unlock() {
	e.t.Helper()
	r := e.call("POST", "/setup/api/unlock", map[string]string{"code": strings.ToLower(e.code)}, nil)
	if r.Status != 200 || e.cookie == nil {
		e.t.Fatalf("unlock %d %s", r.Status, r.Raw)
	}
	if !e.cookie.HttpOnly || e.cookie.SameSite != http.SameSiteStrictMode || e.cookie.Path != "/setup" {
		e.t.Fatalf("setup cookie %+v", e.cookie)
	}
	e.csrf, _ = r.Body["csrf_token"].(string)
	if len(e.csrf) != 64 {
		e.t.Fatalf("csrf %q", e.csrf)
	}
}

func wantSetup(t *testing.T, what string, r setupResp, status int, code string) {
	t.Helper()
	if r.Status != status || (code != "" && r.Body["error"] != code) {
		t.Fatalf("%s: got %d %s, want %d %s", what, r.Status, r.Raw, status, code)
	}
}

// TestSetupCodeLimits: wrong codes are counted and disable the code after
// SetupMaxFailures; an expired code and a used code are refused.
func TestSetupCodeLimits(t *testing.T) {
	e := newSetupEnv(t, nil)
	for i := 1; i < httpapi.SetupMaxFailures; i++ {
		r := e.call("POST", "/setup/api/unlock", map[string]string{"code": "AAAA-AAAA-AAAA"}, nil)
		wantSetup(t, "wrong code", r, 401, "setup_code_invalid")
		if left := r.Body["attempts_left"].(float64); int(left) != httpapi.SetupMaxFailures-i {
			t.Fatalf("attempts_left %v after %d", left, i)
		}
	}
	wantSetup(t, "last wrong code", e.call("POST", "/setup/api/unlock", map[string]string{"code": "x"}, nil), 423, "setup_code_disabled")
	// Even the right code no longer works.
	wantSetup(t, "right code after lockout", e.call("POST", "/setup/api/unlock", map[string]string{"code": e.code}, nil), 423, "setup_code_disabled")
	if e.cookie != nil {
		t.Fatal("a session was opened")
	}

	e = newSetupEnv(t, nil)
	e.mu.Lock()
	e.now = e.now.Add(httpapi.SetupCodeTTL + time.Second)
	e.mu.Unlock()
	wantSetup(t, "expired", e.call("POST", "/setup/api/unlock", map[string]string{"code": e.code}, nil), 410, "setup_code_expired")

	e = newSetupEnv(t, nil)
	e.unlock()
	first := e.cookie
	e.cookie = nil
	wantSetup(t, "used", e.call("POST", "/setup/api/unlock", map[string]string{"code": e.code}, nil), 409, "setup_code_used")
	// The session expires.
	e.cookie = first
	wantSetup(t, "session", e.call("GET", "/setup/api/state", nil, nil), 200, "")
	e.mu.Lock()
	e.now = e.now.Add(2 * time.Hour)
	e.mu.Unlock()
	wantSetup(t, "expired session", e.call("GET", "/setup/api/state", nil, nil), 401, "setup_locked")
}

// TestSetupCodeFormat: 12 characters of the 32-letter alphabet in groups of
// four, different every time.
func TestSetupCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		_, c, err := httpapi.NewSetupCode(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}-[2-9A-HJ-NP-Z]{4}$`).MatchString(c) || seen[c] {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
}

// TestSetupSessionAndCSRF: nothing but the page and the code entry is
// answered without the session; cross-site and non-JSON requests are
// refused; POSTs need the CSRF token.
func TestSetupSessionAndCSRF(t *testing.T) {
	home := t.TempDir()
	e := newSetupEnv(t, func(c *httpapi.SetupConfig) { c.Home = home })
	for _, p := range []string{"/setup/api/state", "/setup/api/volumes"} {
		wantSetup(t, p, e.call("GET", p, nil, nil), 401, "setup_locked")
	}
	for _, p := range []string{"/setup/api/storage/check", "/setup/api/storage", "/setup/api/admin"} {
		wantSetup(t, p, e.call("POST", p, map[string]string{}, nil), 401, "setup_locked")
	}
	wantSetup(t, "cross-site unlock", e.call("POST", "/setup/api/unlock", map[string]string{"code": e.code}, map[string]string{"Sec-Fetch-Site": "cross-site"}), 403, "cross_origin")
	wantSetup(t, "foreign origin unlock", e.call("POST", "/setup/api/unlock", map[string]string{"code": e.code}, map[string]string{"Origin": "http://evil.example"}), 403, "cross_origin")
	wantSetup(t, "form unlock", e.call("POST", "/setup/api/unlock", map[string]string{"code": e.code}, map[string]string{"Content-Type": "text/plain"}), 415, "")
	e.unlock() // none of the refused attempts consumed the code or counted
	csrf := e.csrf
	e.csrf = ""
	wantSetup(t, "no csrf", e.call("POST", "/setup/api/storage/check", map[string]string{"path": home}, nil), 403, "csrf_failed")
	e.csrf = strings.Repeat("0", 64)
	wantSetup(t, "wrong csrf", e.call("POST", "/setup/api/storage/check", map[string]string{"path": home}, nil), 403, "csrf_failed")
	e.csrf = csrf
	wantSetup(t, "foreign origin", e.call("POST", "/setup/api/storage/check", map[string]string{"path": home}, map[string]string{"Origin": "http://evil.example"}), 403, "cross_origin")
	wantSetup(t, "form", e.call("POST", "/setup/api/storage/check", map[string]string{"path": home}, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}), 415, "")
	r := e.call("POST", "/setup/api/storage/check", map[string]string{"path": home}, map[string]string{"Origin": e.srv.URL, "Sec-Fetch-Site": "same-origin"})
	wantSetup(t, "same origin", r, 200, "")
	// A reload of the page gets the CSRF token again with the cookie.
	if st := e.call("GET", "/setup/api/state", nil, nil); st.Body["csrf_token"] != csrf {
		t.Fatalf("state %s", st.Raw)
	}
}

// TestSetupPageAndRoutes: the page is strict (CSP by hash, no HTML sinks),
// "/" goes to it, and no other path, in particular no API of normal mode,
// is served in setup mode.
func TestSetupPageAndRoutes(t *testing.T) {
	e := newSetupEnv(t, nil)
	r := e.call("GET", "/setup", nil, nil)
	if r.Status != 200 || r.Header.Get("Content-Type") != "text/html; charset=utf-8" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page %d %v", r.Status, r.Header)
	}
	page := r.Raw
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src " + cspHash(inlineBody(t, page, "script")) + ";",
		"style-src " + cspHash(inlineBody(t, page, "style")) + ";", "connect-src 'self'", "img-src 'none'", "base-uri 'none'",
		"form-action 'none'", "frame-ancestors 'none'", "require-trusted-types-for 'script'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-") || strings.Contains(csp, "*") {
		t.Errorf("CSP too broad: %q", csp)
	}
	for _, re := range []string{`innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document\.write`, `\beval\(`, `new Function`,
		`\son[a-z]+\s*=\s*["']`, `\sstyle\s*=\s*["']`, `javascript:`, `<script[^>]`, `https?://`, `@import`, `url\(`, `<link\b`, `<img\b`, `\ssrc\s*=`, `업로드 완료`} {
		if regexp.MustCompile(re).MatchString(page) {
			t.Errorf("setup page matches %s", re)
		}
	}
	for _, s := range regexp.MustCompile(`<svg[^>]*>`).FindAllString(page, -1) {
		if !strings.Contains(s, `aria-hidden="true"`) {
			t.Errorf("icon without aria-hidden: %s", s)
		}
	}
	if r := e.call("GET", "/", nil, nil); r.Status != 303 || r.Header.Get("Location") != "/setup" {
		t.Fatalf("/ → %d %q", r.Status, r.Header.Get("Location"))
	}
	for _, rp := range httpapi.Routes() {
		method, path, _ := strings.Cut(rp.Pattern, " ")
		if path == "" {
			method, path = "GET", rp.Pattern
		}
		if path == "/{$}" {
			continue
		}
		path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "x")
		var body any
		if method == "POST" || method == "PATCH" || method == "PUT" {
			body = map[string]string{}
		}
		if res := e.call(method, path, body, nil); res.Status != 404 && res.Status != 405 {
			t.Errorf("setup mode answers %s %s with %d", method, path, res.Status)
		}
	}
	// The route tables are disjoint: setup paths only in setup mode.
	for _, rp := range httpapi.Routes() {
		if strings.Contains(rp.Pattern, "/setup") || rp.Access == httpapi.AccessSetup || rp.Access == httpapi.AccessSetupPublic {
			t.Errorf("normal route table contains %s", rp.Pattern)
		}
	}
	for _, rp := range httpapi.SetupRoutes() {
		_, path, _ := strings.Cut(rp.Pattern, " ")
		if path == "" {
			path = rp.Pattern
		}
		if !strings.HasPrefix(path, "/setup") && path != "/{$}" && path != "/" {
			t.Errorf("setup route outside /setup: %s", rp.Pattern)
		}
		if strings.HasPrefix(path, "/setup/api/") && path != "/setup/api/unlock" && rp.Access != httpapi.AccessSetup {
			t.Errorf("setup API %s is not behind the setup session", rp.Pattern)
		}
	}
}

// TestNormalModeHasNoSetupRoutes: the normal handler answers every setup
// path with 404 (or 401 for API paths it does not know without a session).
func TestNormalModeHasNoSetupRoutes(t *testing.T) {
	e := newACLEnv(t)
	for _, rp := range httpapi.SetupRoutes() {
		method, path, _ := strings.Cut(rp.Pattern, " ")
		if path == "" || path == "/{$}" || path == "/" {
			continue
		}
		res := do(t, e.Raw, method, path, nil, map[string]string{"Content-Type": "application/json"})
		if res.Status != 404 && res.Status != 405 {
			t.Errorf("normal mode answers %s %s with %d", method, path, res.Status)
		}
	}
}

// TestSetupNeverCreatesRoots: a missing directory is never created except
// the one WithAlbum folder, on explicit request, directly on a selected disk
// that is mounted at that moment (or in the home folder).
func TestSetupNeverCreatesRoots(t *testing.T) {
	home := t.TempDir()
	vols := t.TempDir()
	for _, d := range []string{"Disk1", "Gone"} {
		if err := os.Mkdir(filepath.Join(vols, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e := newSetupEnv(t, func(c *httpapi.SetupConfig) { c.Home = home; c.VolumesDir = vols })
	disk1, gone := filepath.Join(evalDir(t, vols), "Disk1"), filepath.Join(evalDir(t, vols), "Gone")
	e.mounted[disk1] = true
	e.unlock()
	r := e.call("GET", "/setup/api/volumes", nil, nil)
	wantSetup(t, "volumes", r, 200, "")
	cands := r.Body["candidates"].([]any)
	byKind := map[string]map[string]any{}
	for _, c := range cands {
		m := c.(map[string]any)
		byKind[m["kind"].(string)+":"+m["name"].(string)] = m
	}
	sys, d1, g := byKind["system:"+filepath.Base(home)], byKind["disk:Disk1"], byKind["disk:Gone"]
	if sys == nil || d1 == nil || g == nil || len(cands) != 3 {
		t.Fatalf("candidates %s", r.Raw)
	}
	if g["problem"] != "not_mounted" || d1["problem"] != "" || d1["exists"] != false || d1["path"] != filepath.Join(disk1, "WithAlbum") {
		t.Fatalf("disk candidates %v %v", d1, g)
	}
	nothing := func(paths ...string) {
		t.Helper()
		for _, p := range paths {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s was created (%v)", p, err)
			}
		}
	}
	// An unmounted disk cannot be chosen, with or without the folder.
	wantSetup(t, "gone", e.call("POST", "/setup/api/storage", map[string]any{"base": gone, "path": filepath.Join(gone, "WithAlbum"), "create_folder": true}, nil), 409, "not_mounted")
	// Without the explicit request nothing is created.
	wantSetup(t, "no create", e.call("POST", "/setup/api/storage", map[string]any{"base": disk1, "path": filepath.Join(disk1, "WithAlbum")}, nil), 409, "folder_missing")
	// A custom path must exist and is never created.
	missing := filepath.Join(home, "nope", "deeper")
	wantSetup(t, "custom missing", e.call("POST", "/setup/api/storage", map[string]any{"path": missing, "create_folder": true}, nil), 400, "missing")
	wantSetup(t, "relative", e.call("POST", "/setup/api/storage", map[string]any{"path": "relative/dir"}, nil), 400, "invalid_path")
	// A path that is not the disk's WithAlbum folder is not a choice.
	wantSetup(t, "other folder", e.call("POST", "/setup/api/storage", map[string]any{"base": disk1, "path": filepath.Join(disk1, "x", "y"), "create_folder": true}, nil), 400, "unknown_choice")
	// The disk was unplugged between listing and creating: refused.
	e.mu.Lock()
	e.mounted[disk1] = false
	e.mu.Unlock()
	wantSetup(t, "unplugged", e.call("POST", "/setup/api/storage", map[string]any{"base": disk1, "path": filepath.Join(disk1, "WithAlbum"), "create_folder": true}, nil), 409, "not_mounted")
	nothing(filepath.Join(disk1, "WithAlbum"), filepath.Join(gone, "WithAlbum"), filepath.Join(home, "nope"), filepath.Join(home, "WithAlbum"))
	if len(e.registers) != 0 {
		t.Fatalf("registered %v", e.registers)
	}
	// Mounted again: exactly one folder is created, then registered.
	e.mu.Lock()
	e.mounted[disk1] = true
	e.mu.Unlock()
	r = e.call("POST", "/setup/api/storage", map[string]any{"base": disk1, "path": filepath.Join(disk1, "WithAlbum"), "create_folder": true}, nil)
	wantSetup(t, "create", r, 200, "")
	if r.Body["step"] != "admin" || len(e.registers) != 1 || e.registers[0] != filepath.Join(disk1, "WithAlbum") {
		t.Fatalf("register %v %s", e.registers, r.Raw)
	}
	if fi, err := os.Stat(filepath.Join(disk1, "WithAlbum")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder %v %v", fi, err)
	}
	entries, _ := os.ReadDir(filepath.Join(disk1, "WithAlbum"))
	if len(entries) != 0 {
		t.Fatalf("setup wrote into the new folder: %v", entries)
	}
	// Only once.
	wantSetup(t, "again", e.call("POST", "/setup/api/storage", map[string]any{"path": home}, nil), 409, "storage_already_set")
}

func evalDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSetupFixedRoot: with WITHALBUM_ROOT only that directory is offered.
func TestSetupFixedRoot(t *testing.T) {
	root := evalDir(t, t.TempDir())
	other := t.TempDir()
	e := newSetupEnv(t, func(c *httpapi.SetupConfig) { c.FixedRoot = root; c.Home = other })
	e.unlock()
	r := e.call("GET", "/setup/api/volumes", nil, nil)
	if c := r.Body["candidates"].([]any); len(c) != 1 || c[0].(map[string]any)["path"] != root || r.Body["custom_allowed"] != false {
		t.Fatalf("volumes %s", r.Raw)
	}
	wantSetup(t, "other", e.call("POST", "/setup/api/storage", map[string]any{"path": other}, nil), 400, "unknown_choice")
	wantSetup(t, "fixed", e.call("POST", "/setup/api/storage", map[string]any{"path": root}, nil), 200, "")
}

// TestSetupAdminAndSwitch: the administrator is validated, claimed once,
// and the server is switched to normal mode; the setup session ends.
func TestSetupAdminAndSwitch(t *testing.T) {
	root := t.TempDir()
	e := newSetupEnv(t, nil)
	e.unlock()
	admin := func(login, pw, pw2 string) setupResp {
		return e.call("POST", "/setup/api/admin", map[string]string{"login_name": login, "display_name": "", "password": pw, "password_confirm": pw2}, nil)
	}
	wantSetup(t, "before storage", admin("alice", "correct horse battery", "correct horse battery"), 409, "storage_required")
	wantSetup(t, "storage", e.call("POST", "/setup/api/storage", map[string]any{"path": root}, nil), 200, "")
	wantSetup(t, "short", admin("alice", "short", "short"), 400, "invalid_password")
	wantSetup(t, "mismatch", admin("alice", "correct horse battery", "correct horse batterY"), 400, "password_mismatch")
	wantSetup(t, "login", admin("-x", "correct horse battery", "correct horse battery"), 400, "invalid_login_name")
	if len(e.claims) != 0 || e.activated != 0 {
		t.Fatal("claimed early")
	}
	r := admin("alice", "correct horse battery", "correct horse battery")
	wantSetup(t, "admin", r, 200, "")
	if len(e.claims) != 1 || e.claims[0].LoginName != "alice" || e.claims[0].DisplayName != "alice" || e.activated != 1 {
		t.Fatalf("claims %+v activated %d", e.claims, e.activated)
	}
	if ok, err := password.Verify("correct horse battery", e.claims[0].PasswordHash); err != nil || !ok {
		t.Fatalf("hash %v %v", ok, err)
	}
	if strings.Contains(r.Raw, "correct horse") || strings.Contains(r.Raw, "argon2") {
		t.Fatalf("response leaks: %s", r.Raw)
	}
	if e.cookie != nil {
		t.Fatal("setup cookie not cleared")
	}
	wantSetup(t, "after", e.call("GET", "/setup/api/state", nil, nil), 401, "setup_locked")
}

// TestSetupExistingAdministrator: choosing a storage that already has an
// administrator ends setup right away.
func TestSetupExistingAdministrator(t *testing.T) {
	e := newSetupEnv(t, nil)
	e.adminSet = true
	e.unlock()
	r := e.call("POST", "/setup/api/storage", map[string]any{"path": t.TempDir()}, nil)
	if r.Status != 200 || r.Body["step"] != "done" || e.activated != 1 {
		t.Fatalf("%d %s activated %d", r.Status, r.Raw, e.activated)
	}
}

// TestSetupRegisterRefusal: a refusal of the server is shown by its code.
func TestSetupRegisterRefusal(t *testing.T) {
	e := newSetupEnv(t, nil)
	e.regErr = &httpapi.SetupRefusal{Code: "legacy_metadata", Err: errors.New("state.json")}
	e.unlock()
	wantSetup(t, "refused", e.call("POST", "/setup/api/storage", map[string]any{"path": t.TempDir()}, nil), 409, "legacy_metadata")
	e.regErr = nil
	wantSetup(t, "retry", e.call("POST", "/setup/api/storage", map[string]any{"path": t.TempDir()}, nil), 200, "")
}
