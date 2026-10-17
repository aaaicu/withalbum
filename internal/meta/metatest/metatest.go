// Package metatest is a conformance suite for meta.MetadataStore
// implementations. Import it only from tests.
package metatest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Store is a store under test with an active account that may own folders.
type Store struct {
	meta.MetadataStore
	OwnerID string
}

// Run checks the meta.MetadataStore contract. open must return a new, empty
// store each time it is called, and the user_id of an active account; Run
// closes the store.
func Run(t *testing.T, open func(t *testing.T) Store) {
	newStore := func(t *testing.T) Store {
		s := open(t)
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return s
	}
	t.Run("Folders", func(t *testing.T) { testFolders(t, newStore(t)) })
	t.Run("CommitUpload", func(t *testing.T) { testCommitUpload(t, newStore(t)) })
	t.Run("ListEvents", func(t *testing.T) { testListEvents(t, newStore(t)) })
	t.Run("ConcurrentCommits", func(t *testing.T) { testConcurrentCommits(t, newStore(t)) })
	t.Run("BeforeCommit", func(t *testing.T) { testBeforeCommit(t, newStore(t)) })
	t.Run("Authorize", func(t *testing.T) { testAuthorize(t, newStore(t)) })
}

// testAuthorize: the hook sees the stored principal and access, is called
// for every outcome, and a refusal writes nothing.
func testAuthorize(t *testing.T, s Store) {
	ctx := context.Background()
	refuse := errors.New("no longer allowed")
	var seen []authz.Principal
	deny := func(p authz.Principal, a authz.AlbumAccess) error { seen = append(seen, p); return refuse }
	if _, err := s.CreateFolder(ctx, meta.NewFolder{Name: "x", OwnerID: s.OwnerID, Authorize: deny}); !errors.Is(err, refuse) {
		t.Fatalf("CreateFolder = %v, want the Authorize error", err)
	}
	if list, _ := s.ListFolders(ctx); len(list) != 0 {
		t.Fatalf("folder created despite refusal: %v", list)
	}
	if len(seen) != 1 || seen[0].UserID != s.OwnerID || !seen[0].Active {
		t.Fatalf("Authorize saw %+v", seen)
	}
	f := mustFolder(t, s, "f")
	var gotAccess authz.AlbumAccess
	allow := func(p authz.Principal, a authz.AlbumAccess) error { gotAccess = a; return nil }
	mustCommit(t, s, meta.CommitRequest{FolderID: f.ID, UserID: s.OwnerID, Asset: asset(0, "a.jpg"), Authorize: allow}, meta.ResultCreated)
	if gotAccess.OwnerID != s.OwnerID || gotAccess.HasMembership {
		t.Fatalf("Authorize access %+v", gotAccess)
	}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, UserID: s.OwnerID, Asset: asset(1, "b.jpg"), Authorize: deny}); !errors.Is(err, refuse) {
		t.Fatalf("CommitUpload = %v, want the Authorize error", err)
	}
	// Refused even when nothing would have been written.
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, UserID: s.OwnerID, Asset: asset(0, "a.jpg"), Authorize: deny}); !errors.Is(err, refuse) {
		t.Fatalf("CommitUpload (already present) = %v, want the Authorize error", err)
	}
	if p := mustEvents(t, s, f.ID, 0, 10); len(p.Events) != 1 {
		t.Fatalf("events after refusals: %+v", p.Events)
	}
	unknown := func(p authz.Principal, a authz.AlbumAccess) error {
		if p != (authz.Principal{}) {
			t.Errorf("unknown user principal %+v", p)
		}
		return refuse
	}
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, UserID: "nobody", Asset: asset(2, "c.jpg"), Authorize: unknown}); !errors.Is(err, refuse) {
		t.Fatalf("CommitUpload (unknown user) = %v", err)
	}
}

