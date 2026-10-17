package storage

import (
	"errors"
	"syscall"
)

var (
	// ErrNoSpace means the volume is full (ENOSPC, EDQUOT or SQLITE_FULL).
	ErrNoSpace = errors.New("storage volume is full")
	// ErrIO means the volume reported an I/O error (EIO or SQLITE_IOERR),
	// typically a failing or disconnected disk.
	ErrIO = errors.New("storage I/O error")
	// ErrFreeSpaceUnsupported means FreeBytes is not implemented on this
	// platform.
	ErrFreeSpaceUnsupported = errors.New("free space query is not supported on this platform")
)

// IsNoSpace reports whether err means the volume is out of space.
func IsNoSpace(err error) bool {
	return errors.Is(err, ErrNoSpace) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// IsIOError reports whether err is an I/O error of the volume.
func IsIOError(err error) bool {
	return errors.Is(err, ErrIO) || errors.Is(err, syscall.EIO)
}

// FreeBytes returns the space available to unprivileged writers on the
// volume. It returns ErrFreeSpaceUnsupported where the platform has no
// implementation.
func (v *Volume) FreeBytes() (uint64, error) { return freeBytes(v.root) }

// SyncDir fsyncs a directory so that renames and creations inside it are
// durable. It is a no-op where directories cannot be fsynced.
func SyncDir(dir string) error { return syncDir(dir) }
