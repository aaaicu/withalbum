package sqlitestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/metatest"
	"github.com/aaaicu/withalbum/internal/storage"
)

func newVolume(t *testing.T) *storage.Volume {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

func open(t *testing.T, vol *storage.Volume) *Store {
	t.Helper()
	s, err := Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func sha(n int) string { return fmt.Sprintf("%064x", n+1) }

func TestContract(t *testing.T) {
	metatest.Run(t, func(t *testing.T) meta.MetadataStore { return open(t, newVolume(t)) })
}

func TestOpenCreatesDatabaseAndBootstrapOwner(t *testing.T) {
	vol := newVolume(t)
	s := open(t, vol)
	defer s.Close()
	st, err := os.Stat(DBPath(vol.Root()))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("database mode %v, want 0600", st.Mode().Perm())
	}
	var id, role, status, hash string
	var canCreate int
	if err := s.db.QueryRow("SELECT user_id, server_role, status, password_hash, can_create_albums FROM users WHERE login_name = ?", BootstrapLoginName).
		Scan(&id, &role, &status, &hash, &canCreate); err != nil {
		t.Fatal(err)
	}
	if id != s.ownerID || len(id) != 32 || role != "SERVER_ADMIN" || status != "active" || canCreate != 1 {
		t.Fatalf("bootstrap user %s %s %s %d", id, role, status, canCreate)
	}
	// Not a PHC hash: no password can ever match it.
	if strings.HasPrefix(hash, "$") || hash == "" {
		t.Fatalf("bootstrap password_hash %q is not locked", hash)
	}
	f, err := s.CreateFolder(t.Context(), "family")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT count(*) FROM folders WHERE folder_id = ? AND owner_user_id = ?", f.ID, id); n != 1 {
		t.Fatal("folder not owned by the bootstrap administrator")
	}
}

func TestOpenRefusesUnverifiedVolume(t *testing.T) {
	vol := newVolume(t)
	if err := os.Remove(filepath.Join(vol.Root(), storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(t.Context(), vol); err == nil {
		s.Close()
		t.Fatal("Open succeeded without a volume marker")
	}
	if _, err := os.Stat(DBPath(vol.Root())); !os.IsNotExist(err) {
		t.Fatalf("database created on an unverified volume: %v", err)
	}
}

func TestCommitStoresRandomAssetIDAndAllRows(t *testing.T) {
	ctx := t.Context()
	s := open(t, newVolume(t))
	defer s.Close()
	f, err := s.CreateFolder(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	in := meta.Asset{ID: sha(0), Filename: "a.jpg", MimeType: "image/jpeg", Bytes: 10, PreviewReady: true}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, DeviceID: "phone", Asset: in}); err != nil {
		t.Fatal(err)
	}
	var assetID, kind, relPath string
	if err := s.db.QueryRow("SELECT a.asset_id, a.media_kind, b.relative_path FROM assets a JOIN blobs b ON b.sha256 = a.original_sha256 WHERE a.original_sha256 = ?", sha(0)).
		Scan(&assetID, &kind, &relPath); err != nil {
		t.Fatal(err)
	}
	if assetID == sha(0) || len(assetID) != 32 || kind != "image" || relPath != ".system/pool/"+sha(0)[:2]+"/"+sha(0) {
		t.Fatalf("asset_id %q kind %q path %q", assetID, kind, relPath)
	}
	if n := count(t, s, "SELECT count(*) FROM folder_assets WHERE folder_id = ? AND asset_id = ? AND uploader_device_label = 'phone'", f.ID, assetID); n != 1 {
		t.Fatal("folder_assets row missing")
	}
	if n := count(t, s, "SELECT count(*) FROM folder_changes WHERE folder_id = ? AND seq = 1 AND asset_id = ? AND uploader_device_label = 'phone'", f.ID, assetID); n != 1 {
		t.Fatal("folder_changes row missing")
	}
	if n := count(t, s, "SELECT last_seq FROM folder_sequences WHERE folder_id = ?", f.ID); n != 1 {
		t.Fatalf("last_seq = %d", n)
	}
}

// TestFailedCommitRollsBack makes the blob insert violate a CHECK constraint
// and requires that no row of the upload survives.
func TestFailedCommitRollsBack(t *testing.T) {
	ctx := t.Context()
	s := open(t, newVolume(t))
	defer s.Close()
	f, err := s.CreateFolder(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	bad := meta.Asset{ID: sha(0), Filename: "a.jpg", MimeType: "image/jpeg", Bytes: 0} // blobs.bytes > 0
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: bad}); err == nil {
		t.Fatal("commit with bytes=0 succeeded")
	}
	for _, table := range []string{"blobs", "assets", "folder_assets", "folder_changes"} {
		if n := count(t, s, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("%s has %d rows after a failed commit", table, n)
		}
	}
	if n := count(t, s, "SELECT last_seq FROM folder_sequences WHERE folder_id = ?", f.ID); n != 0 {
		t.Fatalf("last_seq = %d after a failed commit", n)
	}
	bad.Bytes = 5
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: bad}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.ListEvents(ctx, f.ID, 0, 10); err != nil || len(p.Events) != 1 || p.Events[0].Seq != 1 {
		t.Fatalf("events after recovery %+v %v", p, err)
	}
}