// testBeforeCommit: a failing BeforeCommit hook leaves no trace, its error
// is returned wrapped, and it is not called for ResultAlreadyPresent.
func testBeforeCommit(t *testing.T, s Store) {
	ctx := context.Background()
	f := mustFolder(t, s, "hooked")
	refuse := errors.New("volume lost")
	calls := 0
	req := meta.CommitRequest{FolderID: f.ID, Asset: asset(0, "a.jpg"), BeforeCommit: func() error {
		calls++
		return refuse
	}}
	if _, err := s.CommitUpload(ctx, req); !errors.Is(err, refuse) {
		t.Fatalf("CommitUpload = %v, want the BeforeCommit error", err)
	}
	if calls != 1 {
		t.Fatalf("BeforeCommit called %d times", calls)
	}
	if _, err := s.GetAssetBySHA(ctx, sha(0)); !errors.Is(err, meta.ErrAssetNotFound) {
		t.Fatalf("asset stored after refused commit: %v", err)
	}
	if p := mustEvents(t, s, f.ID, 0, 10); len(p.Events) != 0 {
		t.Fatalf("events after refused commit: %+v", p.Events)
	}
	req.BeforeCommit = func() error { calls++; return nil }
	mustCommit(t, s, req, meta.ResultCreated)
	if p := mustEvents(t, s, f.ID, 0, 10); len(p.Events) != 1 || p.Events[0].Seq != 1 {
		t.Fatalf("events after commit: %+v", p.Events)
	}
	req.BeforeCommit = func() error { t.Error("BeforeCommit called for an already present asset"); return nil }
	mustCommit(t, s, req, meta.ResultAlreadyPresent)
}

func sha(n int) string { return fmt.Sprintf("%064x", n+1) }

func asset(n int, name string) meta.Asset {
	return meta.Asset{SHA256: sha(n), Filename: name, MimeType: "image/jpeg", Bytes: int64(100 + n), PreviewReady: n%2 == 0}
}

func mustFolder(t *testing.T, s Store, name string) meta.Folder {
	t.Helper()
	f, err := s.CreateFolder(context.Background(), meta.NewFolder{Name: name, OwnerID: s.OwnerID})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustCommit(t *testing.T, s Store, req meta.CommitRequest, want string) meta.CommitResult {
	t.Helper()
	res, err := s.CommitUpload(context.Background(), req)
	if err != nil {
		t.Fatalf("CommitUpload(%s, %s): %v", req.FolderID, req.Asset.SHA256, err)
	}
	if res.Result != want {
		t.Fatalf("CommitUpload(%s, %s) = %q, want %q", req.FolderID, req.Asset.SHA256, res.Result, want)
	}
	return res
}

func mustEvents(t *testing.T, s Store, folderID string, cursor int64, limit int) meta.EventPage {
	t.Helper()
	p, err := s.ListEvents(context.Background(), folderID, cursor, limit)
	if err != nil {
		t.Fatal(err)
	}
	if p.Events == nil || p.Assets == nil {
		t.Fatalf("ListEvents returned nil Events or Assets: %+v", p)
	}
	return p
}

func validTime(t *testing.T, what, ts string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("%s %q is not RFC 3339: %v", what, ts, err)
	}
}

func testFolders(t *testing.T, s Store) {
	ctx := context.Background()
	list, err := s.ListFolders(ctx)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty ListFolders = %#v, %v", list, err)
	}
	a := mustFolder(t, s, "family")
	b := mustFolder(t, s, "trip")
	if a.ID == "" || a.ID == b.ID || a.Name != "family" {
		t.Fatalf("folders %+v %+v", a, b)
	}
	validTime(t, "created_at", a.CreatedAt)
	got, err := s.GetFolder(ctx, a.ID)
	if err != nil || got != a {
		t.Fatalf("GetFolder = %+v, %v; want %+v", got, err, a)
	}
	if _, err := s.GetFolder(ctx, "missing"); !errors.Is(err, meta.ErrFolderNotFound) {
		t.Fatalf("GetFolder(missing) err = %v", err)
	}
	list, err = s.ListFolders(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListFolders = %v, %v", list, err)
	}
	seen := map[meta.Folder]bool{}
	for _, f := range list {
		seen[f] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("ListFolders = %v, want %v and %v", list, a, b)
	}
	// The owner sees both, with the owner relation; nobody else sees any.
	mine, err := s.ListFoldersFor(ctx, s.OwnerID)
	if err != nil || len(mine) != 2 || !seen[mine[0].Folder] || !seen[mine[1].Folder] || mine[0].Folder == mine[1].Folder ||
		mine[0].Access != (authz.AlbumAccess{OwnerID: s.OwnerID}) || mine[1].Access != mine[0].Access {
		t.Fatalf("ListFoldersFor(owner) = %+v, %v", mine, err)
	}
	if other, err := s.ListFoldersFor(ctx, "someone-else"); err != nil || other == nil || len(other) != 0 {
		t.Fatalf("ListFoldersFor(other) = %#v, %v", other, err)
	}
	fa, err := s.GetFolderAccess(ctx, a.ID, "someone-else")
	if err != nil || fa.Folder != a || fa.Access != (authz.AlbumAccess{OwnerID: s.OwnerID}) {
		t.Fatalf("GetFolderAccess(other) = %+v, %v", fa, err)
	}
	if _, err := s.GetFolderAccess(ctx, "missing", s.OwnerID); !errors.Is(err, meta.ErrFolderNotFound) {
		t.Fatalf("GetFolderAccess(missing) err = %v", err)
	}
}

