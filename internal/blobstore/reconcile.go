package blobstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aaaicu/withalbum/internal/storage"
)

// BlobIndex lists the blobs the metadata database knows about.
type BlobIndex interface {
	// ForEachBlob calls fn for every recorded blob in ascending sha256
	// order and stops at the first error.
	ForEachBlob(ctx context.Context, fn func(sha string, size int64) error) error
}

// maxSamples bounds the example lists kept in a ReconcileReport.
const maxSamples = 20

// ReconcileReport summarizes what Reconcile found and did. Count fields are
// exact; the Sample lists hold at most 20 entries each.
type ReconcileReport struct {
	// Blobs is the number of blobs in the metadata database.
	Blobs int
	// IncomingRemoved counts unfinished upload temp files that were deleted
	// from .system/incoming. They were never acknowledged to a client.
	IncomingRemoved int
	// PreviewTempsRemoved counts unfinished preview temp files deleted from
	// .system/previews (derived data).
	PreviewTempsRemoved int
	// Orphans counts pool files without a metadata row, moved to
	// .system/orphans (never deleted).
	Orphans      int
	OrphanSample []string // sha256
	// Missing counts blobs recorded in the database whose pool file is
	// missing: an original was lost. This is an error.
	Missing       int
	MissingSample []string // sha256
	// SizeMismatch counts pool files whose size differs from the database:
	// a damaged original. Left untouched. This is an error.
	SizeMismatch       int
	SizeMismatchSample []string // sha256
	// Unexpected counts entries in the pool or in incoming that WithAlbum
	// did not create (wrong names, directories, symlinks). Left untouched.
	Unexpected       int
	UnexpectedSample []string // path relative to the storage root
}

// Healthy reports whether no original is missing or damaged and nothing
// unexpected was found. Removed temp files and quarantined orphans are the
// normal result of a crash and do not make a report unhealthy.
func (r *ReconcileReport) Healthy() bool {
	return r.Missing == 0 && r.SizeMismatch == 0 && r.Unexpected == 0
}

// Changed reports whether Reconcile removed or moved anything.
func (r *ReconcileReport) Changed() bool {
	return r.IncomingRemoved > 0 || r.PreviewTempsRemoved > 0 || r.Orphans > 0
}

// Summary is a one-line, path-free description for logs.
func (r *ReconcileReport) Summary() string {
	return fmt.Sprintf("blobs=%d incoming_removed=%d preview_temps_removed=%d orphans_quarantined=%d missing=%d size_mismatch=%d unexpected=%d",
		r.Blobs, r.IncomingRemoved, r.PreviewTempsRemoved, r.Orphans, r.Missing, r.SizeMismatch, r.Unexpected)
}

func sample(list *[]string, v string) {
	if len(*list) < maxSamples {
		*list = append(*list, v)
	}
}

// Reconcile brings the files in .system in line with the metadata after an
// unclean shutdown. It must run at startup, while the caller holds the
// instance lock (storage.Volume.Lock) and before any upload is accepted:
//
//   - deletes upload temp files in .system/incoming (never acknowledged),
//   - deletes preview temp files in .system/previews and preview
//     converter scratch directories in .system/work,
//   - moves pool files that index does not list to .system/orphans,
//   - reports blobs listed in index whose pool file is missing or has the
//     wrong size (originals are never re-hashed here; that is too slow at
//     startup for large libraries).
//
// Pool files and blobs are compared with one sorted merge, so memory use
// does not grow with the number of originals.
func (s *Store) Reconcile(ctx context.Context, index BlobIndex) (*ReconcileReport, error) {
	if err := s.vol.Check(); err != nil {
		return nil, err
	}
	r := &ReconcileReport{}
	if err := s.cleanIncoming(r); err != nil {
		return r, fmt.Errorf("reconcile incoming: %w", err)
	}
	if err := s.cleanPreviewTemps(r); err != nil {
		return r, fmt.Errorf("reconcile previews: %w", err)
	}
	if err := s.cleanWork(); err != nil {
		return r, fmt.Errorf("reconcile preview work: %w", err)
	}
	it := &poolIter{s: s, r: r}
	cur, ok, err := it.next()
	if err != nil {
		return r, err
	}
	orphan := func(e poolEntry) error {
		if err := s.vol.Check(); err != nil {
			return err
		}
		if _, err := s.quarantine(e.path, e.sha, "orphan"); err != nil {
			return fmt.Errorf("quarantine orphan %s: %w", e.sha, err)
		}
		r.Orphans++
		sample(&r.OrphanSample, e.sha)
		return nil
	}
	err = index.ForEachBlob(ctx, func(sha string, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.Blobs++
		for ok && cur.sha < sha {
			if err := orphan(cur); err != nil {
				return err
			}
			if cur, ok, err = it.next(); err != nil {
				return err
			}
		}
		if ok && cur.sha == sha {
			if cur.size != size {
				r.SizeMismatch++
				sample(&r.SizeMismatchSample, sha)
			}
			cur, ok, err = it.next()
			return err
		}
		r.Missing++
		sample(&r.MissingSample, sha)
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("reconcile pool: %w", err)
	}
	for ok {
		if err := orphan(cur); err != nil {
			return r, fmt.Errorf("reconcile pool: %w", err)
		}
		if cur, ok, err = it.next(); err != nil {
			return r, fmt.Errorf("reconcile pool: %w", err)
		}
	}
	return r, nil
}

func (s *Store) rel(path string) string {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func (s *Store) unexpected(r *ReconcileReport, path string) {
	r.Unexpected++
	sample(&r.UnexpectedSample, s.rel(path))
}

// cleanIncoming deletes regular "upload-*" files in .system/incoming.
func (s *Store) cleanIncoming(r *ReconcileReport) error {
	dir := filepath.Join(s.root, ".system", "incoming")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed := false
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), uploadTempPrefix) {
			s.unexpected(r, path)
			continue
		}
		if err := s.vol.Check(); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		r.IncomingRemoved++
		removed = true
	}
	if removed {
		return storage.SyncDir(dir)
	}
	return nil
}

