package legacyimport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
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

// sampleJPEG generates a small test image; seed makes each one distinct.
func sampleJPEG(t *testing.T, w, h int, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: seed, G: uint8(x), B: uint8(y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// legacy is a storage root populated through the PoC code path: the
// state.json store (jsonstore) and the blob store, used the way the PoC
// upload handler used them.
type legacy struct {
	vol     *storage.Volume
	root    string
	folders []string // folder ids
	shas    []string // every uploaded original
}

// legacyUpload stores body like the PoC upload handler did and returns the
// commit result.
func legacyUpload(t *testing.T, js *jsonstore.Store, blobs *blobstore.Store, folderID string, body []byte, filename, device string) string {
	t.Helper()
	staged, err := blobs.Stage(bytes.NewReader(body), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Remove()
	sha := staged.SHA256
	preview := false
	if existing, err := js.GetAssetBySHA(t.Context(), sha); err == nil {
		preview = existing.PreviewReady
	} else {
		if err := blobs.Promote(staged, sha); err != nil {
			t.Fatal(err)
		}
		preview = blobs.MakePreview(sha) == nil
	}
	if filename == "" {
		filename = sha
	}
	res, err := js.CommitUpload(t.Context(), meta.CommitRequest{FolderID: folderID, DeviceID: device,
		Asset: meta.Asset{ID: sha, Filename: filename, MimeType: staged.MimeType, Bytes: staged.Size, PreviewReady: preview}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Result
}

func buildLegacy(t *testing.T) *legacy {
	t.Helper()
	vol := newVolume(t)
	js, err := jsonstore.Open(vol)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	blobs, err := blobstore.New(vol)
	if err != nil {
		t.Fatal(err)
	}
	l := &legacy{vol: vol, root: vol.Root()}
	for _, name := range []string{"family", "trip", "empty"} {
		f, err := js.CreateFolder(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		l.folders = append(l.folders, f.ID)
		time.Sleep(2 * time.Millisecond) // distinct created_at
	}
	jpg1, jpg2, jpg3 := sampleJPEG(t, 40, 30, 1), sampleJPEG(t, 20, 50, 2), sampleJPEG(t, 33, 33, 3)
	text := []byte("plain sample text, no preview\n")
	uploads := []struct {
		folder           int
		body             []byte
		filename, device string
		result           string
	}{
		{0, jpg1, "beach.jpg", "phone-a", meta.ResultCreated},
		{0, jpg2, "가족.jpg", "phone-b", meta.ResultCreated},
		{0, text, "notes.txt", "", meta.ResultCreated},
		{0, jpg1, "again.jpg", "phone-a", meta.ResultAlreadyPresent}, // no event
		{1, jpg1, "beach-copy.jpg", "tablet", meta.ResultLinked},
		{1, jpg3, "mountain.jpg", "tablet", meta.ResultCreated},
	}
	for i, u := range uploads {
		if got := legacyUpload(t, js, blobs, l.folders[u.folder], u.body, u.filename, u.device); got != u.result {
			t.Fatalf("upload %d: %s, want %s", i, got, u.result)
		}
	}
	l.shas = []string{shaOf(jpg1), shaOf(jpg2), shaOf(text), shaOf(jpg3)}
	return l
}

func (l *legacy) statePath() string { return jsonstore.StatePath(l.root) }

func (l *legacy) state(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(l.statePath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// view is what both the PoC store and the SQLite store can show.
type view interface {
	ListFolders(ctx context.Context) ([]meta.Folder, error)
	ListEvents(ctx context.Context, folderID string, cursor int64, limit int) (meta.EventPage, error)
	GetAssetBySHA(ctx context.Context, sha string) (meta.Asset, error)
}

// observe records everything the stores expose about the legacy data, with
// asset identifiers replaced by the hash of their bytes (the PoC used the
// hash as ID; the import assigns random IDs), plus the blob files.
func observe(t *testing.T, v view, l *legacy) map[string]string {
	t.Helper()
	out := map[string]string{}
	folders, err := v.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].ID < folders[j].ID })
	out["folders"] = fmt.Sprint(folders)
	shaOfAsset := func(a meta.Asset) string {
		if a.SHA256 != "" {
			return a.SHA256
		}
		return a.ID // PoC store
	}
	norm := func(a meta.Asset) meta.Asset {
		a.ID, a.SHA256 = shaOfAsset(a), ""
		// Not in the PoC format; derived from preview_ready (and the MIME
		// type) for imports.
		a.PreviewStatus, a.MediaKind = "", ""
		return a
	}
	for _, fid := range l.folders {
		for cursor := 0; cursor <= 5; cursor++ {
			p, err := v.ListEvents(t.Context(), fid, int64(cursor), 100)
			if err != nil {
				t.Fatal(err)
			}
			var parts []string
			for _, e := range p.Events {
				a := p.Assets[e.AssetID]
				e.AssetID = shaOfAsset(a)
				parts = append(parts, fmt.Sprintf("%+v %+v", e, norm(a)))
			}
			out[fmt.Sprintf("events %s %d", fid, cursor)] = fmt.Sprint(parts, p.NextCursor, p.HasMore)
		}
	}
	for _, sha := range l.shas {
		a, err := v.GetAssetBySHA(t.Context(), sha)
		out["asset "+sha] = fmt.Sprintf("%+v %v", norm(a), err)
		for _, rel := range []string{blobstore.PoolRelPath(sha), blobstore.PreviewRelPath(sha)} {
			b, err := os.ReadFile(filepath.Join(l.root, filepath.FromSlash(rel)))
			out[rel] = fmt.Sprint(shaOf(b), " ", err == nil)
		}
	}
	return out
}

func observeLegacy(t *testing.T, l *legacy) map[string]string {
	t.Helper()
	js, err := jsonstore.Open(l.vol)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	return observe(t, js, l)
}

func openImported(t *testing.T, l *legacy) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.Open(t.Context(), l.vol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sameObservation(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("observed %d items, want %d", len(got), len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s:\n got %s\nwant %s", k, got[k], w)
		}
	}
}

// snapshot maps every path under root to its mode, size and content hash.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			v += " " + shaOf(b)
		}
		out[filepath.ToSlash(rel)] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t *testing.T, what string, want, got map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s changed: %q -> %q", what, k, w, got[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected new path %s", what, k)
		}
	}
}

