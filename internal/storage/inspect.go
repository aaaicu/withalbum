package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Inspection helpers for choosing a storage root (the setup screen). They
// only read, except ProbeWritable (a temporary file that is removed again)
// and MkdirOnMount (exactly one new directory on a disk that was checked to
// be mounted). Nothing here creates a storage root on its own.

// FSInfo describes the file system holding a directory. Fields are zero
// where the platform does not report them (Known is then false).
type FSInfo struct {
	Known      bool
	Type       string // e.g. "apfs", "exfat", "ext4"; "" when unknown
	TotalBytes uint64
	FreeBytes  uint64 // available to unprivileged writers
	ReadOnly   bool
	Local      bool // false for network file systems
}

// ErrInspectUnsupported means StatFS is not implemented on this platform.
var ErrInspectUnsupported = errors.New("file system information is not supported on this platform")

// StatFS reports the file system of path.
func StatFS(path string) (FSInfo, error) { return statFS(path) }

// DeviceOf returns the device number (st_dev) of an existing path. ok is
// false where the platform does not report device numbers.
func DeviceOf(path string) (dev uint64, ok bool, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	dev, ok = devOf(fi)
	return dev, ok, nil
}

// IsMountPoint reports whether dir is the root of a mounted file system,
// i.e. lives on another device than its parent. A directory that merely
// exists under /Volumes after its disk was unplugged is not a mount point.
// It fails closed (false with an error) where device numbers are unknown.
func IsMountPoint(dir string) (bool, error) {
	resolved, err := resolveDir(dir)
	if err != nil {
		return false, err
	}
	parent := filepath.Dir(resolved)
	if parent == resolved {
		return true, nil // the file system root
	}
	d, ok, err := DeviceOf(resolved)
	if err != nil {
		return false, err
	}
	p, okP, err := DeviceOf(parent)
	if err != nil {
		return false, err
	}
	if !ok || !okP {
		return false, errors.New("device numbers are not available on this platform")
	}
	return d != p, nil
}

// MountEntry is one entry of a mounts directory such as /Volumes.
type MountEntry struct {
	Name    string // the entry name, e.g. "T7"
	Path    string // symlink-resolved path
	Mounted bool   // the entry is a mount point (see IsMountPoint)
	Device  uint64 // st_dev of Path (0 when unknown)
}

// ListMounts lists the directories directly inside dir (for example
// /Volumes), resolved through symlinks and sorted by name. Hidden entries
// and entries that are not directories are skipped.
func ListMounts(dir string) ([]MountEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []MountEntry
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "com.apple.") {
			continue
		}
		resolved, err := resolveDir(filepath.Join(dir, name))
		if err != nil {
			continue // dangling symlink, file, unreadable: not a candidate
		}
		m := MountEntry{Name: name, Path: resolved}
		m.Mounted, _ = IsMountPoint(resolved)
		m.Device, _, _ = DeviceOf(resolved)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ProbeWritable creates, fsyncs and removes a temporary file directly in
// dir (the "write test" of the setup screen). It never creates dir.
func ProbeWritable(dir string) error {
	resolved, err := resolveDir(dir)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(resolved, ".withalbum-probe-*")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotWritable, err)
	}
	name := f.Name()
	_, werr := f.Write([]byte{0})
	serr := f.Sync()
	cerr := f.Close()
	rerr := os.Remove(name)
	if err := errors.Join(werr, serr, cerr, rerr); err != nil {
		return fmt.Errorf("%w: %w", ErrNotWritable, err)
	}
	return nil
}

// Contents classifies what an existing directory holds, for choosing a root.
type Contents string

const (
	ContentsEmpty        Contents = "empty"        // nothing (hidden system files aside)
	ContentsOther        Contents = "other"        // unrelated files; Init leaves them alone
	ContentsRegistered   Contents = "registered"   // an initialized WithAlbum storage root
	ContentsUnregistered Contents = "unregistered" // WithAlbum data (.system) without a marker
	ContentsBackupTarget Contents = "backup"       // a registered backup target, never a root
)

// ReadContents classifies dir without changing it.
func ReadContents(dir string) (Contents, error) {
	resolved, err := resolveDir(dir)
	if err != nil {
		return "", err
	}
	exists := func(name string) (bool, error) {
		_, err := os.Lstat(filepath.Join(resolved, name))
		if err == nil {
			return true, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if ok, err := exists(BackupMarkerName); err != nil || ok {
		return ContentsBackupTarget, err
	}
	if ok, err := exists(MarkerName); err != nil || ok {
		return ContentsRegistered, err
	}
	if ok, err := exists(".system"); err != nil || ok {
		return ContentsUnregistered, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		switch e.Name() {
		case ".DS_Store", ".Spotlight-V100", ".fseventsd", ".Trashes", ".TemporaryItems", ".DocumentRevisions-V100":
			continue
		}
		return ContentsOther, nil
	}
	return ContentsEmpty, nil
}

// MkdirOnMount creates the single directory name directly inside mount and
// returns its path. mount must be a mount point (IsMountPoint), re-checked
// here immediately before creating, so an unplugged disk whose path was left
// behind on the internal disk is refused. name must be a plain directory
// name. An existing directory is returned unchanged.
func MkdirOnMount(mount, name string) (string, error) {
	if err := checkFolderName(name); err != nil {
		return "", err
	}
	ok, err := IsMountPoint(mount)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrNotMounted
	}
	return mkdirOne(mount, name)
}

// MkdirOne creates the single directory name directly inside an existing
// parent (no mount check: for the home folder on the system disk). An
// existing directory is returned unchanged.
func MkdirOne(parent, name string) (string, error) {
	if err := checkFolderName(name); err != nil {
		return "", err
	}
	return mkdirOne(parent, name)
}

// ErrNotMounted means a directory chosen as a disk is not a mount point.
var ErrNotMounted = errors.New("the selected disk is not mounted")

func checkFolderName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid folder name %q", name)
	}
	return nil
}

func mkdirOne(parent, name string) (string, error) {
	resolved, err := resolveDir(parent)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(resolved, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", ErrNotDirectory
		}
		return dir, nil
	}
	if err := syncDir(resolved); err != nil {
		return "", err
	}
	return dir, nil
}

// IsDir reports whether path is an existing directory (following symlinks).
func IsDir(path string) (bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.IsDir(), nil
}

// ResolveDir returns the absolute, symlink-resolved path of an existing
// directory (ErrRootMissing, ErrNotDirectory otherwise).
func ResolveDir(path string) (string, error) { return resolveDir(path) }