// TestConcurrentUploadsSameBlob: 20 goroutines commit the same original, half
// to one shared folder and half to their own folder, and then a distinct
// original each to the shared folder. Run with -race.
func TestConcurrentUploadsSameBlob(t *testing.T) {
	ctx := t.Context()
	s := open(t, newVolume(t))
	defer s.Close()
	shared, err := s.CreateFolder(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	own := make([]meta.Folder, n)
	for i := range own {
		if own[i], err = s.CreateFolder(ctx, fmt.Sprintf("own-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	same := meta.Asset{ID: sha(1000), Filename: "same.jpg", MimeType: "image/jpeg", Bytes: 42}
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[string]int{}
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := shared.ID
			if i%2 == 1 {
				target = own[i].ID
			}
			res, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: target, DeviceID: fmt.Sprintf("dev-%d", i), Asset: same})
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			results[res.Result]++
			mu.Unlock()
			unique := meta.Asset{ID: sha(i), Filename: "u.jpg", MimeType: "image/jpeg", Bytes: int64(i + 1)}
			if res, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: shared.ID, Asset: unique}); err != nil {
				errs <- err
			} else if res.Result != meta.ResultCreated {
				errs <- fmt.Errorf("unique asset %d: %s", i, res.Result)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// The shared folder gets one link, each odd goroutine's folder one link.
	want := map[string]int{meta.ResultCreated: 1, meta.ResultLinked: n / 2, meta.ResultAlreadyPresent: n/2 - 1}
	if fmt.Sprint(results) != fmt.Sprint(want) {
		t.Fatalf("results %v, want %v", results, want)
	}
	if c := count(t, s, "SELECT count(*) FROM blobs WHERE sha256 = ?", same.ID); c != 1 {
		t.Fatalf("shared blob rows %d", c)
	}
	if c := count(t, s, "SELECT count(*) FROM assets WHERE original_sha256 = ?", same.ID); c != 1 {
		t.Fatalf("shared asset rows %d", c)
	}
	if c := count(t, s, "SELECT count(*) FROM blobs"); c != n+1 {
		t.Fatalf("blob rows %d, want %d", c, n+1)
	}
	if c := count(t, s, "SELECT count(*) FROM folder_assets fa JOIN assets a USING (asset_id) WHERE a.original_sha256 = ?", same.ID); c != n/2+1 {
		t.Fatalf("shared links %d, want %d", c, n/2+1)
	}
	for i, f := range own {
		p, err := s.ListEvents(ctx, f.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if wantEvents := i % 2; len(p.Events) != wantEvents || (wantEvents == 1 && (p.Events[0].Seq != 1 || p.Events[0].AssetID != same.ID)) {
			t.Fatalf("own folder %d events %+v", i, p.Events)
		}
	}
	p, err := s.ListEvents(ctx, shared.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != n+1 || p.HasMore {
		t.Fatalf("shared events %d, more %v", len(p.Events), p.HasMore)
	}
	seen := map[string]bool{}
	for i, e := range p.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
		if seen[e.AssetID] {
			t.Fatalf("asset %s has two events", e.AssetID)
		}
		seen[e.AssetID] = true
	}
	if c := count(t, s, "SELECT last_seq FROM folder_sequences WHERE folder_id = ?", shared.ID); c != n+1 {
		t.Fatalf("last_seq %d", c)
	}
}

// TestRestartPreservesState closes and reopens the store: folders, assets,
// events and cursors are unchanged and the sequence continues.
func TestRestartPreservesState(t *testing.T) {
	ctx := t.Context()
	vol := newVolume(t)
	s := open(t, vol)
	f1, err := s.CreateFolder(ctx, "family")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := s.CreateFolder(ctx, "trip")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a := meta.Asset{ID: sha(i), Filename: fmt.Sprintf("%d.jpg", i), MimeType: "image/jpeg", Bytes: int64(10 + i), PreviewReady: i == 0}
		if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f1.ID, DeviceID: "d1", Asset: a}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f2.ID, DeviceID: "d2", Asset: meta.Asset{ID: sha(0), Bytes: 10}}); err != nil {
		t.Fatal(err)
	}
	folders, _ := s.ListFolders(ctx)
	before1, _ := s.ListEvents(ctx, f1.ID, 1, 10)
	before2, _ := s.ListEvents(ctx, f2.ID, 0, 10)
	asset0, _ := s.GetAssetBySHA(ctx, sha(0))
	owner := s.ownerID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	r := open(t, vol)
	defer r.Close()
	if r.ownerID != owner || count(t, r, "SELECT count(*) FROM users") != 1 {
		t.Fatal("bootstrap owner changed or duplicated on reopen")
	}
	after, _ := r.ListFolders(ctx)
	if fmt.Sprint(after) != fmt.Sprint(folders) {
		t.Fatalf("folders %v, want %v", after, folders)
	}
	after1, _ := r.ListEvents(ctx, f1.ID, 1, 10)
	after2, _ := r.ListEvents(ctx, f2.ID, 0, 10)
	if fmt.Sprintf("%+v", after1) != fmt.Sprintf("%+v", before1) || fmt.Sprintf("%+v", after2) != fmt.Sprintf("%+v", before2) {
		t.Fatalf("events changed:\n%+v\n%+v\nwant\n%+v\n%+v", after1, after2, before1, before2)
	}
	if after1.NextCursor != 3 || after2.NextCursor != 1 {
		t.Fatalf("cursors %d %d", after1.NextCursor, after2.NextCursor)
	}
	if got, err := r.GetAssetBySHA(ctx, sha(0)); err != nil || got != asset0 || !got.PreviewReady {
		t.Fatalf("asset after reopen %+v %v, want %+v", got, err, asset0)
	}
	res, err := r.CommitUpload(ctx, meta.CommitRequest{FolderID: f1.ID, Asset: meta.Asset{ID: sha(9), Bytes: 1}})
	if err != nil || res.Result != meta.ResultCreated {
		t.Fatal(res, err)
	}
	if p, _ := r.ListEvents(ctx, f1.ID, 3, 10); len(p.Events) != 1 || p.Events[0].Seq != 4 {
		t.Fatalf("seq after reopen %+v", p)
	}
}

