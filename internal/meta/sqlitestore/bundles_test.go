package sqlitestore

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

func (e *uploadEnv) bundle(device, key string, still, video int) meta.NewBundle {
	return meta.NewBundle{UserID: e.owner, DeviceID: device, FolderID: e.folder, Key: key, Kind: meta.BundleLivePhoto, DeviceLabel: "phone",
		Authorize: allow(authz.UploadAsset), Reserve: noReserve,
		Components: []meta.NewBundleComponent{{Role: meta.RoleStill, SHA256: sha(still), Bytes: 100, Filename: "a.heic"},
			{Role: meta.RolePairedVideo, SHA256: sha(video), Bytes: 200, Filename: "a.mov"}}}
}

func (e *uploadEnv) commitBundle(t *testing.T, b meta.Bundle) (meta.CommitResult, error) {
	t.Helper()
	for _, c := range b.Components {
		if _, err := e.s.db.Exec("INSERT INTO blobs (sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, ?, 'x', ?, 'v') ON CONFLICT DO NOTHING",
			c.SHA256, c.TotalBytes, "p/"+c.SHA256); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := b.Component(meta.RoleStill)
	v, _ := b.Component(meta.RolePairedVideo)
	return e.s.CommitBundle(t.Context(), meta.BundleCommit{Ref: meta.BundleRef{ID: b.ID, UserID: e.owner, DeviceID: b.DeviceID, FolderID: e.folder},
		DeviceLabel: "phone", Pairing: meta.PairingVerified, Authorize: allow(authz.UploadAsset),
		Still: meta.Asset{SHA256: s.SHA256, Filename: s.Filename, MimeType: "image/heic", Bytes: s.TotalBytes},
		Video: meta.Component{Role: meta.RolePairedVideo, SHA256: v.SHA256, Filename: v.Filename, MimeType: "video/quicktime", Bytes: v.TotalBytes}})
}

// TestBundleKeysAndLimits: a bundle key and a single upload key never mix,
// the open session limit counts both components, a deleted bundle takes
// its sessions with it.
func TestBundleKeysAndLimits(t *testing.T) {
	e := newUploadEnv(t)
	ctx := t.Context()
	b, created, _, err := e.s.CreateBundle(ctx, e.bundle("d1", "k1", 1, 2))
	if err != nil || !created || len(b.Components) != 2 || b.Components[0].Role != meta.RoleStill || b.Components[1].BundleID != b.ID {
		t.Fatalf("create: %+v %v %v", b, created, err)
	}
	if _, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "k1", sha(1), 100)); !errors.Is(err, meta.ErrIdempotencyMismatch) {
		t.Fatalf("single session with a bundle key: %v", err)
	}
	if _, _, _, err := e.s.CreateUploadSession(ctx, e.req("d1", "single", sha(3), 100)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.s.CreateBundle(ctx, e.bundle("d1", "single", 3, 4)); !errors.Is(err, meta.ErrIdempotencyMismatch) {
		t.Fatalf("bundle with a session key: %v", err)
	}
	bad := e.bundle("d1", "bad", 5, 6)
	bad.Components[1].Role = meta.RoleStill
	if _, _, _, err := e.s.CreateBundle(ctx, bad); err == nil {
		t.Fatal("two stills accepted")
	}
	// 3 open sessions now; 6 more bundles fill 15, the next needs 2.
	for i := range 6 {
		if _, _, _, err := e.s.CreateBundle(ctx, e.bundle("d1", fmt.Sprint("fill", i), 10+i, 20+i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := e.s.CreateBundle(ctx, e.bundle("d1", "full", 30, 31)); !errors.Is(err, meta.ErrTooManyUploads) {
		t.Fatalf("over the limit: %v", err)
	}
	ids, err := e.s.DeleteBundle(ctx, meta.BundleRef{ID: b.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder})
	if err != nil || len(ids) != 2 {
		t.Fatalf("delete: %v %v", ids, err)
	}
	if _, err := e.s.GetBundle(ctx, meta.BundleRef{ID: b.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}); !errors.Is(err, meta.ErrBundleNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := e.s.DeleteBundle(ctx, meta.BundleRef{ID: b.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}); !errors.Is(err, meta.ErrBundleNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	// Other devices and accounts do not see a bundle.
	b2, _, _, err := e.s.CreateBundle(ctx, e.bundle("d2", "k2", 40, 41))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []meta.BundleRef{{ID: b2.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder},
		{ID: b2.ID, UserID: e.other, DeviceID: "d2", FolderID: e.folder}, {ID: b2.ID, UserID: e.owner, DeviceID: "d2", FolderID: e.folder2}} {
		if _, err := e.s.GetBundle(ctx, ref); !errors.Is(err, meta.ErrBundleNotFound) {
			t.Fatalf("%+v: %v", ref, err)
		}
	}
}

// TestBundleSharedAssetConflict: a still-only asset that another folder
// also holds (legacy import) is not changed by a bundle in one folder.
func TestBundleSharedAssetConflict(t *testing.T) {
	e := newUploadEnv(t)
	ctx := t.Context()
	if _, err := e.s.db.Exec("INSERT INTO blobs (sha256, bytes, mime_type, relative_path, verified_at) VALUES (?, 100, 'image/heic', 'p/x', 'v')", sha(1)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO assets (asset_id, original_sha256, original_filename, media_kind) VALUES ('shared', ?, 'a.heic', 'image')", []any{sha(1)}},
		{"INSERT INTO folder_assets (folder_id, asset_id) VALUES (?, 'shared')", []any{e.folder}},
		{"INSERT INTO folder_assets (folder_id, asset_id) VALUES (?, 'shared')", []any{e.folder2}},
	} {
		if _, err := e.s.db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	b, _, _, err := e.s.CreateBundle(ctx, e.bundle("d1", "k", 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.commitBundle(t, b); !errors.Is(err, meta.ErrComponentConflict) {
		t.Fatalf("shared asset: %v", err)
	}
	var kind string
	if err := e.s.db.QueryRow("SELECT media_kind FROM assets WHERE asset_id = 'shared'").Scan(&kind); err != nil || kind != "image" {
		t.Fatalf("shared asset changed: %q %v", kind, err)
	}
	// Still open: the client can cancel or retry.
	if got, err := e.s.GetBundle(ctx, meta.BundleRef{ID: b.ID, UserID: e.owner, DeviceID: "d1", FolderID: e.folder}); err != nil || got.Status != meta.UploadOpen {
		t.Fatalf("bundle after conflict: %+v %v", got, err)
	}
}
