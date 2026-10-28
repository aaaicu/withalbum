package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aaaicu/withalbum/internal/storage"
)

// copyVerified copies the regular file src into a new temp file in tmpDir
// while hashing it, fsyncs it, and returns the temp path, size and SHA-256.
// The caller renames the temp file into place or removes it.
func copyVerified(src, tmpDir string) (tmp string, size int64, sum string, err error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return "", 0, "", err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, "", fmt.Errorf("%s is not a regular file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", 0, "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(tmpDir, "copy-*")
	if err != nil {
		return "", 0, "", err
	}
	fail := func(err error) (string, int64, string, error) {
		out.Close()
		os.Remove(out.Name())
		return "", 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", 0, "", err
	}
	return out.Name(), n, hex.EncodeToString(h.Sum(nil)), nil
}

// place makes tmp read-only (when readOnly) and renames it to dst, creating
// the parent directory of dst (one level, never MkdirAll) and fsyncing the
// directories involved.
func place(tmp, dst string, readOnly bool) error {
	dir := filepath.Dir(dst)
	_, statErr := os.Lstat(dir)
	if err := storage.MkdirIn(dir); err != nil {
		return err
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	if readOnly {
		_ = os.Chmod(tmp, 0400) // ignored by exFAT and similar
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return syncDir(dir)
}

// hashRegular returns the size and SHA-256 of a regular file.
func hashRegular(path string) (int64, string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	if !fi.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// writeJSONAtomic writes v as indented JSON to path (mode 0600) through a
// temp file, fsync and rename, then fsyncs the directory.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir fsyncs a directory; file systems that cannot are tolerated.
func syncDir(dir string) error {
	err := storage.SyncDir(dir)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// onlyLockFile reports whether dir holds nothing but empty directories and,
// at its top level, the instance lock file.
func onlyLockFile(dir string) (bool, error) {
	empty := true
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Dir(path) == dir && d.Name() == storage.LockName && d.Type().IsRegular() {
			return nil
		}
		empty = false
		return filepath.SkipAll
	})
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return empty, err
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

func isSHA(s string) bool { return len(s) == 64 && isHex(s) }
