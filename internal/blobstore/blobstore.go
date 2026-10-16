// Package blobstore stores original files content-addressed by SHA-256 under
// .system/pool and their JPEG previews under .system/previews.
//
// Uploads are first written to .system/incoming (Stage), then moved into the
// pool with an atomic rename (Promote). Callers must verify the storage
// volume (storage.Volume.Check) before calling Stage or Promote.
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aaaicu/withalbum/internal/storage"
)

// Stage errors. Errors other than ErrTooLarge and ErrEmpty wrap the
// underlying error.
var (
	ErrTempCreate = errors.New("cannot create temp file")
	ErrCopy       = errors.New("cannot copy upload")
	ErrTooLarge   = errors.New("upload exceeds limit")
	ErrEmpty      = errors.New("upload is empty")
	ErrSync       = errors.New("cannot sync temp file")
	ErrClose      = errors.New("cannot close temp file")
)

// Store is safe for concurrent use.
type Store struct {
	root string
}

// New creates .system/incoming, .system/pool and .system/previews inside a
// verified volume. It never creates the volume root or any missing parent.
func New(vol *storage.Volume) (*Store, error) {
	if err := vol.Check(); err != nil {
		return nil, err
	}
	root := vol.Root()
	for _, p := range []string{".system", ".system/incoming", ".system/pool", ".system/previews"} {
		if err := storage.MkdirIn(filepath.Join(root, p)); err != nil {
			return nil, err
		}
	}
	return &Store{root: root}, nil
}

// PoolRelPath is PoolPath relative to the storage root, with forward slashes.
// It is the path recorded in the metadata database (blobs.relative_path).
func PoolRelPath(sha string) string {
	return ".system/pool/" + sha[:2] + "/" + sha
}

// PoolPath is where the original with the given SHA-256 hex is stored.
func (s *Store) PoolPath(sha string) string {
	return filepath.Join(s.root, filepath.FromSlash(PoolRelPath(sha)))
}

// PreviewPath is where the JPEG preview of the given SHA-256 hex is stored.
func (s *Store) PreviewPath(sha string) string {
	return filepath.Join(s.root, ".system", "previews", sha[:2], sha+".jpg")
}

// StagedFile is a fully written and fsynced upload in .system/incoming.
type StagedFile struct {
	Path     string
	SHA256   string // lowercase hex
	Size     int64
	MimeType string // http.DetectContentType of the first 512 bytes
}

// Remove deletes the temp file. It is a no-op after a successful Promote.
func (f *StagedFile) Remove() { _ = os.Remove(f.Path) }

// sniffWriter keeps the first 512 bytes written to it.
type sniffWriter struct{ buf []byte }

func (w *sniffWriter) Write(p []byte) (int, error) {
	if room := 512 - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Stage copies r into a temp file while hashing it. It reads at most limit+1
// bytes and fails with ErrTooLarge beyond limit. On error nothing is left in
// .system/incoming; on success the caller must Remove or Promote the file.
func (s *Store) Stage(r io.Reader, limit int64) (*StagedFile, error) {
	temp, err := os.CreateTemp(filepath.Join(s.root, ".system", "incoming"), "upload-*")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTempCreate, err)
	}
	staged := &StagedFile{Path: temp.Name()}
	fail := func(err error) (*StagedFile, error) {
		temp.Close()
		staged.Remove()
		return nil, err
	}
	h := sha256.New()
	sniff := &sniffWriter{}
	count, err := io.Copy(io.MultiWriter(temp, h, sniff), io.LimitReader(r, limit+1))
	if err != nil {
		return fail(fmt.Errorf("%w: %w", ErrCopy, err))
	}
	if count > limit {
		return fail(ErrTooLarge)
	}
	if count == 0 {
		return fail(ErrEmpty)
	}
	if err := temp.Sync(); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrSync, err))
	}
	if err := temp.Close(); err != nil {
		staged.Remove()
		return nil, fmt.Errorf("%w: %w", ErrClose, err)
	}
	staged.SHA256 = hex.EncodeToString(h.Sum(nil))
	staged.Size = count
	staged.MimeType = http.DetectContentType(sniff.buf)
	return staged, nil
}

// Promote atomically moves a staged file to PoolPath(sha) and makes it
// read-only. An existing pool file with the same hash is replaced; both have
// identical content. sha must be f.SHA256 (the verified hash).
func (s *Store) Promote(f *StagedFile, sha string) error {
	if sha != f.SHA256 {
		return errors.New("blobstore: promote hash does not match staged file")
	}
	dest := s.PoolPath(sha)
	if err := storage.MkdirIn(filepath.Dir(dest)); err != nil {
		return err
	}
	if err := os.Rename(f.Path, dest); err != nil {
		return err
	}
	_ = os.Chmod(dest, 0400)
	return nil
}