// blobFiles is the subset of a snapshot under pool/ and previews/.
func blobFiles(s map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range s {
		if strings.HasPrefix(k, ".system/pool") || strings.HasPrefix(k, ".system/previews") {
			out[k] = v
		}
	}
	return out
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists (err %v)", path, err)
	}
}

func apply(t *testing.T, vol *storage.Volume, hook func(string) error) (*Report, error) {
	t.Helper()
	return Run(t.Context(), vol, Options{Apply: true, hook: hook})
}

// TestGoldenImport: data written by the PoC code path is imported with every
// folder, asset, link, event and sequence intact, and the HTTP API shows
// exactly what it showed before.
func TestGoldenImport(t *testing.T) {
	l := buildLegacy(t)
	before := observeLegacy(t, l)
	src := l.state(t)
	snap := snapshot(t, l.root)

	var progress bytes.Buffer
	r, err := Run(t.Context(), l.vol, Options{Progress: &progress})
	if err != nil {
		t.Fatalf("dry-run: %v (%v)", err, r.Problems)
	}
	if r.Folders != 3 || r.Assets != 4 || r.Links != 5 || r.Events != 5 || r.PreviewsReady != 3 || len(r.Problems) != 0 {
		t.Fatalf("dry-run report: %+v", r)
	}
	if r.SourceSHA256 != shaOf(src) || !strings.Contains(progress.String(), "4/4 files") {
		t.Fatalf("dry-run report source %s, progress %q", r.SourceSHA256, progress.String())
	}
	sameSnapshot(t, "dry-run", snap, snapshot(t, l.root))

	r, err = apply(t, l.vol, nil)
	if err != nil {
		t.Fatalf("apply: %v (%v)", err, r.Problems)
	}
	if r.Result != "ok" || r.Resumed || r.BackupPath == "" || r.MigratedPath == "" || r.DatabasePath != ".system/withalbum.db" {
		t.Fatalf("apply report: %+v", r)
	}
	after := snapshot(t, l.root)
	sameSnapshot(t, "pool/previews", blobFiles(snap), blobFiles(after))
	mustNotExist(t, l.statePath())
	mustNotExist(t, sqlitestore.DBPath(l.root)+".importing")
	for _, rel := range []string{r.BackupPath, r.MigratedPath} {
		b, err := os.ReadFile(filepath.Join(l.root, rel))
		if err != nil || !bytes.Equal(b, src) {
			t.Fatalf("%s does not hold the original state.json bytes (%v)", rel, err)
		}
	}
	if info, err := os.Stat(sqlitestore.DBPath(l.root)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database mode: %v %v", info, err)
	}

	store := openImported(t, l)
	sameObservation(t, before, observe(t, store, l))
	ok, err := store.HasLegacyImport(t.Context(), shaOf(src))
	if err != nil || !ok {
		t.Fatalf("legacy_imports row missing: %v", err)
	}
	// The imported folders belong to the bootstrap administrator, who keeps
	// them after taking the account over (admin init keeps the user_id).
	owned, err := store.ListFoldersFor(t.Context(), store.BootstrapOwnerID())
	if err != nil || len(owned) != 3 {
		t.Fatalf("folders of the bootstrap administrator: %+v %v", owned, err)
	}
	for _, fa := range owned {
		if fa.Access.OwnerID != store.BootstrapOwnerID() {
			t.Fatalf("folder %s owner %s", fa.Folder.ID, fa.Access.OwnerID)
		}
	}
	// Legacy assets are reachable only through their folders.
	page, err := store.ListEvents(t.Context(), l.folders[1], 0, 10)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("trip events %+v %v", page, err)
	}
	if _, err := store.GetFolderAsset(t.Context(), l.folders[1], page.Events[0].AssetID); err != nil {
		t.Fatalf("legacy asset in its folder: %v", err)
	}
	if _, err := store.GetFolderAsset(t.Context(), l.folders[2], page.Events[0].AssetID); !errors.Is(err, meta.ErrAssetNotFound) {
		t.Fatalf("legacy asset through another folder: %v", err)
	}
	// Sequences continue where state.json left them.
	next := sampleJPEG(t, 10, 10, 9)
	if _, err := store.CommitUpload(t.Context(), meta.CommitRequest{FolderID: l.folders[0], DeviceID: "phone-a",
		Asset: meta.Asset{SHA256: shaOf(next), Filename: "n.jpg", MimeType: "image/jpeg", Bytes: int64(len(next))}}); err != nil {
		t.Fatalf("upload after import: %v", err)
	}
	page, err = store.ListEvents(t.Context(), l.folders[0], 3, 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].Seq != 4 {
		t.Fatalf("event after import: %+v %v", page, err)
	}

	// Nothing left to import; a second run changes nothing.
	store.Close()
	snap = snapshot(t, l.root)
	if _, err := apply(t, l.vol, nil); !errors.Is(err, ErrNoLegacyState) {
		t.Fatalf("second apply = %v, want ErrNoLegacyState", err)
	}
	sameSnapshot(t, "second apply", snap, snapshot(t, l.root))
}

