package blobstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// stored promotes body into the pool and returns its hash.
func stored(t *testing.T, s *Store, body []byte) string {
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

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// TestDetachOriginals: detached files leave the pool for .system/purge and
// undo puts them back; a missing pool file is skipped; a bad hash moves
// nothing.
func TestDetachOriginals(t *testing.T) {
	s, _ := newTestStore(t)
	a, b := stored(t, s, []byte("first original")), stored(t, s, []byte("second original"))
	missing := "00" + a[2:]
	undo, err := s.DetachOriginals([]string{a, b, missing})
	if err != nil {
		t.Fatal(err)
	}
	if exists(s.PoolPath(a)) || exists(s.PoolPath(b)) || !exists(s.purgePath(a)) || !exists(s.purgePath(b)) {
		t.Fatal("not detached")
	}
	undo()
	if !exists(s.PoolPath(a)) || !exists(s.PoolPath(b)) || exists(s.purgePath(a)) {
		t.Fatal("undo did not put them back")
	}
	if _, err := s.DetachOriginals([]string{a, "../../etc"}); err == nil {
		t.Fatal("bad hash accepted")
	}
	if !exists(s.PoolPath(a)) {
		t.Fatal("a failed detach left a file out of the pool")
	}
}

// TestRecoverPurged: files whose rows survive go back to the pool (or to
// quarantine when the pool holds the name again, never deleted); files of
// committed purges are deleted with their preview and playback copy.
func TestRecoverPurged(t *testing.T) {
	s, _ := newTestStore(t)
	keep, drop, twice := stored(t, s, []byte("keep me")), stored(t, s, []byte("drop me")), stored(t, s, []byte("twice"))
	for _, p := range []string{s.PreviewPath(drop), s.PlaybackPath(drop)} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("derived"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DetachOriginals([]string{keep, drop, twice}); err != nil {
		t.Fatal(err)
	}
	stored(t, s, []byte("twice")) // uploaded again meanwhile
	rows := map[string]bool{keep: true, twice: true}
	r, err := s.RecoverPurged(context.Background(), func(sha string) (bool, error) { return rows[sha], nil })
	if err != nil || r.Restored != 2 || r.Removed != 1 {
		t.Fatalf("recover %+v %v", r, err)
	}
	if !exists(s.PoolPath(keep)) || !exists(s.PoolPath(twice)) || exists(s.PoolPath(drop)) {
		t.Fatal("pool after recovery")
	}
	for _, p := range []string{s.purgePath(drop), s.PreviewPath(drop), s.PlaybackPath(drop), s.purgePath(twice)} {
		if exists(p) {
			t.Fatalf("%s left", p)
		}
	}
	orphans, _ := filepath.Glob(filepath.Join(s.root, ".system", "orphans", twice+".purge-duplicate.*"))
	if len(orphans) != 1 {
		t.Fatalf("duplicate not quarantined: %v", orphans)
	}
}
