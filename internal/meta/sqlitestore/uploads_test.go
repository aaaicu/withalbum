package sqlitestore

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

type uploadEnv struct {
	s               *Store
	owner, other    string
	folder, folder2 string
}

func newUploadEnv(t *testing.T) *uploadEnv {
	t.Helper()
	s := open(t, newVolume(t))
	t.Cleanup(func() { s.Close() })
	e := &uploadEnv{s: s, owner: addUser(t, s, "owner", "USER"), other: addUser(t, s, "other", "USER")}
	for _, d := range []struct{ id, user string }{{"d1", e.owner}, {"d2", e.owner}, {"d3", e.other}} {
		if _, err := s.db.Exec("INSERT INTO devices (device_id, user_id, platform, device_name) VALUES (?, ?, 'ios', 'phone')", d.id, d.user); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []*string{&e.folder, &e.folder2} {
		folder, err := s.CreateFolder(t.Context(), meta.NewFolder{Name: "f", OwnerID: e.owner})
		if err != nil {
			t.Fatal(err)
		}
		*f = folder.ID
	}
	return e
}

func noReserve(int64) error { return nil }

func (e *uploadEnv) req(device, key, sum string, size int64) meta.NewUploadSession {
	return meta.NewUploadSession{UserID: e.owner, DeviceID: device, FolderID: e.folder, Key: key, SHA256: sum, TotalBytes: size,
		Filename: "clip.mov", DeviceLabel: "phone", Authorize: allow(authz.UploadAsset), Reserve: noReserve}
}

func TestUploadSessionLifecycle(t *testing.T) {
	e := newUploadEnv(t)
	ctx := t.Context()
	us, created, replaced, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 1000))
	if err != nil || !created || replaced != "" || len(us.ID) != 32 || us.Status != meta.UploadOpen || us.Received != 0 {
		t.Fatalf("create: %+v %v %q %v", us, created, replaced, err)
	}
	ref := meta.UploadRef{ID: us.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}
	// The same request again: the same session.
	again, created, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 1000))
	if err != nil || created || again.ID != us.ID {
		t.Fatalf("retried create: %+v %v %v", again, created, err)
	}
	// The same key for other bytes.
	for _, r := range []meta.NewUploadSession{e.req("d1", "k1", sha(2), 1000), e.req("d1", "k1", sha(1), 999)} {
		if _, _, _, err := e.s.CreateUploadSession(ctx, r); !errors.Is(err, meta.ErrIdempotencyMismatch) {
			t.Fatalf("other bytes under the key: %v", err)
		}
	}
	// The key is per device: another device starts its own session.
	if other, created, _, err := e.s.CreateUploadSession(ctx, e.req("d2", "k1", sha(1), 1000)); err != nil || !created || other.ID == us.ID {
		t.Fatalf("other device: %+v %v %v", other, created, err)
	}
	// Only its account, device and folder see it.
	for _, bad := range []meta.UploadRef{
		{ID: us.ID, UserID: e.other, DeviceID: "d1", FolderID: e.folder},
		{ID: us.ID, UserID: e.owner, DeviceID: "d2", FolderID: e.folder},
		{ID: us.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder2},
	} {
		if _, err := e.s.GetUploadSession(ctx, bad); !errors.Is(err, meta.ErrUploadNotFound) {
			t.Fatalf("visible to %+v: %v", bad, err)
		}
		if err := e.s.AdvanceUploadSession(ctx, bad, 0, 10); !errors.Is(err, meta.ErrUploadNotFound) {
			t.Fatalf("writable by %+v: %v", bad, err)
		}
		if err := e.s.DeleteUploadSession(ctx, bad); !errors.Is(err, meta.ErrUploadNotFound) {
			t.Fatalf("deletable by %+v: %v", bad, err)
		}
	}
	if err := e.s.AdvanceUploadSession(ctx, ref, 0, 400); err != nil {
		t.Fatal(err)
	}
	// A stale offset or one past the end.
	if err := e.s.AdvanceUploadSession(ctx, ref, 0, 500); !errors.Is(err, meta.ErrUploadConflict) {
		t.Fatalf("stale offset: %v", err)
	}
	if err := e.s.AdvanceUploadSession(ctx, ref, 400, 1001); !errors.Is(err, meta.ErrUploadConflict) {
		t.Fatalf("past the end: %v", err)
	}
	got, err := e.s.GetUploadSession(ctx, ref)
	if err != nil || got.Received != 400 {
		t.Fatalf("get: %+v %v", got, err)
	}
	// Reservations: what open sessions still expect.
	if n, err := e.s.ReservedUploadBytes(ctx, ""); err != nil || n != 600+1000 {
		t.Fatalf("reserved = %d %v", n, err)
	}
	if n, err := e.s.ReservedUploadBytes(ctx, us.ID); err != nil || n != 1000 {
		t.Fatalf("reserved except = %d %v", n, err)
	}
	var seen int64 = -1
	r := e.req("d1", "k2", sha(3), 50)
	r.Reserve = func(reserved int64) error { seen = reserved; return errNotAllowed }
	if _, _, _, err := e.s.CreateUploadSession(ctx, r); !errors.Is(err, errNotAllowed) || seen != 1600 {
		t.Fatalf("refused reservation: %v (saw %d)", err, seen)
	}
	// Authorization inside the transaction.
	r = e.req("d1", "k3", sha(3), 50)
	r.UserID = e.other
	r.DeviceID = "d3"
	if _, _, _, err := e.s.CreateUploadSession(ctx, r); !errors.Is(err, errNotAllowed) {
		t.Fatalf("non-member created a session: %v", err)
	}
	if n := count(t, e.s, "SELECT COUNT(*) FROM upload_sessions"); n != 2 {
		t.Fatalf("%d sessions", n)
	}
	if err := e.s.DeleteUploadSession(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetUploadSession(ctx, ref); !errors.Is(err, meta.ErrUploadNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestUploadSessionExpiryLimitAndPrune(t *testing.T) {
	e := newUploadEnv(t)
	ctx := t.Context()
	us, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 1000))
	if err != nil {
		t.Fatal(err)
	}
	ref := meta.UploadRef{ID: us.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}
	past := time.Now().Add(-time.Minute).UTC().Format(timeLayout)
	if _, err := e.s.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE upload_id = ?", past, us.ID); err != nil {
		t.Fatal(err)
	}
	// Expired: invisible, reserves nothing, and its key starts over.
	if _, err := e.s.GetUploadSession(ctx, ref); !errors.Is(err, meta.ErrUploadNotFound) {
		t.Fatalf("expired session visible: %v", err)
	}
	if n, _ := e.s.ReservedUploadBytes(ctx, ""); n != 0 {
		t.Fatalf("expired session reserves %d", n)
	}
	fresh, created, replaced, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 1000))
	if err != nil || !created || replaced != us.ID || fresh.ID == us.ID {
		t.Fatalf("recreate after expiry: %+v %v %q %v", fresh, created, replaced, err)
	}
	// At most MaxOpenUploads open sessions per account.
	for i := 1; i < meta.MaxOpenUploads; i++ {
		if _, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", fmt.Sprintf("n%d", i), sha(i), 10)); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "one-more", sha(99), 10)); !errors.Is(err, meta.ErrTooManyUploads) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := e.s.db.Exec("UPDATE upload_sessions SET expires_at = ? WHERE upload_id = ?", past, fresh.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := e.s.PruneUploadSessions(ctx, time.Now())
	if err != nil || len(ids) != 1 || ids[0] != fresh.ID {
		t.Fatalf("prune = %v %v", ids, err)
	}
	all, err := e.s.ListUploadSessions(ctx)
	if err != nil || len(all) != meta.MaxOpenUploads-1 {
		t.Fatalf("list = %d %v", len(all), err)
	}
}

