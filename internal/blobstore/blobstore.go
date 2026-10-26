// Package blobstore stores original files content-addressed by SHA-256 under
// .system/pool and their JPEG previews under .system/previews.
//
// Uploads are first written to .system/incoming (Stage), then moved into the
// pool with an atomic rename (Promote). Callers must verify the storage
// volume (storage.Volume.Check) before calling Stage or Promote.
//
// Commit order of an upload (see internal/httpapi): Stage fsyncs the temp
// file, Promote renames it into the pool and fsyncs the pool directory, and
// only then is the metadata transaction committed. A crash at any point
// leaves at most an unreferenced temp or pool file, never a metadata row
// without its file. Reconcile cleans up after such a crash at startup.
//
// On macOS, os.File.Sync issues F_FULLFSYNC (the Go runtime does this), so
// file and directory syncs reach the disk, not just its cache.
package blobstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aaaicu/withalbum/internal/storage"
)

// Stage errors. Errors other than ErrTooLarge and ErrEmpty wrap the
// underlying error.
var (
	ErrTempCreate = errors.New("cannot create temp file")
	ErrCopy       = errors.New("cannot read upload") // reading the request body failed
	ErrWrite      = errors.New("cannot write temp file")
	ErrTooLarge   = errors.New("upload exceeds limit")
	ErrEmpty      = errors.New("upload is empty")
	ErrSync       = errors.New("cannot sync temp file")
	ErrClose      = errors.New("cannot close temp file")
)

// uploadTempPrefix is the name prefix of Stage temp files in .system/incoming.
const uploadTempPrefix = "upload-"

// Store is safe for concurrent use.
type Store struct {
	vol        *storage.Volume
	root       string
	converters []Converter // set once with SetConverters, before use
	transcoder Transcoder  // set once with SetTranscoder, before use; nil: none
	// promoteMu serializes Promote per pool prefix directory (the first two
	// hex digits of the hash). The checks Promote makes on an existing pool
	// file and the rename that replaces it are not atomic together; without
	// this, two concurrent Promotes of the same bytes could take each
	// other's fresh file for a damaged one. One process owns the volume
	// (instance lock), so an in-process lock is enough.
	promoteMu [256]sync.Mutex
}

