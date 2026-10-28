package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

// adminTestParams keeps tests fast. The real command uses password.DefaultParams.
var adminTestParams = password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

type adminRun struct {
	code           int
	stdout, stderr string
}

// runAdminPipe runs an admin command with stdin as a pipe.
func runAdminPipe(stdin string, args ...string) adminRun {
	var out, errOut bytes.Buffer
	code := runAdmin(args, adminIO{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut, params: adminTestParams})
	return adminRun{code, out.String(), errOut.String()}
}

// runAdminTTY runs an admin command as if typed on a terminal.
func runAdminTTY(lines []string, args ...string) adminRun {
	var out, errOut bytes.Buffer
	i := 0
	read := func() ([]byte, error) {
		if i >= len(lines) {
			return nil, errors.New("EOF")
		}
		i++
		return []byte(lines[i-1]), nil
	}
	code := runAdmin(args, adminIO{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut, readHidden: read, params: adminTestParams})
	return adminRun{code, out.String(), errOut.String()}
}

func (r adminRun) leaks(secret string) bool {
	return strings.Contains(r.stdout+r.stderr, secret) || strings.Contains(r.stdout+r.stderr, "$argon2id")
}

// storedHash reads the password hash of login directly from the database.
func storedHashOf(t *testing.T, root, login string) string {
	t.Helper()
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var h string
	if err := s.DB().QueryRow("SELECT password_hash FROM users WHERE login_name = ?", login).Scan(&h); err != nil {
		t.Fatalf("hash of %s: %v", login, err)
	}
	return h
}

func TestAdminCommandsEndToEnd(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	// The server ran before: the locked bootstrap account owns a folder.
	legacy := newBootstrapFolder(t, root)

	const adminPW = "first admin password"
	if r := runAdminPipe("bob password 123\n", "create-user", "-login", "bob"); r.code != 1 || !strings.Contains(r.stderr, "admin init") {
		t.Fatalf("create-user before init: %+v", r)
	}
	if r := runAdminPipe("whatever password\n", "reset-password", "-login", "admin"); r.code != 1 || !strings.Contains(r.stderr, "admin init") {
		t.Fatalf("reset-password of locked bootstrap: %+v", r)
	}
	if r := runAdminPipe("short\n", "init", "-login", "alice"); r.code != 1 || !strings.Contains(r.stderr, "at least 12") || r.leaks("short") {
		t.Fatalf("short password: %+v", r)
	}
	r := runAdminPipe(adminPW+"\n", "init", "-login", "alice", "-display-name", "Alice")
	if r.code != 0 || !strings.Contains(r.stdout, "login=alice") || !strings.Contains(r.stdout, "1 existing folder") || r.leaks(adminPW) {
		t.Fatalf("init: %+v", r)
	}
	if ok, err := password.Verify(adminPW, storedHashOf(t, root, "alice")); !ok || err != nil {
		t.Fatalf("admin password does not verify: %v %v", ok, err)
	}
	// Second init: refused before reading any password, nothing changes.
	before := storedHashOf(t, root, "alice")
	if r := runAdminPipe("attacker password\n", "init", "-login", "mallory"); r.code != 1 || !strings.Contains(r.stderr, "already been set up") {
		t.Fatalf("second init: %+v", r)
	}
	if storedHashOf(t, root, "alice") != before {
		t.Fatal("second init changed the administrator")
	}
	// The legacy folder still belongs to the same account, and the server
	// does not create another bootstrap account.
	a := mustOpenApp(t, root, nil)
	var owner, ownerLogin string
	var nUsers int
	sqlDB := a.meta.(*sqlitestore.Store).DB()
	if err := sqlDB.QueryRow("SELECT f.owner_user_id, u.login_name FROM folders f JOIN users u ON u.user_id = f.owner_user_id WHERE f.folder_id = ?", legacy).Scan(&owner, &ownerLogin); err != nil {
		t.Fatal(err)
	}
	sqlDB.QueryRow("SELECT count(*) FROM users").Scan(&nUsers)
	a.Close()
	if ownerLogin != "alice" || nUsers != 1 || !strings.Contains(r.stdout, owner) {
		t.Fatalf("legacy folder owner %s (%s), %d users", owner, ownerLogin, nUsers)
	}

	// create-user over a pipe, and on a terminal (typed twice).
	if r := runAdminPipe("bob password 123\n", "create-user", "-login", "bob"); r.code != 0 || !strings.Contains(r.stdout, "role=USER can_create_albums=false") || r.leaks("bob password 123") {
		t.Fatalf("create-user: %+v", r)
	}
	if r := runAdminPipe("bob password 123\n", "create-user", "-login", "BOB"); r.code != 1 || !strings.Contains(r.stderr, "already in use") {
		t.Fatalf("duplicate create-user: %+v", r)
	}
	if r := runAdminTTY([]string{"carol password 1", "carol password 2"}, "create-user", "-login", "carol"); r.code != 1 || !strings.Contains(r.stderr, "do not match") || r.leaks("carol password") {
		t.Fatalf("mismatched tty passwords: %+v", r)
	}
	if r := runAdminTTY([]string{"carol password 1", "carol password 1"}, "create-user", "-login", "carol", "-role", "SERVER_ADMIN"); r.code != 0 || !strings.Contains(r.stdout, "role=SERVER_ADMIN") || r.leaks("carol password 1") {
		t.Fatalf("tty create-user: %+v", r)
	}
	if r := runAdminPipe("x\n", "create-user", "-login", "dave", "-role", "OWNER"); r.code != 1 || !strings.Contains(r.stderr, "invalid server role") {
		t.Fatalf("bad role: %+v", r)
	}

	// reset-password.
	if r := runAdminPipe("new bob password\n", "reset-password", "-login", "bob"); r.code != 0 || r.leaks("new bob password") {
		t.Fatalf("reset-password: %+v", r)
	}
	h := storedHashOf(t, root, "bob")
	if ok, _ := password.Verify("new bob password", h); !ok {
		t.Fatal("reset password does not verify")
	}
	if ok, _ := password.Verify("bob password 123", h); ok {
		t.Fatal("old password still verifies")
	}
	if r := runAdminPipe("new bob password\n", "reset-password", "-login", "nobody"); r.code != 1 || !strings.Contains(r.stderr, "no account") {
		t.Fatalf("reset unknown: %+v", r)
	}
}