// TestCommitCompletesUploadSession: the commit of a session's bytes marks
// it completed in the same transaction, for new and known bytes; a session
// that is not open makes the commit write nothing.
func TestCommitCompletesUploadSession(t *testing.T) {
	e := newUploadEnv(t)
	ctx := t.Context()
	commit := func(sessionID, key, sum string) (meta.CommitResult, error) {
		return e.s.CommitUpload(ctx, meta.CommitRequest{FolderID: e.folder, DeviceID: "phone", UserID: e.owner,
			Asset:       meta.Asset{SHA256: sum, Filename: "clip.mov", MimeType: "video/quicktime", Bytes: 1000, PreviewStatus: meta.PreviewStatusPending},
			Idempotency: &meta.Idempotency{DeviceID: "d1", Key: key}, UploadSessionID: sessionID, Authorize: allow(authz.UploadAsset)})
	}
	us, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 1000))
	if err != nil {
		t.Fatal(err)
	}
	res, err := commit(us.ID, "k1", sha(1))
	if err != nil || res.Result != meta.ResultCreated || res.Asset.PreviewStatus != meta.PreviewStatusPending || res.Asset.PreviewReady {
		t.Fatalf("commit: %+v %v", res, err)
	}
	ref := meta.UploadRef{ID: us.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}
	got, err := e.s.GetUploadSession(ctx, ref)
	if err != nil || got.Status != meta.UploadCompleted || got.Received != 1000 {
		t.Fatalf("session after commit: %+v %v", got, err)
	}
	// Committing it again is a replay (receipt), not a second asset.
	if res, err := commit("", "k1", sha(1)); err != nil || !res.Replayed {
		t.Fatalf("replay: %+v %v", res, err)
	}
	// The key is now committed: a new session under it is refused.
	if err := e.s.DeleteUploadSession(ctx, ref); err != nil {
		t.Fatal(err)
	}
	var committed *meta.UploadCommittedError
	if _, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 1000)); !errors.Is(err, meta.ErrUploadCommitted) || !errors.As(err, &committed) {
		t.Fatalf("session for a committed key: %v", err)
	}
	if r := committed.Result; !r.Replayed || r.Result != meta.ResultCreated || r.Asset.ID != res.Asset.ID || r.Asset.SHA256 != sha(1) {
		t.Fatalf("stored result of a committed key: %+v", r)
	}
	if _, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(2), 1000)); !errors.Is(err, meta.ErrIdempotencyMismatch) {
		t.Fatalf("session for a committed key, other bytes: %v", err)
	}
	// Known bytes (already present): the session completes too.
	us2, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k2", sha(1), 1000))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := commit(us2.ID, "k2", sha(1)); err != nil || res.Result != meta.ResultAlreadyPresent {
		t.Fatalf("known bytes: %+v %v", res, err)
	}
	if got, err := e.s.GetUploadSession(ctx, meta.UploadRef{ID: us2.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}); err != nil || got.Status != meta.UploadCompleted {
		t.Fatalf("known bytes session: %+v %v", got, err)
	}
	// A cancelled session: nothing is committed.
	us3, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k3", sha(3), 1000))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteUploadSession(ctx, meta.UploadRef{ID: us3.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}); err != nil {
		t.Fatal(err)
	}
	before := count(t, e.s, "SELECT COUNT(*) FROM assets")
	if _, err := commit(us3.ID, "k3", sha(3)); !errors.Is(err, meta.ErrUploadNotFound) {
		t.Fatalf("commit of a cancelled session: %v", err)
	}
	if n := count(t, e.s, "SELECT COUNT(*) FROM assets"); n != before || count(t, e.s, "SELECT COUNT(*) FROM upload_receipts WHERE idempotency_key = 'k3'") != 0 {
		t.Fatal("a cancelled session's commit wrote rows")
	}
}

