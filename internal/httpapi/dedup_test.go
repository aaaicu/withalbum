package httpapi_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/aaaicu/withalbum/internal/blobstore"
)

// poolUsage is what the pool holds on disk: regular files (hard links of
// one file counted once, so the short-lived second names Promote may leave
// do not count) and the disk blocks they take.
type poolUsage struct {
	files int
	bytes int64 // allocated blocks * 512
}

func diskUsage(t *testing.T, dir string) poolUsage {
	t.Helper()
	var u poolUsage
	seen := map[uint64]bool{}
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		if seen[uint64(st.Ino)] {
			return nil
		}
		seen[uint64(st.Ino)] = true
		u.files++
		u.bytes += int64(st.Blocks) * 512
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// regularFiles lists the regular files under dir (relative paths).
func regularFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSameBytesStoredOnce: the same original sent again and again (to the
// same album, to another album, by another account, through an upload
// session, as an UPLOADER, inside a Live Photo) is one pool file and one
// blobs row; only the album links differ. The disk usage of the pool does
// not grow after the first copy, and nothing is left in the staging areas.
func TestSameBytesStoredOnce(t *testing.T) {
	e := newACLEnv(t)
	pool := filepath.Join(e.Root, ".system", "pool")
	body := randomBytes(t, 3<<20+123) // a few disk blocks, not a multiple of any block size
	sha := shaHex(body)
	family := "/api/v1/folders/" + e.family

	first := wantJSON(t, "first upload", e.upload(t, "owner", e.family, body, "dedup-1"), 201)
	if first["result"] != "created" {
		t.Fatalf("first upload %v", first)
	}
	asset := first["asset_id"].(string)
	base := diskUsage(t, pool)
	baseAssets, baseEvents := e.count(t, "SELECT count(*) FROM assets"), e.events(t, e.family)

	check := func(what string, assetsWithSHA int) {
		t.Helper()
		if got := diskUsage(t, pool); got != base {
			t.Fatalf("%s: pool %+v, want %+v (no new bytes on disk)", what, got, base)
		}
		var named []string
		for _, f := range regularFiles(t, pool) {
			if strings.Contains(f, sha) {
				named = append(named, f)
			}
		}
		if want := []string{blobstore.PoolRelPath(sha)[len(".system/pool/"):]}; !slices.Equal(named, want) {
			t.Fatalf("%s: pool files for the hash %v, want %v", what, named, want)
		}
		if n := e.count(t, "SELECT count(*) FROM blobs WHERE sha256 = ?", sha); n != 1 {
			t.Fatalf("%s: %d blobs rows, want 1", what, n)
		}
		if n := e.count(t, "SELECT count(*) FROM assets WHERE original_sha256 = ?", sha); n != assetsWithSHA {
			t.Fatalf("%s: %d assets with the hash, want %d", what, n, assetsWithSHA)
		}
		for _, dir := range []string{"incoming", "uploads"} {
			if left := regularFiles(t, filepath.Join(e.Root, ".system", dir)); len(left) != 0 {
				t.Fatalf("%s: left in .system/%s: %v", what, dir, left)
			}
		}
	}
	wantPresent := func(what string, r reply) {
		t.Helper()
		m := wantJSON(t, what, r, 200)
		if m["result"] != "already_present" || m["asset_id"] != asset {
			t.Fatalf("%s: %v, want already_present %s", what, m, asset)
		}
	}
	check("first upload", 1)

	// Same album, same account, new requests (other keys): the asset that
	// is there, nothing new to see.
	for i := 2; i <= 4; i++ {
		wantPresent(fmt.Sprint("repeat ", i), e.upload(t, "owner", e.family, body, fmt.Sprint("dedup-", i)))
	}
	// A retry of the first request (same key) replays it.
	if r := e.upload(t, "owner", e.family, body, "dedup-1"); r.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry of the first request not replayed: %d %s", r.Status, r.Body)
	}
	check("repeats", 1)

	// Same album through a resumable upload session.
	c := sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["owner"]}}
	id := wantJSON(t, "session", c.create("dedup-session", body, ""), 201)["upload_id"].(string)
	sendAll(t, c, id, body, 0, 1<<20)
	wantPresent("session complete", c.complete(id))
	check("upload session", 1)

	// Another account, same album.
	wantPresent("contributor", e.upload(t, "contributor", e.family, body, "dedup-contributor"))
	check("contributor", 1)

	if n, m := e.count(t, "SELECT count(*) FROM assets"), e.events(t, e.family); n != baseAssets || m != baseEvents {
		t.Fatalf("same album: assets %d events %d, want %d %d (nothing new visible)", n, m, baseAssets, baseEvents)
	}
	feed := wantJSON(t, "owner sync", e.as(t, "owner", "GET", family+"/sync", nil, nil), 200)
	n := 0
	for _, a := range feed["assets"].(map[string]any) { // asset id -> asset
		if a.(map[string]any)["sha256"] == sha {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("family feed shows the bytes %d times, want once", n)
	}

	// Another album of the same account: a new asset there (its own id,
	// name, uploader), linked to the same stored original.
	second := wantJSON(t, "second album", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"second"}`), nil), 201)["id"].(string)
	other := wantJSON(t, "other album", e.upload(t, "owner", second, body, "dedup-second"), 201)
	if other["result"] != "created" || other["asset_id"] == asset {
		t.Fatalf("other album %v", other)
	}
	check("other album", 2)

	// Another account, its own album.
	theirs := wantJSON(t, "admin album", e.upload(t, "root", e.adminF, body, "dedup-root"), 201)
	if theirs["asset_id"] == asset || theirs["asset_id"] == other["asset_id"] {
		t.Fatalf("other account %v", theirs)
	}
	check("other account", 3)

	// UPLOADER: the same 202 receipt as for bytes the server never saw,
	// and still one copy.
	fresh := randomBytes(t, 4096)
	wantU := func(what string, b []byte, key string) map[string]any {
		t.Helper()
		m := wantJSON(t, what, e.upload(t, "uploader", e.family, b, key), 202)
		if fmt.Sprint(m) != fmt.Sprint(map[string]any{"result": "accepted", "folder_id": e.family, "sha256": shaHex(b), "bytes": float64(len(b))}) {
			t.Fatalf("%s: %v", what, m)
		}
		return m
	}
	wantU("uploader, known bytes", body, "dedup-uploader")
	check("uploader", 3)
	wantU("uploader, new bytes", fresh, "dedup-uploader-fresh")
	if got := diskUsage(t, pool); got.files != base.files+1 {
		t.Fatalf("uploader's new bytes: %d pool files, want %d", got.files, base.files+1)
	}

	// Live Photo: the same still and video in two albums are two pool files.
	lp := newLive(liveID, 45)
	wantJSON(t, "live family", e.uploadLive(t, "owner", e.family, "dedup-live-1", lp), 201)
	afterLive := diskUsage(t, pool)
	wantJSON(t, "live second", e.uploadLive(t, "owner", second, "dedup-live-2", lp), 201)
	if got := diskUsage(t, pool); got != afterLive {
		t.Fatalf("second Live Photo: pool %+v, want %+v", got, afterLive)
	}
	for _, b := range [][]byte{lp.still, lp.video} {
		if n := e.count(t, "SELECT count(*) FROM blobs WHERE sha256 = ?", shaHex(b)); n != 1 {
			t.Fatalf("live component: %d blobs rows", n)
		}
		if _, err := os.Stat(e.Blobs.PoolPath(shaHex(b))); err != nil {
			t.Fatal(err)
		}
	}

	// The administrator's storage totals: the body is held four times
	// (family, second, admin-only and nothing more for the UPLOADER, who
	// sent it to family) but stored once.
	st := wantJSON(t, "storage", e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil), 200)
	u := st["usage"].(map[string]any)
	stored := int64(e.count(t, "SELECT SUM(bytes) FROM blobs"))
	if int64(u["stored_bytes"].(float64)) != stored || int64(u["stored_originals"].(float64)) != int64(e.count(t, "SELECT count(*) FROM blobs")) {
		t.Fatalf("usage %v, stored %d", u, stored)
	}
	// Saved: body twice (second, admin-only), the Live Photo's still and
	// video once each (second album).
	wantSaved := 2*int64(len(body)) + int64(len(lp.still)+len(lp.video))
	if got := int64(u["dedup_saved_bytes"].(float64)); got != wantSaved {
		t.Fatalf("dedup_saved_bytes %d, want %d (%v)", got, wantSaved, u)
	}
	if int64(u["album_bytes"].(float64))-int64(u["dedup_saved_bytes"].(float64)) > stored {
		t.Fatalf("usage %v: distinct album bytes above the stored bytes %d", u, stored)
	}
}

// TestStorageUsageAfterRemoval: an original removed from its only album
// stays stored (originals are never deleted) and no longer counts as held.
func TestStorageUsageAfterRemoval(t *testing.T) {
	e := newACLEnv(t)
	body := randomBytes(t, 10000)
	a := wantJSON(t, "upload", e.upload(t, "owner", e.family, body, "rm-1"), 201)["asset_id"].(string)
	usage := func() map[string]any {
		return wantJSON(t, "storage", e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil), 200)["usage"].(map[string]any)
	}
	before := usage()
	wantStatusReply(t, "remove", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family+"/assets/"+a, nil, nil), 204)
	after := usage()
	if after["stored_bytes"] != before["stored_bytes"] || after["stored_originals"] != before["stored_originals"] {
		t.Fatalf("stored changed on removal: %v -> %v", before, after)
	}
	if before["album_bytes"].(float64)-after["album_bytes"].(float64) != float64(len(body)) ||
		before["album_copies"].(float64)-after["album_copies"].(float64) != 1 || after["dedup_saved_bytes"] != float64(0) {
		t.Fatalf("album totals %v -> %v", before, after)
	}
	// Not for anyone but the server administrator.
	wantError(t, "owner", e.as(t, "owner", "GET", "/api/v1/admin/server/storage", nil, nil), 403, "forbidden")
}
