package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func initRoot(t *testing.T) (string, Marker) {
	t.Helper()
	root := t.TempDir()
	m, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, m
}

func TestInitRequiresExistingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "unmounted", "WithAlbum")
	if _, err := Init(root); !errors.Is(err, ErrRootMissing) {
		t.Fatalf("Init = %v, want ErrRootMissing", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Init created the missing root (stat err %v)", err)
	}
	if _, err := os.Lstat(filepath.Dir(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Init created a parent of the missing root (stat err %v)", err)
	}
	if _, err := Open(root, OpenOptions{}); !errors.Is(err, ErrRootMissing) {
		t.Fatalf("Open = %v, want ErrRootMissing", err)
	}
}

func TestInitTwiceFails(t *testing.T) {
	root, first := initRoot(t)
	if _, err := Init(root); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init = %v, want ErrAlreadyInitialized", err)
	}
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID() != first.VolumeID {
		t.Fatalf("volume id changed: %s != %s", v.ID(), first.VolumeID)
	}
	fi, err := os.Stat(filepath.Join(root, MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("marker mode = %o, want 600", perm)
	}
	var m Marker
	b, _ := os.ReadFile(filepath.Join(root, MarkerName))
	if err := json.Unmarshal(b, &m); err != nil || m.Format != 1 || len(m.VolumeID) != 32 || m.CreatedAt == "" {
		t.Fatalf("unexpected marker %s (%v)", b, err)
	}
}

func TestOpenWithoutMarker(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root, OpenOptions{}); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Open = %v, want ErrNotInitialized", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Open wrote into an uninitialized root: %v", entries)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, OpenOptions{}); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("Open(file) = %v, want ErrNotDirectory", err)
	}
}

func TestOpenVolumeIDMismatch(t *testing.T) {
	root, m := initRoot(t)
	if _, err := Open(root, OpenOptions{ExpectedVolumeID: "00000000000000000000000000000000"}); !errors.Is(err, ErrVolumeMismatch) {
		t.Fatalf("Open = %v, want ErrVolumeMismatch", err)
	}
	if _, err := Open(root, OpenOptions{ExpectedVolumeID: m.VolumeID}); err != nil {
		t.Fatalf("Open with the right id: %v", err)
	}
}

func TestCheckDetectsMarkerRemoval(t *testing.T) {
	root, _ := initRoot(t)
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Check(); err != nil {
		t.Fatalf("Check on a healthy volume: %v", err)
	}
	if err := os.Remove(filepath.Join(root, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if err := v.Check(); err == nil {
		t.Fatal("Check passed after the marker was removed")
	}
}

func TestCheckDetectsRootReplaced(t *testing.T) {
	root := filepath.Join(t.TempDir(), "WithAlbum")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an unmount followed by something recreating the mount path.
	if err := os.Rename(root, root+".unmounted"); err != nil {
		t.Fatal(err)
	}
	if err := v.Check(); !errors.Is(err, ErrVolumeChanged) {
		t.Fatalf("Check with root gone = %v, want ErrVolumeChanged", err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	err = v.Check()
	if !errors.Is(err, ErrNotInitialized) && !errors.Is(err, ErrVolumeChanged) {
		t.Fatalf("Check with recreated root = %v, want failure", err)
	}
	// A different marker in the recreated directory must not be accepted either.
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := v.Check(); !errors.Is(err, ErrVolumeChanged) {
		t.Fatalf("Check with foreign marker = %v, want ErrVolumeChanged", err)
	}
}

func TestOpenReadOnlyRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, _ := initRoot(t)
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0700) })
	if _, err := Open(root, OpenOptions{}); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("Open = %v, want ErrNotWritable", err)
	}
}

func TestInitPreservesExistingData(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".system"), 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, ".system", "state.json")
	want := []byte(`{"folders":{"f1":{"id":"f1","name":"sample"}}}` + "\n")
	if err := os.WriteFile(state, want, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("state.json changed: %q", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("Init left extra files: %v", entries)
	}
	if _, err := Open(root, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, ".system")); len(entries) != 1 {
		t.Fatalf("Open left probe files in .system: %v", entries)
	}
}

// TestCheckStaysFailedAfterVolumeReturns: a volume that disappeared once is
// not trusted again by the same Volume, even when the same disk comes back
// at the same path; Open (a restart) is required.
func TestCheckStaysFailedAfterVolumeReturns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "WithAlbum")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Check(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+".detached"); err != nil {
		t.Fatal(err)
	}
	if err := v.Check(); !errors.Is(err, ErrVolumeChanged) {
		t.Fatalf("Check while detached = %v", err)
	}
	if err := os.Rename(root+".detached", root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := v.Check(); !errors.Is(err, ErrVolumeChanged) {
			t.Fatalf("Check after the volume returned = %v, want ErrVolumeChanged", err)
		}
	}
	if _, err := v.Lock(); !errors.Is(err, ErrVolumeChanged) {
		t.Fatalf("Lock after the volume returned = %v", err)
	}
	again, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Check(); err != nil {
		t.Fatalf("reopened Check = %v", err)
	}
}