// cleanPreviewTemps deletes regular "preview-*" files one level below
// .system/previews. Finished previews and anything else are left alone.
func (s *Store) cleanPreviewTemps(r *ReconcileReport) error {
	root := filepath.Join(s.root, ".system", "previews")
	prefixes, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, p := range prefixes {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), previewTempPrefix) {
				continue
			}
			if err := s.vol.Check(); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
			r.PreviewTempsRemoved++
		}
	}
	return nil
}

// removeLink deletes a leftover ".link-<sha>-<random>" entry of
// equalizeDirWork if it is a second name of the pool file <sha> in the same
// directory; anything else with that prefix is unexpected and left alone.
func (s *Store) removeLink(r *ReconcileReport, dir, name string) error {
	path := filepath.Join(dir, name)
	sha, _, ok := strings.Cut(strings.TrimPrefix(name, LinkPrefix), "-")
	if !ok || len(sha) != 64 || !isHex(sha) || sha[:2] != filepath.Base(dir) {
		s.unexpected(r, path)
		return nil
	}
	li, err := os.Lstat(path)
	if err != nil {
		return err
	}
	oi, err := os.Lstat(filepath.Join(dir, sha))
	if err != nil || !li.Mode().IsRegular() || !os.SameFile(li, oi) {
		s.unexpected(r, path)
		return nil
	}
	if err := s.vol.Check(); err != nil {
		return err
	}
	return os.Remove(path)
}

// isHex reports whether s is non-empty lowercase hex.
func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

type poolEntry struct {
	sha  string
	path string
	size int64
}

// poolIter yields well-formed pool files (.system/pool/<sha[:2]>/<sha>) in
// ascending sha order, reading one prefix directory at a time. Other
// entries are counted as unexpected and skipped.
type poolIter struct {
	s        *Store
	r        *ReconcileReport
	prefixes []fs.DirEntry
	loaded   bool
	pi       int
	files    []fs.DirEntry
	dir      string
	fi       int
}

func (it *poolIter) next() (poolEntry, bool, error) {
	pool := filepath.Join(it.s.root, ".system", "pool")
	if !it.loaded {
		var err error
		if it.prefixes, err = os.ReadDir(pool); err != nil {
			return poolEntry{}, false, err
		}
		it.loaded = true
	}
	for {
		for it.fi < len(it.files) {
			e := it.files[it.fi]
			it.fi++
			path := filepath.Join(it.dir, e.Name())
			name := e.Name()
			if strings.HasPrefix(name, LinkPrefix) {
				if err := it.s.removeLink(it.r, it.dir, name); err != nil {
					return poolEntry{}, false, err
				}
				continue
			}
			if !e.Type().IsRegular() || len(name) != 64 || !isHex(name) || name[:2] != filepath.Base(it.dir) {
				it.s.unexpected(it.r, path)
				continue
			}
			info, err := e.Info()
			if err != nil {
				return poolEntry{}, false, err
			}
			return poolEntry{sha: name, path: path, size: info.Size()}, true, nil
		}
		if it.pi >= len(it.prefixes) {
			return poolEntry{}, false, nil
		}
		p := it.prefixes[it.pi]
		it.pi++
		dir := filepath.Join(pool, p.Name())
		if !p.IsDir() || len(p.Name()) != 2 || !isHex(p.Name()) {
			it.s.unexpected(it.r, dir)
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return poolEntry{}, false, err
		}
		it.dir, it.files, it.fi = dir, files, 0
	}
}