// TestPreviewStatusUpdatesEveryFolder: a pending preview settled by the
// worker changes every asset of those bytes and appends asset_updated to
// each folder holding one.
func TestPreviewStatusUpdatesEveryFolder(t *testing.T) {
	e := newUploadEnv(t)
	ctx := t.Context()
	device := map[string]string{e.folder: "phone-a", e.folder2: "phone-b"}
	for i, f := range []string{e.folder, e.folder2} {
		if _, err := e.s.CommitUpload(ctx, meta.CommitRequest{FolderID: f, UserID: e.owner, DeviceID: device[f],
			Asset: meta.Asset{SHA256: sha(7), Filename: "a.heic", MimeType: "image/heic", Bytes: 10, PreviewStatus: meta.PreviewStatusPending}}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if _, err := e.s.CommitUpload(ctx, meta.CommitRequest{FolderID: e.folder, UserID: e.owner,
		Asset: meta.Asset{SHA256: sha(8), Filename: "b.bin", MimeType: "application/octet-stream", Bytes: 10, PreviewStatus: meta.PreviewStatusUnsupported}}); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.s.PendingPreviews(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0] != (meta.PreviewJob{SHA256: sha(7), MimeType: "image/heic"}) {
		t.Fatalf("pending = %v %v", jobs, err)
	}
	if _, err := e.s.SetPreviewStatus(ctx, sha(7), meta.PreviewStatusPending); err == nil {
		t.Fatal("set back to pending")
	}
	n, err := e.s.SetPreviewStatus(ctx, sha(7), meta.PreviewStatusReady)
	if err != nil || n != 2 {
		t.Fatalf("set ready: %d %v", n, err)
	}
	for _, f := range []string{e.folder, e.folder2} {
		page, err := e.s.ListEvents(ctx, f, 0, 10, meta.ViewAll)
		if err != nil {
			t.Fatal(err)
		}
		last := page.Events[len(page.Events)-1]
		a := page.Assets[last.AssetID]
		// The event names the device that added the asset to this folder.
		if last.Kind != meta.EventAssetUpdated || !a.PreviewReady || a.PreviewStatus != meta.PreviewStatusReady || last.UploaderDeviceID != device[f] {
			t.Fatalf("folder %s: last event %+v asset %+v", f, last, a)
		}
	}
	// Settled: nothing pending, a second call changes nothing.
	if jobs, _ := e.s.PendingPreviews(ctx, 10); len(jobs) != 0 {
		t.Fatalf("still pending: %v", jobs)
	}
	if n, err := e.s.SetPreviewStatus(ctx, sha(7), meta.PreviewStatusFailed); err != nil || n != 0 {
		t.Fatalf("second settle: %d %v", n, err)
	}
	as, err := e.s.GetAssetBySHA(ctx, sha(8))
	if err != nil || as.PreviewStatus != meta.PreviewStatusUnsupported || as.PreviewReady {
		t.Fatalf("unsupported asset: %+v %v", as, err)
	}
}