func testCommitUpload(t *testing.T, s Store) {
	ctx := context.Background()
	if _, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: "missing", Asset: asset(0, "a.jpg")}); !errors.Is(err, meta.ErrFolderNotFound) {
		t.Fatalf("commit to missing folder err = %v", err)
	}
	if _, err := s.GetAssetBySHA(ctx, sha(0)); !errors.Is(err, meta.ErrAssetNotFound) {
		t.Fatalf("asset registered by failed commit: %v", err)
	}

	f1 := mustFolder(t, s, "one")
	f2 := mustFolder(t, s, "two")
	in := asset(0, "a.jpg")
	res := mustCommit(t, s, meta.CommitRequest{FolderID: f1.ID, DeviceID: "dev-1", UserID: s.OwnerID, Asset: in}, meta.ResultCreated)
	validTime(t, "uploaded_at", res.Asset.UploadedAt)
	stored := in
	stored.ID = res.Asset.ID
	stored.UploadedAt = res.Asset.UploadedAt
	if res.Asset != stored || stored.ID == "" || stored.ID == sha(0) {
		t.Fatalf("created asset %+v, want %+v with a random ID", res.Asset, stored)
	}
	if got, err := s.GetAssetBySHA(ctx, sha(0)); err != nil || got != stored {
		t.Fatalf("GetAssetBySHA = %+v, %v", got, err)
	}
	if got, err := s.GetFolderAsset(ctx, f1.ID, stored.ID); err != nil || got != stored {
		t.Fatalf("GetFolderAsset = %+v, %v", got, err)
	}
	// Not reachable through another folder (IDOR) or by its hash.
	for _, q := range [][2]string{{f2.ID, stored.ID}, {f1.ID, sha(0)}, {f1.ID, "missing"}} {
		if _, err := s.GetFolderAsset(ctx, q[0], q[1]); !errors.Is(err, meta.ErrAssetNotFound) {
			t.Fatalf("GetFolderAsset(%s, %s) err = %v", q[0], q[1], err)
		}
	}

	// Same folder again: nothing changes, the stored asset is returned.
	again := asset(0, "renamed.jpg")
	again.PreviewReady = false
	res = mustCommit(t, s, meta.CommitRequest{FolderID: f1.ID, DeviceID: "dev-2", Asset: again}, meta.ResultAlreadyPresent)
	if res.Asset != stored {
		t.Fatalf("already_present asset %+v, want %+v", res.Asset, stored)
	}
	// Another folder: same blob, but an asset of its own with its own name.
	res = mustCommit(t, s, meta.CommitRequest{FolderID: f2.ID, DeviceID: "dev-2", Asset: again}, meta.ResultLinked)
	if res.Asset.ID == stored.ID || res.Asset.SHA256 != sha(0) || res.Asset.Filename != "renamed.jpg" || res.Asset.Bytes != in.Bytes {
		t.Fatalf("linked asset %+v", res.Asset)
	}
	second := res.Asset
	if got, _ := s.GetAssetBySHA(ctx, sha(0)); got != stored {
		t.Fatalf("GetAssetBySHA after link: %+v, want the oldest %+v", got, stored)
	}
	if _, err := s.GetFolderAsset(ctx, f1.ID, second.ID); !errors.Is(err, meta.ErrAssetNotFound) {
		t.Fatalf("f2 asset visible in f1: %v", err)
	}

	p := mustEvents(t, s, f1.ID, 0, 10)
	if len(p.Events) != 1 {
		t.Fatalf("f1 events %+v", p.Events)
	}
	e := p.Events[0]
	if e.Seq != 1 || e.Kind != meta.EventAssetAdded || e.AssetID != stored.ID || e.FolderID != f1.ID || e.UploaderDeviceID != "dev-1" {
		t.Fatalf("event %+v", e)
	}
	if p.Assets[stored.ID] != stored || len(p.Assets) != 1 {
		t.Fatalf("f1 assets %+v", p.Assets)
	}
	validTime(t, "occurred_at", e.OccurredAt)
	p = mustEvents(t, s, f2.ID, 0, 10)
	if len(p.Events) != 1 || p.Events[0].Seq != 1 || p.Events[0].UploaderDeviceID != "dev-2" || p.Events[0].AssetID != second.ID {
		t.Fatalf("f2 events %+v", p.Events)
	}
}

