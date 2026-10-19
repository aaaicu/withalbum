package sqlitestore

import (
	"errors"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// TestCaptureTimes: pending originals, one asset_updated per folder link
// still in place when a capture time is found, none when it is not, and
// settled originals are not touched again.
func TestCaptureTimes(t *testing.T) {
	ctx := t.Context()
	s := open(t, newVolume(t))
	defer s.Close()
	owner := addUser(t, s, "owner", "USER")
	folder := func(name string) string {
		f, err := s.CreateFolder(ctx, meta.NewFolder{Name: name, OwnerID: owner})
		if err != nil {
			t.Fatal(err)
		}
		return f.ID
	}
	f1, f2, f3 := folder("one"), folder("two"), folder("three")
	commit := func(fid string, n int) meta.Asset {
		res, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: fid, UserID: owner, DeviceID: "phone",
			Asset: meta.Asset{SHA256: sha(n), Filename: "p.jpg", MimeType: "image/jpeg", Bytes: 10}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Asset
	}
	a1 := commit(f1, 0)
	commit(f2, 0)
	removed := commit(f3, 0)
	commit(f1, 1)
	if err := s.RemoveAsset(ctx, meta.AlbumChange{FolderID: f3, ActorID: owner, TargetID: removed.ID,
		Authorize: func(authz.Principal, authz.AlbumAccess) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if a1.CapturedAt != "" || a1.UploaderName != "owner" {
		t.Fatalf("committed asset %+v", a1)
	}
	if got, err := s.PendingCaptures(ctx, 10); err != nil || len(got) != 2 || got[0] != sha(0) || got[1] != sha(1) {
		t.Fatalf("PendingCaptures = %v, %v", got, err)
	}
	if _, err := s.PendingCaptures(ctx, 0); !errors.Is(err, meta.ErrInvalidLimit) {
		t.Fatalf("limit 0: %v", err)
	}
	if _, err := s.SetCaptureTimes(ctx, []meta.CaptureResult{{SHA256: sha(0), CapturedUTC: "2026-10-19T05:03:22Z"}}); err == nil {
		t.Fatal("an instant without a capture time was accepted")
	}
	events := func(fid string) int {
		return count(t, s, "SELECT count(*) FROM folder_changes WHERE folder_id = ? AND event_type = 'asset_updated'", fid)
	}
	n, err := s.SetCaptureTimes(ctx, []meta.CaptureResult{
		{SHA256: sha(0), CapturedAt: "2026-10-19T14:03:22+09:00", CapturedUTC: "2026-10-19T05:03:22Z"},
		{SHA256: sha(1)}, // no capture time
	})
	if err != nil || n != 2 {
		t.Fatalf("SetCaptureTimes = %d, %v; want 2 events (f1 and f2, not the removed link)", n, err)
	}
	if events(f1) != 1 || events(f2) != 1 || events(f3) != 0 {
		t.Fatalf("asset_updated events %d %d %d", events(f1), events(f2), events(f3))
	}
	if got, _ := s.PendingCaptures(ctx, 10); len(got) != 0 {
		t.Fatalf("still pending %v", got)
	}
	got, err := s.GetFolderAsset(ctx, f1, a1.ID)
	if err != nil || got.CapturedAt != "2026-10-19T14:03:22+09:00" {
		t.Fatalf("asset %+v, %v", got, err)
	}
	page, err := s.ListEvents(ctx, f1, 0, 100)
	if err != nil || page.Assets[a1.ID].CapturedAt != "2026-10-19T14:03:22+09:00" || page.Assets[a1.ID].UploaderName != "owner" {
		t.Fatalf("feed asset %+v, %v", page.Assets[a1.ID], err)
	}
	last := page.Events[len(page.Events)-1]
	if last.Kind != meta.EventAssetUpdated || last.AssetID != a1.ID || last.UploaderDeviceID != "phone" {
		t.Fatalf("last event %+v", last)
	}
	// Settled originals are left alone.
	if n, err := s.SetCaptureTimes(ctx, []meta.CaptureResult{{SHA256: sha(1), CapturedAt: "2020-01-01T00:00:00"}}); err != nil || n != 0 {
		t.Fatalf("second SetCaptureTimes = %d, %v", n, err)
	}
	if got, _ := s.GetFolderAsset(ctx, f1, a1.ID); got.CapturedAt != "2026-10-19T14:03:22+09:00" {
		t.Fatalf("changed: %+v", got)
	}
	// A new upload of known bytes in another folder has the capture time
	// at once (it belongs to the bytes).
	f4 := folder("four")
	if a := commit(f4, 0); a.CapturedAt != "2026-10-19T14:03:22+09:00" {
		t.Fatalf("known bytes in a new folder: %+v", a)
	}
}

// TestUploaderNameAndMemberCount: names only for accounts that can log in;
// the count is the owner plus members whose accounts can log in.
func TestUploaderNameAndMemberCount(t *testing.T) {
	ctx := t.Context()
	s := open(t, newVolume(t))
	defer s.Close()
	owner := addUser(t, s, "owner", "USER")
	member := addUser(t, s, "member", "USER")
	locked := addUser(t, s, "locked", "USER")
	f, err := s.CreateFolder(ctx, meta.NewFolder{Name: "f", OwnerID: owner})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{member, locked} {
		if _, err := s.db.Exec("INSERT INTO folder_members (folder_id, user_id, role) VALUES (?, ?, 'CONTRIBUTOR')", f.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	countFor := func(user string) int {
		fa, err := s.GetFolderAccess(ctx, f.ID, user)
		if err != nil {
			t.Fatal(err)
		}
		return fa.MemberCount
	}
	if n := countFor(member); n != 3 {
		t.Fatalf("member count %d, want 3", n)
	}
	res, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID, UserID: member,
		Asset: meta.Asset{SHA256: sha(0), Filename: "p.jpg", MimeType: "image/jpeg", Bytes: 10}})
	if err != nil || res.Asset.UploaderName != "member" {
		t.Fatalf("commit %+v, %v", res.Asset, err)
	}
	noUser, err := s.CommitUpload(ctx, meta.CommitRequest{FolderID: f.ID,
		Asset: meta.Asset{SHA256: sha(1), Filename: "q.jpg", MimeType: "image/jpeg", Bytes: 10}})
	if err != nil || noUser.Asset.UploaderName != "" {
		t.Fatalf("commit without account %+v, %v", noUser.Asset, err)
	}
	if _, err := s.db.Exec("UPDATE users SET password_hash = '!locked' WHERE user_id = ?", locked); err != nil {
		t.Fatal(err)
	}
	if n := countFor(owner); n != 2 {
		t.Fatalf("member count with a locked account %d, want 2", n)
	}
	if _, err := s.db.Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", member); err != nil {
		t.Fatal(err)
	}
	if n := countFor(owner); n != 1 {
		t.Fatalf("member count %d, want 1", n)
	}
	if a, err := s.GetFolderAsset(ctx, f.ID, res.Asset.ID); err != nil || a.UploaderName != "" {
		t.Fatalf("asset of a disabled uploader %+v, %v", a, err)
	}
	list, err := s.ListFoldersFor(ctx, owner)
	if err != nil || len(list) != 1 || list[0].MemberCount != 1 {
		t.Fatalf("ListFoldersFor %+v, %v", list, err)
	}
}
