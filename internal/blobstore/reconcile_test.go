package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/storage"
)

// fakeIndex lists blobs in sha order like the metadata database does.
type fakeIndex map[string]int64

func (f fakeIndex) ForEachBlob(ctx context.Context, fn func(string, int64) error) error {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(k, f[k]); err != nil {
			return err
		}
	}
	return nil
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func putPool(t *testing.T, s *Store, body []byte) string {
	t.Helper()
	f, err := s.Stage(bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(f, f.SHA256); err != nil {
		t.Fatal(err)
	}
	return f.SHA256
}

func writeFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile(t *testing.T) {
	s, _ := newTestStore(t)
	sys := filepath.Join(s.root, ".system")
	kept := putPool(t, s, []byte("referenced"))
	orphan := putPool(t, s, []byte("crashed before commit"))
	damaged := putPool(t, s, []byte("size differs"))
	missing := shaOf([]byte("lost original"))
	index := fakeIndex{kept: 10, damaged: 999, missing: 13}

	writeFile(t, filepath.Join(sys, "incoming", "upload-123"), "partial")
	writeFile(t, filepath.Join(sys, "incoming", "upload-456"), "")
	writeFile(t, filepath.Join(sys, "incoming", "notes.txt"), "not ours")
	writeFile(t, filepath.Join(sys, "previews", "ab", "preview-789"), "partial jpeg")
	writeFile(t, filepath.Join(sys, "previews", "ab", strings.Repeat("ab", 32)+".jpg"), "finished preview")
	writeFile(t, filepath.Join(sys, "pool", "zz", "x"), "odd dir")
	writeFile(t, filepath.Join(sys, "pool", kept[:2], "short"), "odd name")
	wrongPrefix := shaOf([]byte("wrong prefix"))
	writeFile(t, filepath.Join(sys, "pool", "00", wrongPrefix), "misplaced")
	if wrongPrefix[:2] == "00" {
		t.Skip("improbable hash prefix")
	}

	r, err := s.Reconcile(t.Context(), index)
	if err != nil {
		t.Fatal(err)
	}
	if r.Blobs != 3 || r.IncomingRemoved != 2 || r.PreviewTempsRemoved != 1 || r.Orphans != 1 || r.Missing != 1 || r.SizeMismatch != 1 || r.Unexpected != 4 {
		t.Fatalf("report %+v", r)
	}
	if r.OrphanSample[0] != orphan || r.MissingSample[0] != missing || r.SizeMismatchSample[0] != damaged {
		t.Fatalf("samples %+v", r)
	}
	if r.Healthy() || !r.Changed() || !strings.Contains(r.Summary(), "orphans_quarantined=1") {
		t.Fatalf("healthy %v changed %v %s", r.Healthy(), r.Changed(), r.Summary())
	}
	if strings.Contains(r.Summary(), s.root) {
		t.Fatal("summary leaks the storage path")
	}
	// The orphan was moved, byte for byte; nothing else was touched.
	if _, err := os.Stat(s.PoolPath(orphan)); !os.IsNotExist(err) {
		t.Fatalf("orphan still in pool: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(sys, "orphans"))
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), orphan+".orphan.") {
		t.Fatalf("orphans %v", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(sys, "orphans", entries[0].Name())); string(b) != "crashed before commit" {
		t.Fatalf("orphan content %q", b)
	}
	for _, p := range []string{s.PoolPath(kept), s.PoolPath(damaged), filepath.Join(sys, "incoming", "notes.txt"),
		filepath.Join(sys, "previews", "ab", strings.Repeat("ab", 32)+".jpg"), filepath.Join(sys, "pool", "00", wrongPrefix)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}

	// A second run changes nothing and reports the same problems.
	r, err = s.Reconcile(t.Context(), index)
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed() || r.Missing != 1 || r.SizeMismatch != 1 || r.Unexpected != 4 {
		t.Fatalf("second report %+v", r)
	}
}

func TestReconcileCleanStore(t *testing.T) {
	s, _ := newTestStore(t)
	a := putPool(t, s, []byte("one"))
	b := putPool(t, s, []byte("two"))
	r, err := s.Reconcile(t.Context(), fakeIndex{a: 3, b: 3})
	if err != nil || !r.Healthy() || r.Changed() || r.Blobs != 2 {
		t.Fatalf("report %+v %v", r, err)
	}
}

func TestReconcileRefusesLostVolume(t *testing.T) {
	s, vol := newTestStore(t)
	orphan := putPool(t, s, []byte("orphan"))
	writeFile(t, filepath.Join(s.root, ".system", "incoming", "upload-1"), "x")
	if err := os.Remove(filepath.Join(vol.Root(), storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(t.Context(), fakeIndex{}); err == nil {
		t.Fatal("Reconcile ran without a volume marker")
	}
	if _, err := os.Stat(s.PoolPath(orphan)); err != nil {
		t.Fatalf("pool changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, ".system", "incoming", "upload-1")); err != nil {
		t.Fatalf("incoming changed: %v", err)
	}
}