func testListEvents(t *testing.T, s Store) {
	ctx := context.Background()
	if _, err := s.ListEvents(ctx, "missing", 0, 10); !errors.Is(err, meta.ErrFolderNotFound) {
		t.Fatalf("ListEvents(missing) err = %v", err)
	}
	f := mustFolder(t, s, "f")
	if _, err := s.ListEvents(ctx, f.ID, 0, 0); !errors.Is(err, meta.ErrInvalidLimit) {
		t.Fatalf("ListEvents(limit 0) err = %v", err)
	}
	p := mustEvents(t, s, f.ID, 0, 10)
	if len(p.Events) != 0 || len(p.Assets) != 0 || p.NextCursor != 0 || p.HasMore {
		t.Fatalf("empty page %+v", p)
	}
	for i := 0; i < 5; i++ {
		mustCommit(t, s, meta.CommitRequest{FolderID: f.ID, Asset: asset(i, fmt.Sprintf("%d.jpg", i))}, meta.ResultCreated)
	}
	p = mustEvents(t, s, f.ID, 0, 2)
	if len(p.Events) != 2 || p.Events[0].Seq != 1 || p.Events[1].Seq != 2 || p.NextCursor != 2 || !p.HasMore {
		t.Fatalf("page 1 %+v", p)
	}
	if len(p.Assets) != 2 || p.Assets[p.Events[0].AssetID].Filename != "0.jpg" || p.Assets[p.Events[1].AssetID].Filename != "1.jpg" {
		t.Fatalf("page 1 assets %+v", p.Assets)
	}
	p = mustEvents(t, s, f.ID, 2, 2)
	if len(p.Events) != 2 || p.Events[0].Seq != 3 || p.NextCursor != 4 || !p.HasMore {
		t.Fatalf("page 2 %+v", p)
	}
	p = mustEvents(t, s, f.ID, 4, 2)
	if len(p.Events) != 1 || p.Events[0].Seq != 5 || p.NextCursor != 5 || p.HasMore {
		t.Fatalf("page 3 %+v", p)
	}
	p = mustEvents(t, s, f.ID, 99, 2)
	if len(p.Events) != 0 || p.NextCursor != 99 || p.HasMore {
		t.Fatalf("past end %+v", p)
	}
}

// testConcurrentCommits commits distinct assets to one folder in parallel.
// Run it with -race: sequence numbers must be 1..n without gaps or repeats.
func testConcurrentCommits(t *testing.T, s Store) {
	f := mustFolder(t, s, "busy")
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.CommitUpload(context.Background(), meta.CommitRequest{FolderID: f.ID, Asset: asset(i, "x.jpg")})
			if err == nil && res.Result != meta.ResultCreated {
				err = fmt.Errorf("asset %d: result %q", i, res.Result)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p := mustEvents(t, s, f.ID, 0, 100)
	if len(p.Events) != n || p.HasMore {
		t.Fatalf("events %d, more %v", len(p.Events), p.HasMore)
	}
	ids := map[string]bool{}
	for i, e := range p.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
		ids[e.AssetID] = true
	}
	if len(ids) != n {
		t.Fatalf("distinct assets %d, want %d", len(ids), n)
	}
}
