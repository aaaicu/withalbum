package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

func TestStorageSetupPossible(t *testing.T) {
	for _, tc := range []struct {
		src  rootSource
		err  error
		want bool
	}{
		{sourceDefault, storage.ErrRootMissing, true},
		{sourceDefault, storage.ErrNotInitialized, true},
		{sourceEnv, storage.ErrNotInitialized, true},
		{sourceEnv, storage.ErrRootMissing, false},    // WITHALBUM_ROOT must exist (M1)
		{sourceConfig, storage.ErrRootMissing, false}, // registered disk missing: fail closed
		{sourceConfig, storage.ErrNotInitialized, false},
		{sourceDefault, storage.ErrInvalidMarker, false},
		{sourceDefault, storage.ErrVolumeMismatch, false},
		{sourceDefault, storage.ErrRestorePending, false},
		{sourceDefault, storage.ErrLocked, false},
	} {
		if got := storageSetupPossible(storageTarget{Source: tc.src}, errors.Join(errors.New("STORAGE"), tc.err)); got != tc.want {
			t.Errorf("source %d, %v: got %v", tc.src, tc.err, got)
		}
	}
}

// TestSetupOnlyOnPlainLoopback: no TLS or LAN listener ever serves setup.
func TestSetupOnlyOnPlainLoopback(t *testing.T) {
	for _, tc := range []struct {
		lc listenConfig
		ok bool
	}{
		{listenConfig{Addr: "127.0.0.1:8787"}, true},
		{listenConfig{Addr: "[::1]:8787"}, true},
		{listenConfig{Addr: "localhost:8787"}, true},
		{listenConfig{Addr: "127.0.0.1:8787", TLS: true}, false},
		{listenConfig{Addr: "127.0.0.1:8787", AllowLAN: true}, false},
		{listenConfig{Addr: "192.168.1.20:8787", AllowLAN: true, TLS: true}, false},
		{listenConfig{Addr: "0.0.0.0:8787", AllowLAN: true, TLS: true}, false},
		{listenConfig{Addr: ":8787"}, false},
	} {
		if got := setupListenProblem(tc.lc) == ""; got != tc.ok {
			t.Errorf("%+v: setup allowed %v", tc.lc, got)
		}
	}
}

// TestResolveStorage: WITHALBUM_ROOT, then the config file (with its volume
// pin), then the default; a broken config file is an error.
func TestResolveStorage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WITHALBUM_CONFIG", filepath.Join(dir, "cfg", "server.json"))
	t.Setenv("WITHALBUM_ROOT", "")
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	t.Chdir(dir)
	got, err := resolveStorage()
	if err != nil || got.Source != sourceDefault || !strings.HasSuffix(got.Root, "withalbum-data") {
		t.Fatalf("default %+v %v", got, err)
	}
	if err := writeConfig(serverConfig{StorageRoot: "/Volumes/X/WithAlbum", VolumeID: strings.Repeat("ab", 16)}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "cfg", "server.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v %v", fi, err)
	}
	if di, _ := os.Stat(filepath.Join(dir, "cfg")); di.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode %v", di.Mode())
	}
	got, err = resolveStorage()
	if err != nil || got.Source != sourceConfig || got.Root != "/Volumes/X/WithAlbum" || got.VolumeID != strings.Repeat("ab", 16) {
		t.Fatalf("config %+v %v", got, err)
	}
	if expectedVolumeID() != strings.Repeat("ab", 16) {
		t.Fatal("volume pin not applied to commands")
	}
	t.Setenv("WITHALBUM_ROOT", dir)
	if got, _ = resolveStorage(); got.Source != sourceEnv || got.Root != dir || got.VolumeID != "" {
		t.Fatalf("env %+v", got)
	}
	t.Setenv("WITHALBUM_ROOT", "")
	if err := os.WriteFile(filepath.Join(dir, "cfg", "server.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStorage(); err == nil {
		t.Fatal("broken config accepted")
	}
}

type setupClient struct {
	t    *testing.T
	srv  *httptest.Server
	c    *http.Client
	csrf string
}

