package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aaaicu/withalbum/internal/storage"
)

// Partial uploads of resumable upload sessions live in .system/uploads as
// "<upload_id>.part" (mode 0600). The metadata database records how many of
// their bytes are complete; a file may be longer after a crash (bytes
// written but not recorded) and is cut back to the recorded length before
// the next append. Callers serialize the operations on one upload ID.

// UploadsRelDir holds the partial files of upload sessions.
const UploadsRelDir = ".system/uploads"

const partSuffix = ".part"

var uploadIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Part errors.
var (
	// ErrPartShort: the partial file holds fewer bytes than recorded (lost
	// or damaged); the upload must start again at 0.
	ErrPartShort = errors.New("partial upload is shorter than recorded")
	// ErrPartMismatch: the completed file does not have the declared size
	// and SHA-256.
	ErrPartMismatch = errors.New("uploaded bytes do not match the declared size and SHA-256")
	// ErrPartMissing: neither the partial file nor a matching pool file
	// exists.
	ErrPartMissing = errors.New("partial upload is missing")
)

func (s *Store) partPath(id string) (string, error) {
	if !uploadIDRE.MatchString(id) {
		return "", fmt.Errorf("blobstore: invalid upload id %q", id)
	}
	return filepath.Join(s.root, filepath.FromSlash(UploadsRelDir), id+partSuffix), nil
}

// AppendResult reports what AppendPart wrote.
type AppendResult struct {
	Written int64  // bytes appended at the offset and fsynced
	SHA256  string // lowercase hex of the appended bytes
}

// AppendPart appends at most limit bytes from r to the partial file of
// upload id, at offset (the recorded length). A longer file is truncated to
// offset first; a shorter one is ErrPartShort. The file is created when
// offset is 0 and it does not exist.
//
// Whatever was appended is fsynced before AppendPart returns, also when
// reading r failed (ErrCopy, with Written > 0 possible), so the caller may
// record it. On a write or sync error the file is cut back to offset. More
// than limit bytes in r is ErrTooLarge, with the file cut back to offset.
func (s *Store) AppendPart(id string, offset int64, r io.Reader, limit int64) (AppendResult, error) {
	path, err := s.partPath(id)
	if err != nil {
		return AppendResult{}, err
	}
	f, created, err := openPart(path, offset)
	if err != nil {
		return AppendResult{}, err
	}
	defer f.Close()
	if created {
		if err := storage.SyncDir(filepath.Dir(path)); err != nil {
			return AppendResult{}, err
		}
	}
	fi, err := f.Stat()
	if err != nil {
		return AppendResult{}, err
	}
	switch {
	case fi.Size() < offset:
		return AppendResult{}, ErrPartShort
	case fi.Size() > offset:
		if err := f.Truncate(offset); err != nil {
			return AppendResult{}, fmt.Errorf("%w: %w", ErrWrite, err)
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return AppendResult{}, err
	}
	cutBack := func(cause error) (AppendResult, error) {
		if err := f.Truncate(offset); err == nil {
			_ = f.Sync()
		}
		return AppendResult{}, cause
	}
	h := sha256.New()
	file := &errWriter{w: f}
	n, copyErr := io.Copy(io.MultiWriter(file, h), io.LimitReader(r, limit+1))
	if file.err != nil {
		return cutBack(fmt.Errorf("%w: %w", ErrWrite, file.err))
	}
	if n > limit {
		return cutBack(ErrTooLarge)
	}
	if err := f.Sync(); err != nil {
		return cutBack(fmt.Errorf("%w: %w", ErrSync, err))
	}
	res := AppendResult{Written: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	if copyErr != nil {
		return res, fmt.Errorf("%w: %w", ErrCopy, copyErr)
	}
	return res, nil
}

// openPart opens the partial file for writing, creating it only at offset 0.
func openPart(path string, offset int64) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, false, fmt.Errorf("%w: %w", ErrTempCreate, err)
		}
		return f, false, nil
	}
	if offset != 0 {
		return nil, false, ErrPartShort
	}
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrTempCreate, err)
	}
	return f, true, nil
}

// TruncatePart cuts the partial file of upload id back to size (a missing
// file is fine) and fsyncs it.
func (s *Store) TruncatePart(id string, size int64) error {
	path, err := s.partPath(id)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

// RemovePart deletes the partial file of upload id, if any.
func (s *Store) RemovePart(id string) error {
	path, err := s.partPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FinishPart checks a completed upload against its declared size and
// SHA-256 by hashing the file on disk, and returns it as a StagedFile for
// Promote (which moves it into the pool) with its sniffed type. When the
// partial file is gone because an earlier completion already moved it into
// the pool and then failed to commit, the pool file is checked and
// returned instead (Promote then reuses it). ErrPartMismatch,
// ErrPartMissing.
func (s *Store) FinishPart(id, sha string, size int64) (*StagedFile, error) {
	path, err := s.partPath(id)
	if err != nil {
		return nil, err
	}
	check := func(p string) (*StagedFile, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() || fi.Size() != size {
			return nil, ErrPartMismatch
		}
		got, head, err := hashHead(p)
		if err != nil {
			return nil, err
		}
		if got != sha {
			return nil, ErrPartMismatch
		}
		return &StagedFile{Path: path, SHA256: sha, Size: size, MimeType: SniffMIME(head)}, nil
	}
	f, err := check(path)
	if !errors.Is(err, os.ErrNotExist) {
		return f, err
	}
	f, err = check(s.PoolPath(sha))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrPartMismatch) {
		return nil, ErrPartMissing
	}
	return f, err
}

// hashHead returns the SHA-256 of the file and its first 512 bytes.
func hashHead(path string) (string, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	h := sha256.New()
	sniff := &sniffWriter{}
	if _, err := io.Copy(io.MultiWriter(h, sniff), file); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), sniff.buf, nil
}

// PartInfo is a partial file found in .system/uploads.
type PartInfo struct {
	UploadID string
	Size     int64
}

// ListParts returns the partial files in .system/uploads, and the names
// of other entries (left alone, for the log).
func (s *Store) ListParts() ([]PartInfo, []string, error) {
	dir := filepath.Join(s.root, filepath.FromSlash(UploadsRelDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var parts []PartInfo
	var other []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), partSuffix)
		if !ok || !uploadIDRE.MatchString(id) || !e.Type().IsRegular() {
			other = append(other, e.Name())
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, PartInfo{UploadID: id, Size: fi.Size()})
	}
	return parts, other, nil
}
