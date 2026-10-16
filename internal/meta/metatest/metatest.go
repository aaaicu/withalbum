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

	"github.com/aaaicu/withalbum/internal/meta"
)

// Run checks the meta.MetadataStore contract. open must return a new, empty
// store each time it is called; Run closes it.
func Run(t *testing.T, open func(t *testing.T) meta.MetadataStore) {
	newStore := func(t *testing.T) meta.MetadataStore {
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
}

func sha(n int) string { return fmt.Sprintf("%064x", n+1) }

func asset(n int, name string) meta.Asset {
	return meta.Asset{ID: sha(n), Filename: name, MimeType: "image/jpeg", Bytes: int64(100 + n), PreviewReady: n%2 == 0}
}

func mustFolder(t *testing.T, s meta.MetadataStore, name string) meta.Folder {
	t.Helper()
	f, err := s.CreateFolder(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustCommit(t *testing.T, s meta.MetadataStore, req meta.CommitRequest, want string) meta.CommitResult {
	t.Helper()
	res, err := s.CommitUpload(context.Background(), req)
	if err != nil {
		t.Fatalf("CommitUpload(%s, %s): %v", req.FolderID, req.Asset.ID, err)
	}
	if res.Result != want {
		t.Fatalf("CommitUpload(%s, %s) = %q, want %q", req.FolderID, req.Asset.ID, res.Result, want)
	}
	return res
}

func mustEvents(t *testing.T, s meta.MetadataStore, folderID string, cursor int64, limit int) meta.EventPage {
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

func testFolders(t *testing.T, s meta.MetadataStore) {
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
}

func testCommitUpload(t *testing.T, s meta.MetadataStore) {
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
	res := mustCommit(t, s, meta.CommitRequest{FolderID: f1.ID, DeviceID: "dev-1", Asset: in}, meta.ResultCreated)
	validTime(t, "uploaded_at", res.Asset.UploadedAt)
	stored := in
	stored.UploadedAt = res.Asset.UploadedAt
	if res.Asset != stored {
		t.Fatalf("created asset %+v, want %+v", res.Asset, stored)
	}
	if got, err := s.GetAssetBySHA(ctx, sha(0)); err != nil || got != stored {
		t.Fatalf("GetAssetBySHA = %+v, %v", got, err)
	}

	// Same folder again: nothing changes, the stored asset is returned.
	again := asset(0, "renamed.jpg")
	again.PreviewReady = false
	res = mustCommit(t, s, meta.CommitRequest{FolderID: f1.ID, DeviceID: "dev-2", Asset: again}, meta.ResultAlreadyPresent)
	if res.Asset != stored {
		t.Fatalf("already_present asset %+v, want %+v", res.Asset, stored)
	}
	// Another folder: linked to the existing asset, which is not overwritten.
	res = mustCommit(t, s, meta.CommitRequest{FolderID: f2.ID, DeviceID: "dev-2", Asset: again}, meta.ResultLinked)
	if res.Asset != stored {
		t.Fatalf("linked asset %+v, want %+v", res.Asset, stored)
	}
	if got, _ := s.GetAssetBySHA(ctx, sha(0)); got != stored {
		t.Fatalf("asset overwritten: %+v", got)
	}

	p := mustEvents(t, s, f1.ID, 0, 10)
	if len(p.Events) != 1 {
		t.Fatalf("f1 events %+v", p.Events)
	}
	e := p.Events[0]
	if e.Seq != 1 || e.Kind != meta.EventAssetAdded || e.AssetID != sha(0) || e.FolderID != f1.ID || e.UploaderDeviceID != "dev-1" {
		t.Fatalf("event %+v", e)
	}
	validTime(t, "occurred_at", e.OccurredAt)
	p = mustEvents(t, s, f2.ID, 0, 10)
	if len(p.Events) != 1 || p.Events[0].Seq != 1 || p.Events[0].UploaderDeviceID != "dev-2" {
		t.Fatalf("f2 events %+v", p.Events)
	}
}

func testListEvents(t *testing.T, s meta.MetadataStore) {
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
	if len(p.Assets) != 2 || p.Assets[sha(0)].Filename != "0.jpg" || p.Assets[sha(1)].Filename != "1.jpg" {
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
func testConcurrentCommits(t *testing.T, s meta.MetadataStore) {
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