func TestImportMapsRowsAsSpecified(t *testing.T) {
	l := buildLegacy(t)
	var st state
	{
		p, err := parseState(l.state(t))
		if err != nil {
			t.Fatal(err)
		}
		st = *p
	}
	if _, err := apply(t, l.vol, nil); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.Open(sqlitestore.DBPath(l.root))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	// Random asset ids, never the SHA-256, recorded in legacy_asset_map.
	rows, err := sqlDB.Query("SELECT m.legacy_sha256, a.asset_id FROM legacy_asset_map m JOIN assets a ON a.asset_id = m.asset_id")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var sha, id string
		if err := rows.Scan(&sha, &id); err != nil {
			t.Fatal(err)
		}
		if id == sha || len(id) != 32 {
			t.Errorf("asset id %q for %s is not a random id", id, sha)
		}
		n++
	}
	rows.Close()
	if n != len(st.Assets) {
		t.Fatalf("%d mapped assets, want %d", n, len(st.Assets))
	}
	var owners, labels int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM folders f JOIN users u ON u.user_id = f.owner_user_id
		WHERE u.login_name = 'admin' AND u.password_hash = '!locked'`).Scan(&owners); err != nil || owners != 3 {
		t.Fatalf("folders owned by bootstrap admin: %d %v", owners, err)
	}
	if err := sqlDB.QueryRow(`SELECT count(*) FROM folder_changes WHERE uploader_device_label = 'tablet'`).Scan(&labels); err != nil || labels != 2 {
		t.Fatalf("device labels: %d %v", labels, err)
	}
	for _, fid := range l.folders {
		var last int64
		if err := sqlDB.QueryRow("SELECT last_seq FROM folder_sequences WHERE folder_id = ?", fid).Scan(&last); err != nil || last != st.NextSeq[fid] {
			t.Fatalf("folder %s last_seq %d, want %d (%v)", fid, last, st.NextSeq[fid], err)
		}
	}
	var report string
	if err := sqlDB.QueryRow("SELECT report_json FROM legacy_imports").Scan(&report); err != nil || !strings.Contains(report, `"result":"ok"`) {
		t.Fatalf("report_json %q %v", report, err)
	}
}

// editState decodes state.json generically, lets fn change it and writes it back.
func editState(t *testing.T, l *legacy, fn func(m map[string]any)) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(l.state(t), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.statePath(), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func folderEvents(m map[string]any, fid string) []any {
	return m["events"].(map[string]any)[fid].([]any)
}

func rewritePool(t *testing.T, l *legacy, sha string, fn func([]byte) []byte) {
	t.Helper()
	path := filepath.Join(l.root, filepath.FromSlash(blobstore.PoolRelPath(sha)))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fn(b), 0400); err != nil {
		t.Fatal(err)
	}
}

// TestDefectiveInputIsRefused: every defect makes dry-run and apply fail with
// a clear reason, and nothing at all is written (no backup, no database).
func TestDefectiveInputIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   error
		detail string
		break_ func(t *testing.T, l *legacy)
	}{
		{"missing pool file", ErrFilesInvalid, "is missing", func(t *testing.T, l *legacy) {
			path := filepath.Join(l.root, filepath.FromSlash(blobstore.PoolRelPath(l.shas[1])))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"hash mismatch", ErrFilesInvalid, "content does not match", func(t *testing.T, l *legacy) {
			rewritePool(t, l, l.shas[0], func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b })
		}},
		{"size mismatch", ErrFilesInvalid, "bytes, state.json says", func(t *testing.T, l *legacy) {
			rewritePool(t, l, l.shas[2], func(b []byte) []byte { return b[:len(b)-1] })
		}},
		{"pool symlink", ErrFilesInvalid, "not a regular file", func(t *testing.T, l *legacy) {
			path := filepath.Join(l.root, filepath.FromSlash(blobstore.PoolRelPath(l.shas[3])))
			moved := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing preview", ErrFilesInvalid, "preview_ready but preview", func(t *testing.T, l *legacy) {
			if err := os.Remove(filepath.Join(l.root, filepath.FromSlash(blobstore.PreviewRelPath(l.shas[0])))); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated json", ErrInvalidState, "parse state.json", func(t *testing.T, l *legacy) {
			b := l.state(t)
			if err := os.WriteFile(l.statePath(), b[:len(b)/2], 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"empty file", ErrInvalidState, "parse state.json", func(t *testing.T, l *legacy) {
			if err := os.WriteFile(l.statePath(), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"trailing data", ErrInvalidState, "after the JSON object", func(t *testing.T, l *legacy) {
			if err := os.WriteFile(l.statePath(), append(l.state(t), []byte("{}")...), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate key", ErrInvalidState, "duplicate key", func(t *testing.T, l *legacy) {
			b := bytes.Replace(l.state(t), []byte(`"folders": {`), []byte(`"folders": {"x": {}, "x": {},`), 1)
			if err := os.WriteFile(l.statePath(), b, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown field", ErrInvalidState, "unknown field", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) { m["deleted"] = map[string]any{} })
		}},
		{"missing top-level key", ErrInvalidState, "next_seq", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) { delete(m, "next_seq") })
		}},
		{"seq gap", ErrInvalidState, "without gaps", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) { folderEvents(m, l.folders[0])[1].(map[string]any)["seq"] = 3 })
		}},
		{"next_seq mismatch", ErrInvalidState, "does not match 3 events", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) { m["next_seq"].(map[string]any)[l.folders[0]] = 7 })
		}},
		{"event without link", ErrInvalidState, "not linked", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) {
				delete(m["links"].(map[string]any)[l.folders[1]].(map[string]any), l.shas[3])
			})
		}},
		{"link without event", ErrInvalidState, "no asset_added event", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) {
				evs := folderEvents(m, l.folders[1])
				m["events"].(map[string]any)[l.folders[1]] = evs[:1]
				m["next_seq"].(map[string]any)[l.folders[1]] = 1
			})
		}},
		{"unknown asset in link", ErrInvalidState, "unknown asset", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) { delete(m["assets"].(map[string]any), l.shas[3]) })
		}},
		{"asset key not a hash", ErrInvalidState, "not a lowercase SHA-256", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) {
				m["assets"].(map[string]any)["../../etc"] = map[string]any{"id": "../../etc", "filename": "x", "mime_type": "text/plain", "bytes": 1, "uploaded_at": "2026-01-01T00:00:00Z"}
			})
		}},
		{"unsupported event kind", ErrInvalidState, "unsupported kind", func(t *testing.T, l *legacy) {
			editState(t, l, func(m map[string]any) { folderEvents(m, l.folders[0])[0].(map[string]any)["kind"] = "asset_removed" })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := buildLegacy(t)
			tc.break_(t, l)
			snap := snapshot(t, l.root)
			for _, applyMode := range []bool{false, true} {
				r, err := Run(t.Context(), l.vol, Options{Apply: applyMode})
				if !errors.Is(err, tc.want) {
					t.Fatalf("apply=%v: err = %v, want %v", applyMode, err, tc.want)
				}
				if r.Result != "failed" || !strings.Contains(strings.Join(r.Problems, "\n"), tc.detail) {
					t.Fatalf("apply=%v: problems %q do not mention %q", applyMode, r.Problems, tc.detail)
				}
				sameSnapshot(t, "after refused run", snap, snapshot(t, l.root))
			}
			mustNotExist(t, sqlitestore.DBPath(l.root))
		})
	}
}

// TestReimportRefusedWhenDatabaseExists: a state.json next to a database that
// does not record it is never imported over that database.
func TestReimportRefusedWhenDatabaseExists(t *testing.T) {
	l := buildLegacy(t)
	src := l.state(t)
	if err := os.Rename(l.statePath(), l.statePath()+".aside"); err != nil {
		t.Fatal(err)
	}
	s, err := sqlitestore.Open(t.Context(), l.vol) // a new install's database
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Rename(l.statePath()+".aside", l.statePath()); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, l.root)
	for _, applyMode := range []bool{false, true} {
		if _, err := Run(t.Context(), l.vol, Options{Apply: applyMode}); !errors.Is(err, ErrDatabaseExists) {
			t.Fatalf("apply=%v: err = %v, want ErrDatabaseExists", applyMode, err)
		}
	}
	sameSnapshot(t, "refused", snap, snapshot(t, l.root))
	if !bytes.Equal(l.state(t), src) {
		t.Fatal("state.json changed")
	}
}

// TestReimportAfterSuccessRefused: after an import, a different state.json is
// refused (the database does not record it).
func TestReimportAfterSuccessRefused(t *testing.T) {
	l := buildLegacy(t)
	if _, err := apply(t, l.vol, nil); err != nil {
		t.Fatal(err)
	}
	other := []byte(`{"folders":{},"assets":{},"links":{},"events":{},"next_seq":{}}`)
	if err := os.WriteFile(l.statePath(), other, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(t, l.vol, nil); !errors.Is(err, ErrDatabaseExists) {
		t.Fatalf("err = %v, want ErrDatabaseExists", err)
	}
	if b := l.state(t); !bytes.Equal(b, other) {
		t.Fatal("state.json changed")
	}
}

var errCrash = errors.New("simulated crash")

// TestCrashAtEveryPointIsRecoverable: the process "dies" at each step of
// apply. Originals and the state.json bytes are untouched, the database only
// appears once it is verified, and running apply again completes the import
// with the same observable result.
func TestCrashAtEveryPointIsRecoverable(t *testing.T) {
	for _, point := range []string{pointAfterBackup, pointBeforeCommit, pointAfterCommit, pointAfterVerify, pointAfterDBRename, pointAfterStateRename} {
		t.Run(point, func(t *testing.T) {
			l := buildLegacy(t)
			before := observeLegacy(t, l)
			src := l.state(t)
			blobs := blobFiles(snapshot(t, l.root))
			_, err := apply(t, l.vol, func(p string) error {
				if p == point {
					return errCrash
				}
				return nil
			})
			if !errors.Is(err, errCrash) {
				t.Fatalf("apply = %v, want the simulated crash", err)
			}
			sameSnapshot(t, "pool/previews after crash", blobs, blobFiles(snapshot(t, l.root)))
			dbPlaced := point == pointAfterDBRename || point == pointAfterStateRename
			if ok, _ := exists(sqlitestore.DBPath(l.root)); ok != dbPlaced {
				t.Fatalf("withalbum.db exists = %v after crash at %s", ok, point)
			}
			if point != pointAfterStateRename {
				if !bytes.Equal(l.state(t), src) {
					t.Fatal("state.json changed")
				}
			}
			if !dbPlaced {
				// Still exactly the pre-import state for the old version.
				sameObservation(t, before, observeLegacy(t, l))
			}

			r, err := apply(t, l.vol, nil)
			if point == pointAfterStateRename {
				if !errors.Is(err, ErrNoLegacyState) {
					t.Fatalf("re-run = %v, want ErrNoLegacyState (already complete)", err)
				}
			} else if err != nil {
				t.Fatalf("re-run: %v (%v)", err, r.Problems)
			} else if r.Resumed != (point == pointAfterDBRename) {
				t.Fatalf("re-run resumed = %v", r.Resumed)
			}
			mustNotExist(t, l.statePath())
			mustNotExist(t, sqlitestore.DBPath(l.root)+".importing")
			sameSnapshot(t, "pool/previews after re-run", blobs, blobFiles(snapshot(t, l.root)))
			matches, _ := filepath.Glob(l.statePath() + ".migrated-*")
			if len(matches) != 1 {
				t.Fatalf("migrated copies: %v", matches)
			}
			if b, _ := os.ReadFile(matches[0]); !bytes.Equal(b, src) {
				t.Fatal("migrated state.json differs from the original")
			}
			store := openImported(t, l)
			sameObservation(t, before, observe(t, store, l))
			ok, err := store.HasLegacyImport(t.Context(), shaOf(src))
			if err != nil || !ok {
				t.Fatalf("legacy import row: %v %v", ok, err)
			}
		})
	}
}

// TestFailuresDiscardTemporaryDatabase: failures that are detected (not
// crashes) remove the temporary database right away and leave no
// withalbum.db; fixing the cause and re-running succeeds.
func TestFailuresDiscardTemporaryDatabase(t *testing.T) {
	tmpOf := func(l *legacy) string { return sqlitestore.DBPath(l.root) + ".importing" }
	for _, tc := range []struct {
		name   string
		want   error
		point  string
		inject func(t *testing.T, l *legacy)
		repair func(t *testing.T, l *legacy, src []byte)
	}{
		{"verification mismatch", ErrVerifyFailed, pointAfterCommit, func(t *testing.T, l *legacy) {
			sqlDB, err := db.Open(tmpOf(l))
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			if _, err := sqlDB.Exec("DELETE FROM folder_changes WHERE seq = 2"); err != nil {
				t.Fatal(err)
			}
		}, func(*testing.T, *legacy, []byte) {}},
		{"state changed during import", ErrStateChanged, pointAfterVerify, func(t *testing.T, l *legacy) {
			if err := os.WriteFile(l.statePath(), append(l.state(t), '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, l *legacy, src []byte) {
			if err := os.WriteFile(l.statePath(), src, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"volume lost", storage.ErrNotInitialized, pointAfterBackup, func(t *testing.T, l *legacy) {
			if err := os.Rename(filepath.Join(l.root, storage.MarkerName), filepath.Join(l.root, "marker.aside")); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, l *legacy, _ []byte) {
			if err := os.Rename(filepath.Join(l.root, "marker.aside"), filepath.Join(l.root, storage.MarkerName)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := buildLegacy(t)
			before := observeLegacy(t, l)
			src := l.state(t)
			blobs := blobFiles(snapshot(t, l.root))
			_, err := apply(t, l.vol, func(p string) error {
				if p == tc.point {
					tc.inject(t, l)
				}
				return nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("apply = %v, want %v", err, tc.want)
			}
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				mustNotExist(t, tmpOf(l)+suffix)
				mustNotExist(t, sqlitestore.DBPath(l.root)+suffix)
			}
			sameSnapshot(t, "pool/previews", blobs, blobFiles(snapshot(t, l.root)))
			tc.repair(t, l, src)
			if !bytes.Equal(l.state(t), src) {
				t.Fatal("state.json changed")
			}
			if r, err := apply(t, l.vol, nil); err != nil {
				t.Fatalf("re-run: %v (%v)", err, r.Problems)
			}
			sameObservation(t, before, observe(t, openImported(t, l), l))
		})
	}
}

func TestStaleDatabaseSideFilesRefused(t *testing.T) {
	l := buildLegacy(t)
	wal := sqlitestore.DBPath(l.root) + "-wal"
	if err := os.WriteFile(wal, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, l.root)
	if _, err := apply(t, l.vol, nil); !errors.Is(err, ErrStaleFiles) {
		t.Fatalf("err = %v, want ErrStaleFiles", err)
	}
	sameSnapshot(t, "refused", snap, snapshot(t, l.root))
}

// TestWarningsDoNotBlockAndLoseNothing: unreferenced pool files and an
// unlinked asset are reported; the unlinked asset is still imported.
func TestWarningsDoNotBlockAndLoseNothing(t *testing.T) {
	l := buildLegacy(t)
	orphan := sampleJPEG(t, 8, 8, 7)
	orphanSHA := shaOf(orphan)
	dir := filepath.Join(l.root, ".system", "pool", orphanSHA[:2])
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, orphanSHA), orphan, 0400); err != nil {
		t.Fatal(err)
	}
	// Register a second orphan as an unlinked asset.
	unlinked := []byte("unlinked asset bytes")
	unlinkedSHA := shaOf(unlinked)
	udir := filepath.Join(l.root, ".system", "pool", unlinkedSHA[:2])
	if err := os.MkdirAll(udir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udir, unlinkedSHA), unlinked, 0400); err != nil {
		t.Fatal(err)
	}
	editState(t, l, func(m map[string]any) {
		m["assets"].(map[string]any)[unlinkedSHA] = map[string]any{"id": unlinkedSHA, "filename": "u.txt", "mime_type": "text/plain; charset=utf-8",
			"bytes": len(unlinked), "preview_ready": false, "uploaded_at": "2026-01-01T00:00:00Z"}
	})
	r, err := apply(t, l.vol, nil)
	if err != nil {
		t.Fatalf("apply: %v (%v)", err, r.Problems)
	}
	w := strings.Join(r.Warnings, "\n")
	if !strings.Contains(w, orphanSHA) || !strings.Contains(w, "not linked to any folder") {
		t.Fatalf("warnings: %q", r.Warnings)
	}
	store := openImported(t, l)
	if a, err := store.GetAssetBySHA(t.Context(), unlinkedSHA); err != nil || a.Filename != "u.txt" {
		t.Fatalf("unlinked asset: %+v %v", a, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, orphanSHA)); err != nil || !bytes.Equal(b, orphan) {
		t.Fatal("orphan pool file changed")
	}
}

func TestEmptyStateImports(t *testing.T) {
	vol := newVolume(t)
	if err := os.Mkdir(filepath.Join(vol.Root(), ".system"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonstore.StatePath(vol.Root()), []byte(`{"folders":{},"assets":{},"links":{},"events":{},"next_seq":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := apply(t, vol, nil); err != nil {
		t.Fatalf("apply: %v (%v)", err, r.Problems)
	}
	s, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if f, err := s.ListFolders(t.Context()); err != nil || len(f) != 0 {
		t.Fatalf("folders %v %v", f, err)
	}
}

func TestNoStateJSON(t *testing.T) {
	vol := newVolume(t)
	snap := snapshot(t, vol.Root())
	for _, applyMode := range []bool{false, true} {
		if _, err := Run(t.Context(), vol, Options{Apply: applyMode}); !errors.Is(err, ErrNoLegacyState) {
			t.Fatalf("err = %v, want ErrNoLegacyState", err)
		}
	}
	sameSnapshot(t, "no state", snap, snapshot(t, vol.Root()))
}

func TestReportSummary(t *testing.T) {
	l := buildLegacy(t)
	r, err := Run(t.Context(), l.vol, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	r.WriteSummary(&buf)
	for _, want := range []string{"dry-run): ok", "folders 3, assets 4, folder links 5, events 5", `"family"`, "nothing was written"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, buf.String())
		}
	}
	if err := json.NewEncoder(io.Discard).Encode(r); err != nil {
		t.Fatal(err)
	}
}
