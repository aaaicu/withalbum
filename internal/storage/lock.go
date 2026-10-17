package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// LockName is the instance lock file inside .system.
const LockName = "withalbum.lock"

// ErrLocked means another WithAlbum process (a server or migrate-legacy
// -apply) holds the instance lock of this volume.
var ErrLocked = errors.New("storage is in use by another WithAlbum process")

// InstanceLock is an exclusive advisory lock on .system/withalbum.lock. The
// operating system releases it when the process exits, including on SIGKILL.
type InstanceLock struct{ f *os.File }

// Lock takes the instance lock of the volume without waiting. It creates
// .system (never the root) and the lock file if they do not exist, and
// records the process id in the file for operators. Where advisory locks are
// not implemented it succeeds without locking.
func (v *Volume) Lock() (*InstanceLock, error) {
	if err := v.Check(); err != nil {
		return nil, err
	}
	sys := filepath.Join(v.root, ".system")
	if err := MkdirIn(sys); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(sys, LockName), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	// Best effort: the pid only helps an operator find the other process.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &InstanceLock{f: f}, nil
}

// Release drops the lock. The lock file itself is kept: removing it would
// let a second process lock a new file while a third still holds the old one.
func (l *InstanceLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("release instance lock: %w", err)
	}
	return nil
}
