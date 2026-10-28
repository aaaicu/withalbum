//go:build darwin || linux

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupTargetMarker(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenBackupTarget(dir, true); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("open before init: %v", err)
	}
	m, err := InitBackupTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackupTarget(dir); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second init: %v", err)
	}
	// A backup target is not a storage root and cannot become one.
	if _, err := Open(dir, OpenOptions{}); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("open backup target as storage: %v", err)
	}
	if _, err := Init(dir); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("init storage in a backup target: %v", err)
	}
	v, err := OpenBackupTarget(dir, false)
	if err != nil || v.ID() != m.VolumeID {
		t.Fatalf("open: %v", err)
	}
	if err := v.Check(); err != nil {
		t.Fatal(err)
	}
	// Its lock lives in the directory itself; no .system is created.
	lock, err := v.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	lock.Release()
	if _, err := os.Stat(filepath.Join(dir, ".system")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(".system created in a backup target")
	}
	os.Remove(filepath.Join(dir, BackupMarkerName))
	if err := v.Check(); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("check without marker: %v", err)
	}
}

func TestOpenRefusesPendingRestore(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, RestoreJournalName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, OpenOptions{}); !errors.Is(err, ErrRestorePending) {
		t.Fatalf("open: %v", err)
	}
	if _, err := Open(root, OpenOptions{AllowPendingRestore: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSameDeviceAndLockFile(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	same, known, err := SameDevice(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if known && !same {
		t.Fatal("two temp directories on different devices")
	}
	if _, _, err := SameDevice(a, filepath.Join(b, "missing")); err == nil {
		t.Fatal("missing path accepted")
	}
	p := filepath.Join(a, "x.lock")
	l, err := LockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockFile(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	l.Release()
	l, err = LockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}