func TestForEachBlobInSHAOrder(t *testing.T) {
	ctx := t.Context()
	s := open(t, newVolume(t))
	defer s.Close()
	f, err := s.CreateFolder(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{5, 1, 9, 3} {
		if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: meta.Asset{ID: sha(n), Filename: "x", MimeType: "image/jpeg", Bytes: int64(n + 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	err = s.ForEachBlob(ctx, func(h string, size int64) error {
		got = append(got, fmt.Sprintf("%s:%d", h, size))
		return nil
	})
	want := []string{sha(1) + ":2", sha(3) + ":4", sha(5) + ":6", sha(9) + ":10"}
	if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ForEachBlob = %v %v", got, err)
	}
	stop := errors.New("stop")
	calls := 0
	if err := s.ForEachBlob(ctx, func(string, int64) error { calls++; return stop }); !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("ForEachBlob stop = %v after %d calls", err, calls)
	}
}

// TestWriteContentionIsErrBusy: while another connection holds the write
// lock longer than busy_timeout (5s), writes fail with meta.ErrBusy and
// change nothing.
func TestWriteContentionIsErrBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for busy_timeout")
	}
	t.Parallel()
	ctx := t.Context()
	vol := newVolume(t)
	s := open(t, vol)
	defer s.Close()
	f, err := s.CreateFolder(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.Open(DBPath(vol.Root()))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(ctx, nil) // BEGIN IMMEDIATE: takes the write lock
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: meta.Asset{ID: sha(0), Filename: "x", MimeType: "image/jpeg", Bytes: 1}})
	if !errors.Is(err, meta.ErrBusy) {
		t.Fatalf("CommitUpload under contention = %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 4*time.Second {
		t.Fatalf("gave up after %v, before busy_timeout", waited)
	}
	if _, err := s.CreateFolder(context.Background(), "g"); !errors.Is(err, meta.ErrBusy) {
		t.Fatalf("CreateFolder under contention = %v, want ErrBusy", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT count(*) FROM assets"); n != 0 {
		t.Fatalf("assets = %d after busy commit", n)
	}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, Asset: meta.Asset{ID: sha(0), Filename: "x", MimeType: "image/jpeg", Bytes: 1}}); err != nil {
		t.Fatalf("retry after contention: %v", err)
	}
}

// After `admin init` renames the bootstrap administrator, the store must keep
// using the same account as folder owner instead of creating a new locked
// "admin".
func TestBootstrapOwnerFoundByIDAfterRename(t *testing.T) {
	vol := newVolume(t)
	s := open(t, vol)
	owner := s.ownerID
	if n := count(t, s, "SELECT count(*) FROM account_bootstrap WHERE user_id = ? AND claimed_at IS NULL", owner); n != 1 {
		t.Fatal("bootstrap account not recorded in account_bootstrap")
	}
	if s.DB() == nil {
		t.Fatal("DB() is nil")
	}
	if _, err := s.db.Exec("UPDATE users SET login_name = 'renamed' WHERE user_id = ?", owner); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r := open(t, vol)
	defer r.Close()
	if r.ownerID != owner || count(t, r, "SELECT count(*) FROM users") != 1 {
		t.Fatal("a second bootstrap account was created after the rename")
	}
	f, err := r.CreateFolder(t.Context(), "after")
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, r, "SELECT count(*) FROM folders WHERE folder_id = ? AND owner_user_id = ?", f.ID, owner); n != 1 {
		t.Fatal("new folder not owned by the renamed bootstrap account")
	}
}