func (sc *setupClient) do(method, path string, body any, hdr map[string]string) (int, map[string]any, string) {
	sc.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, sc.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sc.csrf != "" {
		req.Header.Set("X-Setup-CSRF", sc.csrf)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := sc.c.Do(req)
	if err != nil {
		sc.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return res.StatusCode, m, string(raw)
}

func newSetupClient(t *testing.T, h http.Handler) *setupClient {
	jar, _ := cookiejar.New(nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &setupClient{t: t, srv: srv, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// TestSetupModeEndToEnd: an empty machine (no storage, no config) starts in
// setup mode; the code is required and limited; the chosen directory is
// registered and pinned; the administrator is created through the same
// takeover as "admin init"; the same handler then serves the normal API,
// login works, and the setup routes are gone.
func TestSetupModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WITHALBUM_CONFIG", filepath.Join(dir, "cfg", "server.json"))
	t.Setenv("WITHALBUM_ROOT", "")
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	t.Setenv("HOME", filepath.Join(dir, "home"))
	if err := os.Mkdir(filepath.Join(dir, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	ctx := context.Background()
	target, err := resolveStorage()
	if err != nil {
		t.Fatal(err)
	}
	opts := appOptions{Auth: fastAuth}
	if _, err := openApp(ctx, target.Root, opts); err == nil || !storageSetupPossible(target, err) {
		t.Fatalf("startup on an empty machine: %v", err)
	}
	if _, err := os.Lstat(target.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup created the default root")
	}
	code, display, err := httpapi.NewSetupCode(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	normal := 0
	run, _ := newSetupRun(target, opts, nil, code, func(*app) { normal++ })
	t.Cleanup(func() {
		if a := run.current(); a != nil {
			a.Close()
		}
	})
	sc := newSetupClient(t, run.handler)

	if st, _, _ := sc.do("GET", "/setup", nil, map[string]string{"Host": "evil.example:8787"}); st != 421 {
		t.Fatalf("DNS rebinding Host: %d", st)
	}
	if st, _, _ := sc.do("GET", "/api/v1/health", nil, nil); st != 404 {
		t.Fatalf("normal API in setup mode: %d", st)
	}
	if st, _, _ := sc.do("GET", "/setup/api/volumes", nil, nil); st != 401 {
		t.Fatalf("volumes before the code: %d", st)
	}
	if st, m, _ := sc.do("POST", "/setup/api/unlock", map[string]string{"code": "2222-2222-2222"}, nil); st != 401 || m["error"] != "setup_code_invalid" {
		t.Fatalf("wrong code: %d %v", st, m)
	}
	st, m, raw := sc.do("POST", "/setup/api/unlock", map[string]string{"code": display}, nil)
	if st != 200 || m["step"] != "storage" {
		t.Fatalf("unlock: %d %s", st, raw)
	}
	sc.csrf = m["csrf_token"].(string)

	photos := filepath.Join(dir, "photos")
	if err := os.Mkdir(photos, 0o700); err != nil {
		t.Fatal(err)
	}
	photos, _ = filepath.EvalSymlinks(photos)
	if st, m, raw := sc.do("POST", "/setup/api/storage/check", map[string]string{"path": photos}, nil); st != 200 || m["writable"] != true {
		t.Fatalf("check: %d %s", st, raw)
	}
	if st, m, raw := sc.do("POST", "/setup/api/storage", map[string]any{"path": photos}, nil); st != 200 || m["step"] != "admin" {
		t.Fatalf("storage: %d %s", st, raw)
	}
	if _, err := os.Stat(filepath.Join(photos, storage.MarkerName)); err != nil {
		t.Fatalf("not registered: %v", err)
	}
	got, err := resolveStorage()
	if err != nil || got.Source != sourceConfig || got.Root != photos || len(got.VolumeID) != 32 {
		t.Fatalf("config after setup %+v %v", got, err)
	}
	// The storage is open and locked: console commands refuse meanwhile.
	if _, err := openApp(ctx, photos, opts); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	if normal != 0 {
		t.Fatal("normal mode before the administrator exists")
	}
	if st, m, raw := sc.do("POST", "/setup/api/admin", map[string]string{"login_name": "family-admin", "display_name": "Family", "password": "correct horse battery", "password_confirm": "correct horse battery"}, nil); st != 200 || m["step"] != "done" {
		t.Fatalf("admin: %d %s", st, raw)
	}
	if normal != 1 {
		t.Fatalf("normal mode started %d times", normal)
	}
	// Normal mode on the same listener: setup is gone, login works.
	for _, p := range []string{"/setup", "/setup/api/state", "/setup/api/volumes"} {
		if st, _, _ := sc.do("GET", p, nil, nil); st != 404 {
			t.Errorf("after setup GET %s: %d", p, st)
		}
	}
	if st, _, _ := sc.do("POST", "/setup/api/unlock", map[string]string{"code": display}, nil); st != 404 {
		t.Errorf("after setup unlock: %d", st)
	}
	if st, _, raw := sc.do("GET", "/api/v1/health", nil, nil); st != 200 {
		t.Fatalf("health: %d %s", st, raw)
	}
	if st, _, raw := sc.do("GET", "/", nil, nil); st != 200 || !strings.Contains(raw, `id="loginBox"`) {
		t.Fatalf("dashboard: %d", st)
	}
	sc.csrf = ""
	if st, m, raw := sc.do("POST", "/api/v1/auth/login", map[string]string{"login_name": "family-admin", "password": "correct horse battery", "device_name": "test", "platform": "macos"}, nil); st != 200 || m["access_token"] == nil {
		t.Fatalf("login: %d %s", st, raw)
	}
	a := run.current()
	var actions []string
	rows, err := a.meta.(interface{ DB() *sql.DB }).DB().QueryContext(ctx, "SELECT action FROM audit_log ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		actions = append(actions, s)
	}
	rows.Close()
	joined := strings.Join(actions, ",")
	if !strings.Contains(joined, "account.bootstrap_claim") || !strings.Contains(joined, "setup.complete") {
		t.Fatalf("audit %v", actions)
	}
	u, err := a.auth.Users.GetByLogin(ctx, "family-admin")
	if err != nil || u.Role != users.RoleServerAdmin || u.PasswordLocked {
		t.Fatalf("admin %+v %v", u, err)
	}
}

// TestSetupModeAdminStep: a registered storage without an administrator
// (for example after "-init-storage" only, or a restart between the two
// setup steps) starts setup at the administrator step.
func TestSetupModeAdminStep(t *testing.T) {
	root := newRoot(t)
	ctx := context.Background()
	a, err := openApp(ctx, root, appOptions{Auth: fastAuth})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, _ := a.auth.Users.Initialized(ctx); claimed {
		t.Fatal("claimed")
	}
	code, display, _ := httpapi.NewSetupCode(time.Now())
	run, _ := newSetupRun(storageTarget{Root: root, Source: sourceEnv}, appOptions{Auth: fastAuth}, a, code, nil)
	t.Cleanup(func() { run.current().Close() })
	sc := newSetupClient(t, run.handler)
	st, m, raw := sc.do("POST", "/setup/api/unlock", map[string]string{"code": display}, nil)
	if st != 200 || m["step"] != "admin" {
		t.Fatalf("unlock: %d %s", st, raw)
	}
	sc.csrf = m["csrf_token"].(string)
	if st, m, _ := sc.do("POST", "/setup/api/storage", map[string]any{"path": root}, nil); st != 409 || m["error"] != "storage_already_set" {
		t.Fatalf("storage again: %d %v", st, m)
	}
	if st, _, raw := sc.do("POST", "/setup/api/admin", map[string]string{"login_name": "admin", "password": "correct horse battery", "password_confirm": "correct horse battery"}, nil); st != 200 {
		t.Fatalf("admin: %d %s", st, raw)
	}
	if st, _, _ := sc.do("GET", "/setup", nil, nil); st != 404 {
		t.Fatalf("setup after: %d", st)
	}
	// The WITHALBUM_ROOT case writes no config file.
	if p, _ := configPath(); p != "" {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("config written for WITHALBUM_ROOT: %v", err)
		}
	}
}
