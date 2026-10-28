//go:build darwin || linux

package backup

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func finishedSnapshots(t *testing.T, target string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(target, SnapshotsDir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if snapshotIDRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestBackupKeepPrunesOldestSnapshots: -keep N leaves the N newest finished
// snapshots (the new one always among them), removes older ones only after
// the new one is complete, keeps the shared originals and never touches
// directories that are not finished snapshots.
func TestBackupKeepPrunesOldestSnapshots(t *testing.T) {
	l := newLibrary(t, 2)
	target := newTarget(t)
	snaps := filepath.Join(target, SnapshotsDir)
	first := l.backup(t, target, Options{})
	// Older snapshots by name (as a backup from 2020 would be named).
	for _, id := range []string{"20200101T000000Z-aaaaaa", "20200102T000000Z-bbbbbb"} {
		if err := os.Mkdir(filepath.Join(snaps, id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snaps, id, ManifestName), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Not finished snapshots: left alone.
	keepAlone := []string{"20200103T000000Z-cccccc", "notes"}
	for _, d := range keepAlone {
		if err := os.Mkdir(filepath.Join(snaps, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	poolBefore := treeOf(t, filepath.Join(target, "pool"))

	// A failed backup removes nothing.
	l.add(t, photo(t, 50))
	if _, err := Backup(t.Context(), l.vol, l.store.DB(), target, Options{Keep: 1, hook: func(p string) error {
		if p == PointBeforeFinalize {
			return errors.New("crash")
		}
		return nil
	}}); err == nil {
		t.Fatal("hooked backup succeeded")
	}
	if got := finishedSnapshots(t, target); len(got) != 4 {
		t.Fatalf("a failed backup pruned: %v", got)
	}

	r := l.backup(t, target, Options{Keep: 2})
	if len(r.Pruned) != 2 || r.Pruned[0] != "20200101T000000Z-aaaaaa" || r.Pruned[1] != "20200102T000000Z-bbbbbb" || r.PruneError != "" {
		t.Fatalf("pruned %v (%s)", r.Pruned, r.PruneError)
	}
	got := finishedSnapshots(t, target)
	want := []string{"20200103T000000Z-cccccc", first.ID, r.ID}
	sort.Strings(want)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("snapshots %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(snaps, "notes")); err != nil {
		t.Fatal("pruning removed an unrelated directory")
	}
	for _, id := range []string{first.ID, r.ID} {
		if _, err := Restore(t.Context(), filepath.Join(snaps, id), "", RestoreOptions{CheckOnly: true}); err != nil {
			t.Fatalf("kept snapshot %s: %v", id, err)
		}
	}
	poolAfter := treeOf(t, filepath.Join(target, "pool"))
	for k, v := range poolBefore {
		if poolAfter[k] != v {
			t.Fatalf("pool file %s changed or removed by pruning", k)
		}
	}

	// Keep 1: only the new snapshot remains of the finished ones.
	r3 := l.backup(t, target, Options{Keep: 1})
	if len(r3.Pruned) != 2 {
		t.Fatalf("keep 1 pruned %v", r3.Pruned)
	}
	got = finishedSnapshots(t, target)
	if len(got) != 2 || (got[0] != r3.ID && got[1] != r3.ID) {
		t.Fatalf("after keep 1: %v (new %s)", got, r3.ID)
	}
	// An interrupted prune leaves a .partial- directory; the next backup
	// removes it.
	leftover := filepath.Join(snaps, partialPrefix+"pruned-20200104T000000Z-dddddd")
	if err := os.MkdirAll(filepath.Join(leftover, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	l.backup(t, target, Options{})
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover of an interrupted prune: %v", err)
	}
}