func TestAdminPasswordInputRules(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	for name, stdin := range map[string]string{
		"empty":      "",
		"blank line": "\n",
		"two lines":  "first password 1\nsecond password\n",
		"too long":   strings.Repeat("a", password.MaxBytes+1) + "\n",
		"nul":        "first\x00password 1\n",
	} {
		if r := runAdminPipe(stdin, "init", "-login", "alice"); r.code != 1 {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// CRLF and a missing final newline are accepted.
	if r := runAdminPipe("first admin password\r\n", "init", "-login", "alice"); r.code != 0 {
		t.Fatalf("CRLF: %+v", r)
	}
	if ok, _ := password.Verify("first admin password", storedHashOf(t, root, "alice")); !ok {
		t.Fatal("CRLF password stored with the line ending")
	}
	if r := runAdminPipe("other password 1", "reset-password", "-login", "alice"); r.code != 0 {
		t.Fatalf("no final newline: %+v", r)
	}
}

func TestAdminUsage(t *testing.T) {
	t.Setenv("WITHALBUM_ROOT", newRoot(t))
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"init"},                                 // -login missing
		{"init", "-login", "alice", "secret pw"}, // positional argument (no passwords on the command line)
		{"init", "-login", "alice", "-password"}, // no such flag
		{"reset-password", "-login", "a", "-role"}, // flag of another command
	} {
		if r := runAdminPipe("first admin password\n", args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, r.code, r.stderr)
		}
	}
	if r := runAdminPipe("", "init", "-login", "alice", "pw"); !strings.Contains(r.stderr, "never accepted as arguments") {
		t.Fatalf("hint missing: %s", r.stderr)
	}
}

func TestAdminRefusesWhileServerRuns(t *testing.T) {
	root := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root)
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	a := mustOpenApp(t, root, nil) // holds the instance lock like a running server
	r := runAdminPipe("first admin password\n", "init", "-login", "alice")
	a.Close()
	if r.code != 1 || !strings.Contains(r.stderr, "LOCKED") {
		t.Fatalf("init while server runs: %+v", r)
	}
	if r := runAdminPipe("first admin password\n", "init", "-login", "alice"); r.code != 0 {
		t.Fatalf("init after stop: %+v", r)
	}
}

func TestAdminRefusesUnmigratedOrMissingStorage(t *testing.T) {
	vol := newTestVolume(t)
	writeLegacyState(t, vol.Root())
	t.Setenv("WITHALBUM_ROOT", vol.Root())
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	if r := runAdminPipe("first admin password\n", "init", "-login", "alice"); r.code != 1 || !strings.Contains(r.stderr, "migrate-legacy") {
		t.Fatalf("unmigrated: %+v", r)
	}
	t.Setenv("WITHALBUM_ROOT", t.TempDir()+"/missing")
	if r := runAdminPipe("first admin password\n", "init", "-login", "alice"); r.code != 1 || !strings.Contains(r.stderr, "STORAGE") {
		t.Fatalf("missing root: %+v", r)
	}
}
