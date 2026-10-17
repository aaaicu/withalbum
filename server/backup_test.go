//go:build darwin || linux

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/backup"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/integrity"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

const backupTestPassword = "backup test password"

func loginAs(t *testing.T, h http.Handler, login string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"login_name":"`+login+`","password":"`+backupTestPassword+`","device_name":"test","platform":"ios"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil || w.Code != 200 {
		t.Fatalf("login %s: %d %s", login, w.Code, w.Body)
	}
	return tok.AccessToken
}

func serveJSON(h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// sharedLibrary is a library with an owner, a VIEWER, an UPLOADER and a
// non-member, and some photos in one album.
type sharedLibrary struct {
	folder string
	assets []string
	logins []string
}

func newSharedLibrary(t *testing.T, root string) sharedLibrary {
	t.Helper()
	a := mustOpenApp(t, root, nil)
	defer a.Close()
	db := a.meta.(interface{ DB() *sql.DB }).DB()
	hash, err := password.Hash(backupTestPassword, fastAuth.PasswordParams)
	if err != nil {
		t.Fatal(err)
	}
	us := users.New(db)
	if _, err := us.ClaimBootstrap(t.Context(), users.Claim{LoginName: "owner", DisplayName: "Owner", PasswordHash: hash}, users.Console); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, login := range []string{"viewer", "uploader", "stranger"} {
		u, err := us.Create(t.Context(), users.NewUser{LoginName: login, DisplayName: login, PasswordHash: hash, Role: users.RoleUser}, users.Console)
		if err != nil {
			t.Fatal(err)
		}
		ids[login] = u.ID
	}
	h := a.api.Handler()
	tok := map[string]string{}
	for _, login := range []string{"owner", "viewer", "uploader", "stranger"} {
		tok[login] = loginAs(t, h, login)
	}
	w := serveJSON(h, tok["owner"], "POST", "/api/v1/folders", `{"name":"family"}`)
	var f struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil || w.Code != 201 || f.ID == "" {
		t.Fatalf("create folder: %d %s", w.Code, w.Body)
	}
	for login, role := range map[string]string{"viewer": "VIEWER", "uploader": "UPLOADER"} {
		w := serveJSON(h, tok["owner"], "POST", "/api/v1/folders/"+f.ID+"/invitations", `{"invited_user_id":"`+ids[login]+`","role":"`+role+`"}`)
		var inv struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil || w.Code != 201 {
			t.Fatalf("invite %s: %d %s", login, w.Code, w.Body)
		}
		if w := serveJSON(h, tok[login], "POST", "/api/v1/invitations/"+inv.ID+"/accept", ""); w.Code != 200 {
			t.Fatalf("accept %s: %d %s", login, w.Code, w.Body)
		}
	}
	lib := sharedLibrary{folder: f.ID, logins: []string{"owner", "viewer", "uploader", "stranger"}}
	for i := range 3 {
		w := serve(h, tok["owner"], "POST", "/api/v1/folders/"+f.ID+"/assets", testPhoto(t, 40+i))
		var res struct {
			AssetID string `json:"asset_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 201 {
			t.Fatalf("upload: %d %s", w.Code, w.Body)
		}
		lib.assets = append(lib.assets, res.AssetID)
	}
	return lib
}

// probe sends the same read requests as every user and records status and
// body hash; the UPLOADER also re-uploads a photo that is already present.
func (l sharedLibrary) probe(t *testing.T, h http.Handler, tok map[string]string) []string {
	t.Helper()
	paths := []string{"/api/v1/folders", "/api/v1/folders/" + l.folder, "/api/v1/folders/" + l.folder + "/sync"}
	for _, a := range l.assets {
		paths = append(paths, "/api/v1/folders/"+l.folder+"/assets/"+a+"/original", "/api/v1/folders/"+l.folder+"/assets/"+a+"/preview")
	}
	var out []string
	for _, login := range l.logins {
		for _, p := range paths {
			w := serve(h, tok[login], "GET", p, nil)
			out = append(out, fmt.Sprintf("%s GET %s -> %d %s", login, p, w.Code, shaHex(w.Body.Bytes())))
		}
		w := serve(h, tok[login], "POST", "/api/v1/folders/"+l.folder+"/assets", testPhoto(t, 40))
		out = append(out, fmt.Sprintf("%s POST upload -> %d", login, w.Code))
	}
	return out
}

func runCmd(t *testing.T, fn func([]string, *bytes.Buffer, *bytes.Buffer) int, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := fn(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func cmdBackup(args []string, o, e *bytes.Buffer) int  { return runBackup(args, o, e) }
func cmdRestore(args []string, o, e *bytes.Buffer) int { return runRestore(args, o, e) }
func cmdVerify(args []string, o, e *bytes.Buffer) int  { return runVerify(args, o, e) }

// TestBackupRestoreSameBehavior: back up a shared library with the CLI,
// restore it into a new volume, and check that every user sees exactly the
// same responses (contents and permissions), that sessions from before the
// restore no longer work, and that verify detects a damaged original.
func TestBackupRestoreSameBehavior(t *testing.T) {
	root := newRoot(t)
	lib := newSharedLibrary(t, root)

	a := mustOpenApp(t, root, nil)
	tok := map[string]string{}
	for _, login := range lib.logins {
		tok[login] = loginAs(t, a.api.Handler(), login)
	}
	before := lib.probe(t, a.api.Handler(), tok)
	if !strings.Contains(strings.Join(before, "\n"), "stranger GET /api/v1/folders/"+lib.folder+" -> 404") ||
		!strings.Contains(strings.Join(before, "\n"), "uploader POST upload -> 202") {
		t.Fatalf("unexpected permissions before backup:\n%s", strings.Join(before, "\n"))
	}

	// Backup while the server runs (the app holds the instance lock).
	t.Setenv("WITHALBUM_ROOT", root)
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(t, cmdBackup, "-to", target); code != 1 || !strings.Contains(stderr, "-init") {
		t.Fatalf("backup to unregistered target: %d %s", code, stderr)
	}
	if code, _, stderr := runCmd(t, cmdBackup, "-init", "-to", target); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	code, stdout, stderr := runCmd(t, cmdBackup, "-to", target, "-json")
	if code != 0 {
		t.Fatalf("backup: %d %s", code, stderr)
	}
	if !strings.Contains(stderr, "same disk") {
		t.Fatalf("no same-disk warning: %s", stderr)
	}
	var br backup.Report
	if err := json.Unmarshal([]byte(stdout), &br); err != nil || br.Blobs != 3 {
		t.Fatalf("backup report: %v %s", err, stdout)
	}
	if code, out, stderr := runCmd(t, cmdRestore, "-from", br.Path, "-check"); code != 0 || !strings.Contains(out, "OK") {
		t.Fatalf("restore -check: %d %s %s", code, out, stderr)
	}
	// Restoring over the running library is refused (instance lock).
	if code, _, stderr := runCmd(t, cmdRestore, "-from", br.Path, "-replace"); code != 1 || !strings.Contains(stderr, "LOCKED") {
		t.Fatalf("restore while running: %d %s", code, stderr)
	}
	a.Close()

	// Restore into a new volume.
	root2 := newRoot(t)
	t.Setenv("WITHALBUM_ROOT", root2)
	if code, out, stderr := runCmd(t, cmdRestore, "-from", br.Path); code != 0 || !strings.Contains(out, "sessions revoked") {
		t.Fatalf("restore: %d %s %s", code, out, stderr)
	}
	a2 := mustOpenApp(t, root2, nil)
	defer a2.Close()
	if !a2.report.Healthy() || a2.report.Changed() {
		t.Fatalf("startup after restore: %s", a2.report.Summary())
	}
	h2 := a2.api.Handler()
	for login, old := range tok {
		if w := serve(h2, old, "GET", "/api/v1/folders", nil); w.Code != 401 {
			t.Fatalf("session of %s from before the restore still works: %d", login, w.Code)
		}
	}
	tok2 := map[string]string{}
	for _, login := range lib.logins {
		tok2[login] = loginAs(t, h2, login)
	}
	after := lib.probe(t, h2, tok2)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("responses differ after restore\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}

	// verify, then a same-size corruption of one original.
	if code, out, stderr := runCmd(t, cmdVerify, "-hash"); code != 0 {
		t.Fatalf("verify restored: %d %s %s", code, out, stderr)
	}
	var sha string
	db2 := a2.meta.(interface{ DB() *sql.DB }).DB()
	if err := db2.QueryRow("SELECT original_sha256 FROM assets WHERE asset_id = ?", lib.assets[1]).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root2, filepath.FromSlash(blobstore.PoolRelPath(sha)))
	os.Chmod(p, 0600)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0x10
	if err := os.WriteFile(p, b, 0400); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCmd(t, cmdVerify); code != 0 {
		t.Fatalf("size-only verify flagged a same-size change: %d", code)
	}
	code, out, stderr := runCmd(t, cmdVerify, "-hash", "-json")
	if code != 1 {
		t.Fatalf("verify -hash on damaged original: %d %s %s", code, out, stderr)
	}
	var vr integrity.Report
	if err := json.Unmarshal([]byte(out), &vr); err != nil {
		t.Fatal(err)
	}
	if vr.HashMismatch != 1 || len(vr.Problems) != 1 || vr.Problems[0].SHA256 != sha ||
		len(vr.Problems[0].Assets) != 1 || vr.Problems[0].Assets[0].AssetID != lib.assets[1] || vr.Problems[0].Assets[0].FolderID != lib.folder {
		t.Fatalf("verify report: %s", out)
	}
	// A missing original.
	os.Remove(p)
	if code, out, _ := runCmd(t, cmdVerify); code != 1 || !strings.Contains(out, "MISSING") {
		t.Fatalf("verify missing: %d %s", code, out)
	}
}

// TestBackupDuringUploads runs backups while uploads are in progress: every
// snapshot must be self-consistent (each original it lists is in the
// backup, verified), and a restored mid-upload snapshot must start cleanly.
func TestBackupDuringUploads(t *testing.T) {
	root := newRoot(t)
	f := newFolder(t, root)
	// Each upload pauses between writing its pool file and committing its
	// row: the window in which a snapshot sees a file without a row.
	a := mustOpenApp(t, root, slowCommit{})
	defer a.Close()
	h := a.api.Handler()
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.InitBackupTarget(target); err != nil {
		t.Fatal(err)
	}
	// The backup opens the storage and the database on its own, like the CLI.
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := integrity.OpenDB(vol)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	const writers, perWriter = 3, 8
	var wg sync.WaitGroup
	done := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				if r := serve(h, f.token, "POST", "/api/v1/folders/"+f.id+"/assets", testPhoto(t, w*100+i)); r.Code != 201 {
					t.Errorf("upload: %d %s", r.Code, r.Body)
				}
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()
	var reports []*backup.Report
	for running := true; running || len(reports) < 2; {
		time.Sleep(15 * time.Millisecond)
		select {
		case <-done:
			running = false
		default:
		}
		r, err := backup.Backup(t.Context(), vol, sqlDB, target, backup.Options{})
		if err != nil {
			t.Fatalf("backup during uploads: %v", err)
		}
		reports = append(reports, r)
	}
	if last := reports[len(reports)-1]; last.Blobs != writers*perWriter {
		t.Fatalf("final backup has %d originals, want %d", last.Blobs, writers*perWriter)
	}
	for _, r := range reports {
		if _, err := backup.Restore(t.Context(), r.Path, "", backup.RestoreOptions{CheckOnly: true}); err != nil {
			t.Fatalf("snapshot %s (%d originals): %v", r.ID, r.Blobs, err)
		}
	}
	// Restore a mid-upload snapshot: it starts with nothing to repair.
	var first *backup.Report
	for _, r := range reports {
		if r.Blobs > 0 && r.Blobs < writers*perWriter {
			first = r
			break
		}
	}
	if first == nil {
		t.Fatalf("no snapshot was taken while uploads were in progress (%d snapshots)", len(reports))
	}
	root2 := newRoot(t)
	if _, err := backup.Restore(t.Context(), first.Path, root2, backup.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	a2 := mustOpenApp(t, root2, nil)
	defer a2.Close()
	if !a2.report.Healthy() || a2.report.Changed() || a2.report.Blobs != first.Blobs {
		t.Fatalf("restored mid-upload snapshot: %s, want %d blobs", a2.report.Summary(), first.Blobs)
	}
	t.Logf("%d snapshots, restored one had %d of %d originals", len(reports), first.Blobs, writers*perWriter)
}

// slowCommit delays every upload right before its metadata commit.
type slowCommit struct{}

func (slowCommit) Fault(point string) error {
	if point == httpapi.FaultBeforeCommit {
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// TestStartupRefusesInterruptedRestore: while the restore journal exists the
// server does not start.
func TestStartupRefusesInterruptedRestore(t *testing.T) {
	root := newRoot(t)
	if err := os.WriteFile(filepath.Join(root, storage.RestoreJournalName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := openApp(t.Context(), root, appOptions{Auth: fastAuth})
	if err == nil || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("startup with a restore journal: %v", err)
	}
}
