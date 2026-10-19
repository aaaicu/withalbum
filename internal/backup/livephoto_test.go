//go:build darwin || linux

package backup

import (
	"bytes"
	"os"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/integrity"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

// addLivePhoto commits a Live Photo (still + paired video) through an
// upload bundle, as the HTTP API does, and leaves a second bundle open.
func (l *library) addLivePhoto(t *testing.T) (still, video []byte) {
	t.Helper()
	ctx := t.Context()
	owner := l.store.BootstrapOwnerID()
	if _, err := l.store.DB().Exec("INSERT INTO devices (device_id, user_id, device_name, platform) VALUES ('dev', ?, 'phone', 'ios')", owner); err != nil {
		t.Fatal(err)
	}
	still, video = lpt.HEIC("ID-1", 1), lpt.MOV("ID-1", 1)
	allow := func(authz.Principal, authz.AlbumAccess) error { return nil }
	newBundle := func(key string, s, v []byte) meta.Bundle {
		b, _, _, err := l.store.CreateBundle(ctx, meta.NewBundle{UserID: owner, DeviceID: "dev", FolderID: l.folder, Key: key,
			Kind: meta.BundleLivePhoto, DeviceLabel: "phone", Authorize: allow, Reserve: func(int64) error { return nil },
			Components: []meta.NewBundleComponent{{Role: meta.RoleStill, SHA256: shaOf(s), Bytes: int64(len(s)), Filename: "IMG_1.HEIC"},
				{Role: meta.RolePairedVideo, SHA256: shaOf(v), Bytes: int64(len(v)), Filename: "IMG_1.MOV"}}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := newBundle("live-1", still, video)
	for _, body := range [][]byte{still, video} {
		staged, err := l.blobs.Stage(bytes.NewReader(body), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.blobs.Promote(staged, staged.SHA256); err != nil {
			t.Fatal(err)
		}
		staged.Remove()
		l.files[staged.SHA256] = body
	}
	if _, err := l.store.CommitBundle(ctx, meta.BundleCommit{Ref: meta.BundleRef{ID: b.ID, UserID: owner, DeviceID: "dev", FolderID: l.folder},
		DeviceLabel: "phone", Pairing: meta.PairingVerified, PairingID: "ID-1", Authorize: allow,
		Still: meta.Asset{SHA256: shaOf(still), Filename: "IMG_1.HEIC", MimeType: "image/heic", Bytes: int64(len(still)), PreviewStatus: meta.PreviewStatusUnsupported},
		Video: meta.Component{Role: meta.RolePairedVideo, SHA256: shaOf(video), Filename: "IMG_1.MOV", MimeType: "video/quicktime", Bytes: int64(len(video))}}); err != nil {
		t.Fatal(err)
	}
	newBundle("live-open", lpt.HEIC("ID-2", 2), lpt.MOV("ID-2", 2))
	return still, video
}

// TestBackupRestoreLivePhoto: both originals of a Live Photo are in the
// backup and come back byte for byte with the asset's component; open
// bundles (whose partial files are not backed up) are dropped.
func TestBackupRestoreLivePhoto(t *testing.T) {
	l := newLibrary(t, 1)
	still, video := l.addLivePhoto(t)
	r := l.backup(t, newTarget(t), Options{})
	root, rr := restoreAndCompare(t, l, r.Path)
	if rr.Blobs != 3 {
		t.Fatalf("restored %d originals, want 3 (photo, still, video)", rr.Blobs)
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
	page, err := st.ListFolderAssets(t.Context(), l.folder, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range page.Assets {
		if a.MediaKind != meta.MediaLivePhoto {
			continue
		}
		found = true
		if a.SHA256 != shaOf(still) || a.PairedVideo.SHA256 != shaOf(video) || a.Pairing != meta.PairingVerified {
			t.Fatalf("restored live photo %+v", a)
		}
		for sha, want := range map[string][]byte{a.SHA256: still, a.PairedVideo.SHA256: video} {
			got, err := os.ReadFile(root + "/.system/pool/" + sha[:2] + "/" + sha)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("restored original %s: %v", sha, err)
			}
		}
	}
	if !found {
		t.Fatal("live photo not restored")
	}
	var n int
	if err := st.DB().QueryRow("SELECT (SELECT count(*) FROM upload_bundles) + (SELECT count(*) FROM upload_sessions)").Scan(&n); err != nil || n != 0 {
		t.Fatalf("open bundles survived the restore: %d, %v", n, err)
	}
	vr, err := integrity.Run(t.Context(), vol, st.DB(), integrity.Options{Hash: true})
	if err != nil || !vr.Healthy() || vr.Blobs != 3 || vr.LivePhotosWithoutVideo != 0 {
		t.Fatalf("verify restored: %v %+v", err, vr)
	}
}
