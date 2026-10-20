package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// fakeDisks makes diskUUID answer from a map of resolved root -> UUID
// (missing: "" unknown) for the duration of the test.
func fakeDisks(t *testing.T, disks map[string]string) {
	t.Helper()
	old := diskUUID
	diskUUID = func(path string) (string, error) {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
		return disks[resolved], nil
	}
	t.Cleanup(func() { diskUUID = old })
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func markerOf(t *testing.T, root string) Marker {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func copyMarker(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(from, MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, MarkerName), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInitRecordsDiskUUID(t *testing.T) {
	root := resolvedTempDir(t)
	fakeDisks(t, map[string]string{root: "AAAAAAAA-0000-0000-0000-000000000001"})
	m, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.DiskUUID != "AAAAAAAA-0000-0000-0000-000000000001" || markerOf(t, root).DiskUUID != m.DiskUUID {
		t.Fatalf("marker disk uuid = %q / %q", m.DiskUUID, markerOf(t, root).DiskUUID)
	}
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur, rec := v.DiskUUID(); cur != m.DiskUUID || rec != m.DiskUUID {
		t.Fatalf("DiskUUID = %q, %q", cur, rec)
	}
	// Backup targets do not record one.
	bt := resolvedTempDir(t)
	if bm, err := InitBackupTarget(bt); err != nil || bm.DiskUUID != "" {
		t.Fatalf("backup target marker %+v, %v", bm, err)
	}
}

func TestCopiedMarkerOnAnotherDiskIsRefused(t *testing.T) {
	orig, copyRoot := resolvedTempDir(t), resolvedTempDir(t)
	fakeDisks(t, map[string]string{orig: "11111111-1111-1111-1111-111111111111", copyRoot: "22222222-2222-2222-2222-222222222222"})
	if _, err := Init(orig); err != nil {
		t.Fatal(err)
	}
	copyMarker(t, orig, copyRoot)
	if _, err := Open(copyRoot, OpenOptions{}); !errors.Is(err, ErrDiskMismatch) {
		t.Fatalf("Open(copy) = %v, want ErrDiskMismatch", err)
	}
	// The pinned volume id does not override the disk check.
	id := markerOf(t, orig).VolumeID
	if _, err := Open(copyRoot, OpenOptions{ExpectedVolumeID: id}); !errors.Is(err, ErrDiskMismatch) {
		t.Fatalf("Open(copy, pinned id) = %v, want ErrDiskMismatch", err)
	}
	if _, err := Open(orig, OpenOptions{ExpectedVolumeID: id}); err != nil {
		t.Fatalf("Open(original) = %v", err)
	}
	// The operator's explicit decision (-adopt-disk) rebinds the copy.
	v, err := Open(copyRoot, OpenOptions{IgnoreDiskUUID: true})
	if err != nil {
		t.Fatal(err)
	}
	old, cur, err := v.RebindDiskUUID()
	if err != nil || old != "11111111-1111-1111-1111-111111111111" || cur != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("RebindDiskUUID = %q, %q, %v", old, cur, err)
	}
	if m := markerOf(t, copyRoot); m.DiskUUID != cur || m.VolumeID != id {
		t.Fatalf("rebound marker %+v", m)
	}
	if _, err := Open(copyRoot, OpenOptions{}); err != nil {
		t.Fatalf("Open after rebind = %v", err)
	}
	// Moving the original marker to the copy's disk is refused now as well.
	copyMarker(t, orig, copyRoot)
	if _, err := Open(copyRoot, OpenOptions{}); !errors.Is(err, ErrDiskMismatch) {
		t.Fatalf("Open = %v, want ErrDiskMismatch", err)
	}
}

func TestDiskUUIDIsCaseInsensitive(t *testing.T) {
	root := resolvedTempDir(t)
	disks := map[string]string{root: "abcdef01-0000-0000-0000-000000000000"}
	fakeDisks(t, disks)
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	disks[root] = "ABCDEF01-0000-0000-0000-000000000000"
	if _, err := Open(root, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordedDiskUUIDUnreadableFailsClosed(t *testing.T) {
	root := resolvedTempDir(t)
	disks := map[string]string{root: "33333333-3333-3333-3333-333333333333"}
	fakeDisks(t, disks)
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	delete(disks, root)
	if _, err := Open(root, OpenOptions{}); !errors.Is(err, ErrDiskUnknown) {
		t.Fatalf("Open = %v, want ErrDiskUnknown", err)
	}
	old := diskUUID
	diskUUID = func(string) (string, error) { return "", errors.New("boom") }
	defer func() { diskUUID = old }()
	if _, err := Open(root, OpenOptions{}); !errors.Is(err, ErrDiskUnknown) {
		t.Fatalf("Open = %v, want ErrDiskUnknown", err)
	}
}

func TestLegacyMarkerAdoptsDiskUUID(t *testing.T) {
	root := resolvedTempDir(t)
	disks := map[string]string{}
	fakeDisks(t, disks)
	first, err := Init(root) // disk unknown: a marker like the ones before disk_uuid
	if err != nil {
		t.Fatal(err)
	}
	if first.DiskUUID != "" {
		t.Fatalf("unknown disk recorded %q", first.DiskUUID)
	}
	raw, _ := os.ReadFile(filepath.Join(root, MarkerName))
	if regexp.MustCompile(`disk_uuid`).Match(raw) {
		t.Fatalf("empty disk_uuid written: %s", raw)
	}
	// Unknown disk: nothing to adopt.
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.AdoptDiskUUID(); err != nil || got != "" {
		t.Fatalf("AdoptDiskUUID(unknown) = %q, %v", got, err)
	}
	disks[root] = "44444444-4444-4444-4444-444444444444"
	v, err = Open(root, OpenOptions{})
	if err != nil {
		t.Fatalf("legacy marker refused: %v", err)
	}
	got, err := v.AdoptDiskUUID()
	if err != nil || got != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("AdoptDiskUUID = %q, %v", got, err)
	}
	m := markerOf(t, root)
	if m.DiskUUID != got || m.VolumeID != first.VolumeID || m.CreatedAt != first.CreatedAt || m.Format != 1 {
		t.Fatalf("adopted marker %+v, before %+v", m, first)
	}
	if fi, err := os.Stat(filepath.Join(root, MarkerName)); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("marker mode %v, %v", fi.Mode(), err)
	}
	if err := v.Check(); err != nil {
		t.Fatalf("Check after adoption: %v", err)
	}
	if again, err := v.AdoptDiskUUID(); err != nil || again != "" {
		t.Fatalf("second AdoptDiskUUID = %q, %v", again, err)
	}
	// From now on another disk is refused.
	other := resolvedTempDir(t)
	disks[other] = "55555555-5555-5555-5555-555555555555"
	copyMarker(t, root, other)
	if _, err := Open(other, OpenOptions{}); !errors.Is(err, ErrDiskMismatch) {
		t.Fatalf("Open(copy) = %v, want ErrDiskMismatch", err)
	}
}

func TestPlatformDiskUUID(t *testing.T) {
	dir := t.TempDir()
	u, err := platformDiskUUID(dir)
	if err != nil {
		t.Fatalf("platformDiskUUID: %v", err)
	}
	if runtime.GOOS == "darwin" {
		if !regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`).MatchString(u) {
			t.Fatalf("macOS volume UUID %q", u)
		}
		// Every directory of the same file system has the same UUID.
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0700); err != nil {
			t.Fatal(err)
		}
		if u2, err := platformDiskUUID(sub); err != nil || u2 != u {
			t.Fatalf("subdirectory UUID %q (%v), root %q", u2, err, u)
		}
	}
	if _, err := platformDiskUUID(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing path: no error")
	}
}
