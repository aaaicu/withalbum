package integrity_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/integrity"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

func TestRun(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := integrity.OpenDB(vol); err == nil {
		t.Fatal("OpenDB created a database")
	}
	store, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bs, err := blobstore.New(vol)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.CreateFolder(t.Context(), meta.NewFolder{Name: "f", OwnerID: store.BootstrapOwnerID()})
	if err != nil {
		t.Fatal(err)
	}
	var shas []string
	for i := range 4 {
		staged, err := bs.Stage(strings.NewReader(strings.Repeat("x", 100+i)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := bs.Promote(staged, staged.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitUpload(t.Context(), meta.CommitRequest{FolderID: f.ID,
			Asset: meta.Asset{SHA256: staged.SHA256, Filename: "a.bin", MimeType: staged.MimeType, Bytes: staged.Size, PreviewReady: i == 3}}); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, staged.SHA256)
	}
	r, err := integrity.Run(t.Context(), vol, store.DB(), integrity.Options{Hash: true})
	if err != nil {
		t.Fatal(err)
	}
	// shas[3] claims a preview that does not exist (a warning only).
	if !r.Healthy() || r.Blobs != 4 || r.Hashed != 4 || r.PreviewsMissing != 1 {
		t.Fatalf("healthy library: %+v", r)
	}
	pool := func(sha string) string { return filepath.Join(root, filepath.FromSlash(blobstore.PoolRelPath(sha))) }
	os.Remove(pool(shas[0]))
	os.Chmod(pool(shas[1]), 0600)
	os.WriteFile(pool(shas[1]), []byte("short"), 0400)
	os.Chmod(pool(shas[2]), 0600)
	os.WriteFile(pool(shas[2]), bytes.Repeat([]byte("y"), 102), 0400)
	// A pool file without a row, an orphan and an incoming file.
	stray := strings.Repeat("ab", 32)
	os.MkdirAll(filepath.Dir(pool(stray)), 0700)
	os.WriteFile(pool(stray), []byte("stray"), 0400)
	os.MkdirAll(filepath.Join(root, ".system", "orphans"), 0700)
	os.WriteFile(filepath.Join(root, ".system", "orphans", "x.orphan.1"), []byte("12345"), 0400)
	os.WriteFile(filepath.Join(root, ".system", "incoming", "upload-1"), []byte("1"), 0600)

	before, _ := os.ReadDir(filepath.Join(root, ".system"))
	r, err = integrity.Run(t.Context(), vol, store.DB(), integrity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Healthy() || r.Missing != 1 || r.SizeMismatch != 1 || r.HashMismatch != 0 || r.Unreferenced != 1 || r.OrphanFiles != 1 || r.OrphanBytes != 5 || r.IncomingFiles != 1 {
		t.Fatalf("size check: %+v", r)
	}
	r, err = integrity.Run(t.Context(), vol, store.DB(), integrity.Options{Hash: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Missing != 1 || r.SizeMismatch != 1 || r.HashMismatch != 1 || len(r.Problems) != 3 {
		t.Fatalf("hash check: %+v", r)
	}
	for _, p := range r.Problems {
		if len(p.Assets) != 1 || p.Assets[0].FolderID != f.ID || p.Assets[0].Filename != "a.bin" {
			t.Fatalf("problem without its asset: %+v", p)
		}
	}
	var out bytes.Buffer
	r.WriteSummary(&out)
	for _, want := range []string{"PROBLEMS FOUND", "MISSING", "SIZE_MISMATCH", "HASH_MISMATCH", f.ID} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("summary lacks %q:\n%s", want, out.String())
		}
	}
	// Nothing was changed or moved.
	after, _ := os.ReadDir(filepath.Join(root, ".system"))
	if len(before) != len(after) {
		t.Fatal("verify changed .system")
	}
	if _, err := os.Stat(pool(stray)); err != nil {
		t.Fatal("verify moved an unreferenced file")
	}
}
