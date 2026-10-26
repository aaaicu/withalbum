//go:build darwin || linux

package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/integrity"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

// library is a source volume with a database and some originals.
type library struct {
	root   string
	vol    *storage.Volume
	store  *sqlitestore.Store
	blobs  *blobstore.Store
	folder string
	files  map[string][]byte // sha -> content
}

func newDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newVolume(t *testing.T) (string, *storage.Volume) {
	t.Helper()
	root := newDir(t)
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return root, vol
}

func newTarget(t *testing.T) string {
	t.Helper()
	dir := newDir(t)
	if _, err := storage.InitBackupTarget(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func photo(t *testing.T, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for i := range img.Pix {
		img.Pix[i] = byte(i*3 + seed*11)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newLibrary(t *testing.T, n int) *library {
	t.Helper()
	root, vol := newVolume(t)
	store, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	bs, err := blobstore.New(vol)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.CreateFolder(t.Context(), meta.NewFolder{Name: "f", OwnerID: store.BootstrapOwnerID()})
	if err != nil {
		t.Fatal(err)
	}
	l := &library{root: root, vol: vol, store: store, blobs: bs, folder: f.ID, files: map[string][]byte{}}
	for i := range n {
		l.add(t, photo(t, i))
	}
	return l
}

func (l *library) add(t *testing.T, body []byte) string {
	t.Helper()
	staged, err := l.blobs.Stage(bytes.NewReader(body), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.blobs.Promote(staged, staged.SHA256); err != nil {
		t.Fatal(err)
	}
	staged.Remove()
	ready := l.blobs.MakePreview(staged.SHA256) == nil
	_, err = l.store.CommitUpload(context.Background(), meta.CommitRequest{FolderID: l.folder,
		Asset: meta.Asset{SHA256: staged.SHA256, Filename: fmt.Sprintf("p%d.jpg", len(l.files)), MimeType: staged.MimeType, Bytes: staged.Size, PreviewReady: ready}})
	if err != nil {
		t.Fatal(err)
	}
	l.files[staged.SHA256] = body
	return staged.SHA256
}

func (l *library) poolPath(sha string) string {
	return filepath.Join(l.root, filepath.FromSlash(blobstore.PoolRelPath(sha)))
}

func (l *library) backup(t *testing.T, target string, opt Options) *Report {
	t.Helper()
	r, err := Backup(t.Context(), l.vol, l.store.DB(), target, opt)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return r
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// treeOf maps every regular file below dir (relative path) to its SHA-256,
// skipping SQLite side files and the lock file.
func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") || d.Name() == storage.LockName {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = shaOf(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBackupAndCheck(t *testing.T) {
	l := newLibrary(t, 3)
	target := newTarget(t)
	r := l.backup(t, target, Options{ServerVersion: "test"})
	if r.Blobs != 3 || r.Copied != 3 || r.Reused != 0 || r.Previews != 3 || r.SameDevice != true {
		t.Fatalf("report %+v", r)
	}
	fi, err := os.Stat(filepath.Join(r.Path, DBName))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("snapshot db mode: %v %v", fi.Mode(), err)
	}
	for sha, body := range l.files {
		got, err := os.ReadFile(filepath.Join(target, "pool", sha[:2], sha))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("pool copy of %s: %v", sha, err)
		}
	}
	cr, err := Restore(t.Context(), r.Path, "", RestoreOptions{CheckOnly: true})
	if err != nil || cr.Blobs != 3 || cr.Previews != 3 {
		t.Fatalf("check: %+v %v", cr, err)
	}

	// Incremental: only the new original is copied.
	l.add(t, photo(t, 100))
	r2 := l.backup(t, target, Options{})
	if r2.Blobs != 4 || r2.Copied != 1 || r2.Reused != 3 {
		t.Fatalf("second backup %+v", r2)
	}
	if r2.ID == r.ID {
		t.Fatal("same snapshot id")
	}
	// The first snapshot is still complete and valid.
	if _, err := Restore(t.Context(), r.Path, "", RestoreOptions{CheckOnly: true}); err != nil {
		t.Fatal(err)
	}
	// A lost source original is reported even though the target has it.
	for sha := range l.files {
		os.Remove(l.poolPath(sha))
		r3 := l.backup(t, target, Options{})
		if len(r3.SourceWarnings) != 1 || r3.SourceWarnings[0] != sha {
			t.Fatalf("source warnings: %+v", r3)
		}
		break
	}
}

func TestBackupRefusesBadTargets(t *testing.T) {
	l := newLibrary(t, 1)
	// Not registered.
	if _, err := Backup(t.Context(), l.vol, l.store.DB(), newDir(t), Options{}); !errors.Is(err, storage.ErrNotInitialized) {
		t.Fatalf("unregistered target: %v", err)
	}
	// Missing (an unmounted backup disk): never created.
	missing := filepath.Join(newDir(t), "gone")
	if _, err := Backup(t.Context(), l.vol, l.store.DB(), missing, Options{}); !errors.Is(err, storage.ErrRootMissing) {
		t.Fatalf("missing target: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing target was created")
	}
	// A storage root is not a backup target, and vice versa.
	if _, err := storage.InitBackupTarget(l.root); !errors.Is(err, storage.ErrAlreadyInitialized) {
		t.Fatalf("init backup target in a storage root: %v", err)
	}
	if _, err := Backup(t.Context(), l.vol, l.store.DB(), l.root, Options{}); err == nil {
		t.Fatal("backup into the storage root")
	}
	inside := filepath.Join(l.root, "backups")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.InitBackupTarget(inside); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(t.Context(), l.vol, l.store.DB(), inside, Options{}); !errors.Is(err, ErrTargetInsideSource) {
		t.Fatalf("target inside source: %v", err)
	}
	// Locked by another backup.
	target := newTarget(t)
	tv, err := storage.OpenBackupTarget(target, true)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := tv.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(t.Context(), l.vol, l.store.DB(), target, Options{}); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("locked target: %v", err)
	}
	lock.Release()
}

func TestBackupRefusesDamagedSource(t *testing.T) {
	l := newLibrary(t, 2)
	var bad string
	for sha := range l.files {
		bad = sha
		break
	}
	// Same size, different content: only a hash shows it.
	p := l.poolPath(bad)
	os.Chmod(p, 0600)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0400); err != nil {
		t.Fatal(err)
	}
	target := newTarget(t)
	r, err := Backup(t.Context(), l.vol, l.store.DB(), target, Options{})
	if !errors.Is(err, ErrSourceDamaged) || len(r.SourceDamaged) != 1 || !strings.Contains(r.SourceDamaged[0], bad) {
		t.Fatalf("damaged source: %v %+v", err, r)
	}
	snaps, _ := os.ReadDir(filepath.Join(target, SnapshotsDir))
	if len(snaps) != 0 {
		t.Fatalf("a snapshot was left: %v", snaps)
	}
	// The good original was still copied, the damaged one was not.
	if _, err := os.Stat(filepath.Join(target, "pool", bad[:2], bad)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("damaged original copied")
	}
	// verify -hash names it with its asset.
	vr, err := integrity.Run(t.Context(), l.vol, l.store.DB(), integrity.Options{Hash: true})
	if err != nil || vr.Healthy() || vr.HashMismatch != 1 || len(vr.Problems) != 1 || vr.Problems[0].SHA256 != bad || len(vr.Problems[0].Assets) != 1 {
		t.Fatalf("verify: %v %+v", err, vr)
	}
	// Without -hash the same-size damage is invisible.
	if vr, err := integrity.Run(t.Context(), l.vol, l.store.DB(), integrity.Options{}); err != nil || !vr.Healthy() {
		t.Fatalf("size-only verify: %v %+v", err, vr)
	}
}

func TestBackupRehashReplacesDamagedCopy(t *testing.T) {
	l := newLibrary(t, 2)
	target := newTarget(t)
	l.backup(t, target, Options{})
	var sha string
	for s := range l.files {
		sha = s
		break
	}
	p := filepath.Join(target, "pool", sha[:2], sha)
	os.Chmod(p, 0600)
	b, _ := os.ReadFile(p)
	b[10] ^= 1
	os.WriteFile(p, b, 0400)
	// A damaged copy makes the snapshot fail its check.
	snaps, _ := os.ReadDir(filepath.Join(target, SnapshotsDir))
	first := filepath.Join(target, SnapshotsDir, snaps[0].Name())
	if _, err := Restore(t.Context(), first, "", RestoreOptions{CheckOnly: true}); !errors.Is(err, ErrBackupDamaged) {
		t.Fatalf("check of damaged backup: %v", err)
	}
	r := l.backup(t, target, Options{Rehash: true})
	if r.Replaced != 1 || r.Copied != 1 || r.Reused != 1 {
		t.Fatalf("rehash: %+v", r)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, l.files[sha]) {
		t.Fatal("copy not repaired")
	}
	dmg, _ := os.ReadDir(filepath.Join(target, "damaged"))
	if len(dmg) != 1 {
		t.Fatalf("damaged copy not kept: %v", dmg)
	}
	if _, err := Restore(t.Context(), first, "", RestoreOptions{CheckOnly: true}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCrashLeavesNoSnapshot(t *testing.T) {
	for _, point := range []string{PointAfterDBSnapshot, PointAfterBlobCopy, PointBeforeFinalize} {
		t.Run(point, func(t *testing.T) {
			l := newLibrary(t, 2)
			target := newTarget(t)
			errCrash := errors.New("crash")
			_, err := Backup(t.Context(), l.vol, l.store.DB(), target, Options{hook: func(p string) error {
				if p == point {
					return errCrash
				}
				return nil
			}})
			if !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
			snaps, _ := os.ReadDir(filepath.Join(target, SnapshotsDir))
			for _, s := range snaps {
				if !strings.HasPrefix(s.Name(), partialPrefix) {
					t.Fatalf("finished snapshot after crash: %s", s.Name())
				}
			}
			r := l.backup(t, target, Options{})
			snaps, _ = os.ReadDir(filepath.Join(target, SnapshotsDir))
			if len(snaps) != 1 || snaps[0].Name() != r.ID {
				t.Fatalf("snapshots after retry: %v", snaps)
			}
			if _, err := Restore(t.Context(), r.Path, "", RestoreOptions{CheckOnly: true}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// restoreAndCompare restores snapshot into a new volume and compares every
// original and preview with the source.
func restoreAndCompare(t *testing.T, l *library, snapshot string) (string, *RestoreReport) {
	t.Helper()
	root, _ := newVolume(t)
	rr, err := Restore(t.Context(), snapshot, root, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	compareLibrary(t, l, root)
	return root, rr
}

func compareLibrary(t *testing.T, l *library, root string) {
	t.Helper()
	src := treeOf(t, filepath.Join(l.root, ".system"))
	dst := treeOf(t, filepath.Join(root, ".system"))
	for _, k := range keys(src) {
		if strings.HasPrefix(k, "pool/") || strings.HasPrefix(k, "previews/") {
			if dst[k] != src[k] {
				t.Fatalf("restored %s differs", k)
			}
		}
	}
	for _, k := range keys(dst) {
		if k == "withalbum.db" {
			continue
		}
		if src[k] != dst[k] {
			t.Fatalf("restored has extra or different %s", k)
		}
	}
}

func TestRestoreIntoNewVolume(t *testing.T) {
	l := newLibrary(t, 3)
	target := newTarget(t)
	r := l.backup(t, target, Options{})
	root, rr := restoreAndCompare(t, l, r.Path)
	if rr.Blobs != 3 || rr.Previews != 3 || rr.PreservedPath != "" {
		t.Fatalf("report %+v", rr)
	}
	for _, name := range []string{stagingName, storage.RestoreJournalName} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left behind", name)
		}
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vr, err := integrity.Run(t.Context(), vol, st.DB(), integrity.Options{Hash: true})
	if err != nil || !vr.Healthy() || vr.Blobs != 3 || vr.PreviewsMissing != 0 || vr.Unreferenced != 0 {
		t.Fatalf("verify restored: %v %+v", err, vr)
	}
	var action string
	if err := st.DB().QueryRow("SELECT action FROM audit_log WHERE action = 'storage.restore'").Scan(&action); err != nil {
		t.Fatalf("restore not audited: %v", err)
	}
	fs1, _ := l.store.ListFolders(t.Context())
	fs2, _ := st.ListFolders(t.Context())
	if fmt.Sprint(fs1) != fmt.Sprint(fs2) {
		t.Fatalf("folders differ: %v vs %v", fs1, fs2)
	}
}

func TestRestoreRefusesNonEmptyAndKeepsReplaced(t *testing.T) {
	l := newLibrary(t, 2)
	target := newTarget(t)
	r := l.backup(t, target, Options{})
	// Restoring onto the source itself (a library in use) needs -replace.
	l.store.Close()
	before := treeOf(t, filepath.Join(l.root, ".system"))
	if _, err := Restore(t.Context(), r.Path, l.root, RestoreOptions{}); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("non-empty target: %v", err)
	}
	// The refused restore changed nothing.
	if after := treeOf(t, filepath.Join(l.root, ".system")); !maps.Equal(after, before) {
		t.Fatalf("refused restore changed the library:\nbefore %v\nafter  %v", before, after)
	}
	// Change the library after the backup, then restore over it.
	l.store, _ = sqlitestore.Open(t.Context(), l.vol)
	extra := l.add(t, photo(t, 50))
	l.store.Close()
	before = treeOf(t, filepath.Join(l.root, ".system"))
	rr, err := Restore(t.Context(), r.Path, l.root, RestoreOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if rr.PreservedPath == "" {
		t.Fatal("previous data not preserved")
	}
	kept := treeOf(t, rr.PreservedPath)
	for k, v := range before {
		if kept[k] != v {
			t.Fatalf("previous %s not preserved intact", k)
		}
	}
	// The restored library does not have the later upload.
	if _, err := os.Stat(l.poolPath(extra)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("restore kept a later original in .system")
	}
	l.store, _ = sqlitestore.Open(t.Context(), l.vol)
}

func TestRestoreRefusesDamagedBackup(t *testing.T) {
	cases := map[string]func(t *testing.T, target, snap string, l *library){
		"blob": func(t *testing.T, target, snap string, l *library) {
			for sha := range l.files {
				p := filepath.Join(target, "pool", sha[:2], sha)
				os.Chmod(p, 0600)
				b, _ := os.ReadFile(p)
				b[len(b)-3] ^= 4
				os.WriteFile(p, b, 0400)
				return
			}
		},
		"missing blob": func(t *testing.T, target, snap string, l *library) {
			for sha := range l.files {
				os.Remove(filepath.Join(target, "pool", sha[:2], sha))
				return
			}
		},
		"database": func(t *testing.T, target, snap string, l *library) {
			p := filepath.Join(snap, DBName)
			b, _ := os.ReadFile(p)
			b[len(b)-100] ^= 1
			os.WriteFile(p, b, 0600)
		},
		"manifest": func(t *testing.T, target, snap string, l *library) {
			p := filepath.Join(snap, ManifestName)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(b, []byte(`"blobs": [`), []byte(`"blobs": [{"sha256":"`+strings.Repeat("0", 64)+`","bytes":1},`), 1), 0600)
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLibrary(t, 2)
			target := newTarget(t)
			r := l.backup(t, target, Options{})
			damage(t, target, r.Path, l)
			if _, err := Restore(t.Context(), r.Path, "", RestoreOptions{CheckOnly: true}); !errors.Is(err, ErrBackupDamaged) {
				t.Fatalf("check: %v", err)
			}
			root, _ := newVolume(t)
			if _, err := Restore(t.Context(), r.Path, root, RestoreOptions{}); !errors.Is(err, ErrBackupDamaged) {
				t.Fatalf("restore: %v", err)
			}
			// Nothing was activated: the volume is still empty and opens.
			if _, err := os.Stat(filepath.Join(root, ".system", "withalbum.db")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("damaged backup activated")
			}
			if _, err := storage.Open(root, storage.OpenOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRestoreCrashRecovery stops the restore at every crash point, checks
// what the volume looks like there, and reruns it to completion.
func TestRestoreCrashRecovery(t *testing.T) {
	points := []string{PointAfterStaging, PointAfterJournal, PointAfterPreserve, PointAfterSwap}
	for i, point := range points {
		t.Run(point, func(t *testing.T) {
			l := newLibrary(t, 2)
			target := newTarget(t)
			r := l.backup(t, target, Options{})
			// Restore over an existing library, so preservation is exercised.
			dst := newLibrary(t, 1)
			dst.store.Close()
			before := treeOf(t, filepath.Join(dst.root, ".system"))
			errCrash := errors.New("crash")
			_, err := Restore(t.Context(), r.Path, dst.root, RestoreOptions{Replace: true, hook: func(p string) error {
				if p == point {
					return errCrash
				}
				return nil
			}})
			if !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
			_, openErr := storage.Open(dst.root, storage.OpenOptions{})
			if i == 0 {
				// Before the journal: the old library is untouched and usable.
				if openErr != nil {
					t.Fatal(openErr)
				}
				if got := treeOf(t, filepath.Join(dst.root, ".system")); fmt.Sprint(got) != fmt.Sprint(before) {
					t.Fatal("old library changed before the journal")
				}
			} else if !errors.Is(openErr, storage.ErrRestorePending) {
				t.Fatalf("open during interrupted restore: %v", openErr)
			}
			rr, err := Restore(t.Context(), r.Path, dst.root, RestoreOptions{Replace: true})
			if err != nil {
				t.Fatal(err)
			}
			if i > 0 && !rr.Resumed {
				t.Fatal("not resumed")
			}
			compareLibrary(t, l, dst.root)
			if _, err := storage.Open(dst.root, storage.OpenOptions{}); err != nil {
				t.Fatal(err)
			}
			// The old library survives in exactly one preserved directory.
			entries, _ := os.ReadDir(dst.root)
			var preserved []string
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), preservedPrefix) {
					preserved = append(preserved, e.Name())
				}
				if e.Name() == stagingName {
					t.Fatal("staging left behind")
				}
			}
			if len(preserved) != 1 {
				t.Fatalf("preserved dirs: %v", preserved)
			}
			kept := treeOf(t, filepath.Join(dst.root, preserved[0]))
			for k, v := range before {
				if kept[k] != v {
					t.Fatalf("preserved %s differs", k)
				}
			}
			if _, err := os.Stat(filepath.Join(dst.root, ".system", stagingSentinel)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("sentinel left in .system")
			}
		})
	}
}

func TestRestoreRevokesSessions(t *testing.T) {
	l := newLibrary(t, 1)
	sqlDB := l.store.DB()
	mustExec(t, sqlDB, "INSERT INTO devices (device_id, user_id, platform, device_name) VALUES ('d1', ?, 'ios', 'phone')", l.store.BootstrapOwnerID())
	mustExec(t, sqlDB, "INSERT INTO auth_sessions (session_id, device_id, refresh_token_sha256, expires_at) VALUES ('s1', 'd1', ?, '2999-01-01T00:00:00.000000Z')", strings.Repeat("a", 64))
	target := newTarget(t)
	r := l.backup(t, target, Options{})
	root, rr := restoreAndCompare(t, l, r.Path)
	if rr.SessionsRevoked != 1 {
		t.Fatalf("revoked %d", rr.SessionsRevoked)
	}
	vol, _ := storage.Open(root, storage.OpenOptions{})
	st, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var reason string
	var devRevoked sql.NullString
	if err := st.DB().QueryRow("SELECT s.revoke_reason, d.revoked_at FROM auth_sessions s JOIN devices d USING (device_id) WHERE session_id = 's1'").Scan(&reason, &devRevoked); err != nil {
		t.Fatal(err)
	}
	if reason != "restore" || !devRevoked.Valid {
		t.Fatalf("session after restore: %q %v", reason, devRevoked)
	}
	// The source is untouched.
	var revoked sql.NullString
	sqlDB.QueryRow("SELECT revoked_at FROM auth_sessions WHERE session_id = 's1'").Scan(&revoked)
	if revoked.Valid {
		t.Fatal("source session revoked")
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
