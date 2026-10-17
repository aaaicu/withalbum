package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestErrorClasses(t *testing.T) {
	pathErr := &fs.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}
	for _, tc := range []struct {
		err       error
		noSpace   bool
		ioFailure bool
	}{
		{pathErr, true, false},
		{fmt.Errorf("wrapped: %w", syscall.EDQUOT), true, false},
		{fmt.Errorf("%w: disk", ErrNoSpace), true, false},
		{&fs.PathError{Op: "read", Path: "x", Err: syscall.EIO}, false, true},
		{fmt.Errorf("%w: sqlite", ErrIO), false, true},
		{errors.New("other"), false, false},
		{nil, false, false},
	} {
		if got := IsNoSpace(tc.err); got != tc.noSpace {
			t.Errorf("IsNoSpace(%v) = %v", tc.err, got)
		}
		if got := IsIOError(tc.err); got != tc.ioFailure {
			t.Errorf("IsIOError(%v) = %v", tc.err, got)
		}
	}
}

func openRoot(t *testing.T) *Volume {
	t.Helper()
	root, _ := initRoot(t)
	v, err := Open(root, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFreeBytes(t *testing.T) {
	v := openRoot(t)
	free, err := v.FreeBytes()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if !errors.Is(err, ErrFreeSpaceUnsupported) {
			t.Fatalf("FreeBytes = %d, %v", free, err)
		}
		return
	}
	if err != nil || free == 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
	if _, err := freeBytes(filepath.Join(v.Root(), "missing")); err == nil {
		t.Fatal("freeBytes of a missing path succeeded")
	}
}

func TestInstanceLock(t *testing.T) {
	v := openRoot(t)
	l, err := v.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if _, err := v.Lock(); !errors.Is(err, ErrLocked) {
			t.Fatalf("second Lock = %v, want ErrLocked", err)
		}
	}
	b, err := os.ReadFile(filepath.Join(v.Root(), ".system", LockName))
	if err != nil || string(b) != fmt.Sprintf("%d\n", os.Getpid()) {
		t.Fatalf("lock file %q %v", b, err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	l2, err := v.Lock()
	if err != nil {
		t.Fatalf("Lock after Release: %v", err)
	}
	l2.Release()
}

func TestLockRefusesLostVolume(t *testing.T) {
	v := openRoot(t)
	if err := os.Remove(filepath.Join(v.Root(), MarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Lock(); err == nil {
		t.Fatal("Lock succeeded without marker")
	}
	if _, err := os.Lstat(filepath.Join(v.Root(), ".system")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".system created on a lost volume: %v", err)
	}
}