// New creates .system/incoming, .system/pool, .system/previews,
// .system/uploads (resumable upload sessions), .system/work (preview
// converter scratch space) and .system/playback (playback copies of
// videos) inside a verified volume. It never creates the
// volume root or any missing parent.
func New(vol *storage.Volume) (*Store, error) {
	if err := vol.Check(); err != nil {
		return nil, err
	}
	root := vol.Root()
	for _, p := range []string{".system", ".system/incoming", ".system/pool", ".system/previews", ".system/uploads", ".system/work", PlaybackRelDir} {
		if err := storage.MkdirIn(filepath.Join(root, p)); err != nil {
			return nil, err
		}
	}
	// All 256 prefix directories exist from the start, so storing new bytes
	// never creates (and syncs) a directory that known bytes would not:
	// the work, and the time an upload takes, does not depend on whether
	// the bytes were already stored.
	for _, top := range []string{".system/pool", ".system/previews"} {
		created := false
		for i := 0; i < 256; i++ {
			dir := filepath.Join(root, filepath.FromSlash(top), fmt.Sprintf("%02x", i))
			if _, err := os.Lstat(dir); err == nil {
				continue
			}
			if err := storage.MkdirIn(dir); err != nil {
				return nil, err
			}
			created = true
		}
		if created {
			if err := storage.SyncDir(filepath.Join(root, filepath.FromSlash(top))); err != nil {
				return nil, err
			}
		}
	}
	return &Store{vol: vol, root: root}, nil
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

// PreviewRelPath is PreviewPath relative to the storage root, with forward
// slashes.
func PreviewRelPath(sha string) string {
	return ".system/previews/" + sha[:2] + "/" + sha + ".jpg"
}

// PreviewPath is where the JPEG preview of the given SHA-256 hex is stored.
func (s *Store) PreviewPath(sha string) string {
	return filepath.Join(s.root, filepath.FromSlash(PreviewRelPath(sha)))
}

// StagedFile is a fully written and fsynced upload in .system/incoming.
type StagedFile struct {
	Path     string
	SHA256   string // lowercase hex
	Size     int64
	MimeType string // SniffMIME of the first 512 bytes
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

// errWriter remembers the first error of the underlying writer, so that
// Stage can tell a failed disk write from a failed read of the request.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

// Stage copies r into a temp file while hashing it. It reads at most limit+1
// bytes and fails with ErrTooLarge beyond limit. On error nothing is left in
// .system/incoming; on success the caller must Remove or Promote the file.
func (s *Store) Stage(r io.Reader, limit int64) (*StagedFile, error) {
	temp, err := os.CreateTemp(filepath.Join(s.root, ".system", "incoming"), uploadTempPrefix+"*")
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
	file := &errWriter{w: temp}
	count, err := io.Copy(io.MultiWriter(file, h, sniff), io.LimitReader(r, limit+1))
	if file.err != nil {
		return fail(fmt.Errorf("%w: %w", ErrWrite, file.err))
	}
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
	staged.MimeType = SniffMIME(sniff.buf)
	return staged, nil
}

// Promote durably moves a staged file to PoolPath(sha): the file is made
// read-only, renamed into place and the pool directory is fsynced (also the
// pool itself when the prefix directory is new). sha must be f.SHA256 (the
// verified hash).
//
// Promotes into the same pool prefix directory run one at a time, so a
// pool file another upload of the same bytes has just put in place is
// never mistaken for a damaged one.
//
// If a pool file for sha already exists (a concurrent upload of the same
// bytes, or a file left by a crash before the metadata commit) it is re-hashed:
// when it matches it is reused and the staged file is left for the caller
// to Remove; when it does not match, the damaged file is moved to
// .system/orphans (never deleted) and replaced by the staged file.
func (s *Store) Promote(f *StagedFile, sha string) error {
	if sha != f.SHA256 {
		return errors.New("blobstore: promote hash does not match staged file")
	}
	if len(sha) != 64 {
		return errors.New("blobstore: promote needs a SHA-256 hex digest")
	}
	prefix, err := strconv.ParseUint(sha[:2], 16, 8)
	if err != nil {
		return errors.New("blobstore: promote needs a SHA-256 hex digest")
	}
	s.promoteMu[prefix].Lock()
	defer s.promoteMu[prefix].Unlock()
	dest := s.PoolPath(sha)
	dir := filepath.Dir(dest)
	_, statErr := os.Lstat(dir)
	if err := storage.MkdirIn(dir); err != nil {
		return err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := storage.SyncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	switch ok, err := fileMatches(dest, sha, f.Size); {
	case err != nil:
		return err
	case ok:
		// Reuse. The existing file was fsynced by whoever wrote it; sync its
		// directory entry in case that writer crashed before doing so.
		// Before that, change the directory the way storing new bytes does
		// (a name added, a name removed), so that the sync costs the same
		// and the time of an upload does not tell whether the bytes were
		// already stored (an UPLOADER must not learn that).
		equalizeDirWork(dir, dest, sha)
		return storage.SyncDir(dir)
	default:
		if _, err := os.Lstat(dest); err == nil {
			if _, err := s.quarantine(dest, sha, "mismatch"); err != nil {
				return err
			}
		}
	}
	// Read-only before it becomes visible. exFAT and similar file systems
	// ignore modes; that is not an error.
	_ = os.Chmod(f.Path, 0400)
	if err := os.Rename(f.Path, dest); err != nil {
		return err
	}
	return storage.SyncDir(dir)
}

// LinkPrefix names the short-lived extra hard links equalizeDirWork makes
// in a pool prefix directory: ".link-<sha>-<random>". They are second names
// of a pool file, never data of their own.
const LinkPrefix = ".link-"

// equalizeDirWork adds and removes a hard link to the pool file dest in its
// directory. Nothing else sees the link; a crash can leave it behind, and
// Reconcile removes it (it is only a second name of a pool file). File
// systems without hard links (exFAT) skip it.
func equalizeDirWork(dir, dest, sha string) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return
	}
	link := filepath.Join(dir, LinkPrefix+sha+"-"+hex.EncodeToString(b[:]))
	if err := os.Link(dest, link); err == nil {
		_ = os.Remove(link)
	}
}

// fileMatches reports whether path is a regular file of the given size whose
// SHA-256 is sha. A missing file is (false, nil).
func fileMatches(path, sha string, size int64) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() || fi.Size() != size {
		return false, nil
	}
	got, err := hashFile(path)
	if err != nil {
		return false, err
	}
	return got == sha, nil
}

// hashFile returns the lowercase hex SHA-256 of the file at path.
func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// OrphansRelDir holds pool files that no metadata references and damaged
// pool files. Nothing in it is ever deleted automatically.
const OrphansRelDir = ".system/orphans"

// quarantine moves path into .system/orphans as "<sha>.<reason>.<UTC time>"
// and fsyncs both directories. It returns the new path.
func (s *Store) quarantine(path, sha, reason string) (string, error) {
	dir := filepath.Join(s.root, filepath.FromSlash(OrphansRelDir))
	if err := storage.MkdirIn(dir); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, sha+"."+reason+"."+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("blobstore: quarantine target %s exists", dst)
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	if err := storage.SyncDir(dir); err != nil {
		return "", err
	}
	return dst, storage.SyncDir(filepath.Dir(path))
}
