package blobstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/aaaicu/withalbum/internal/storage"
)

// Originals of a purged album (meta.AlbumManagement.PurgeDeletedFolder).
//
// The purge transaction deletes the rows of the originals no asset holds
// any more and, right before its COMMIT, calls DetachOriginals, which
// renames their pool files into .system/purge (same volume, so a rename).
// After the COMMIT the caller deletes them with RemoveDetached. A file in
// .system/purge is therefore always in one of two states, which
// RecoverPurged tells apart at the next start by asking the database:
//
//   - its row still exists (the process died between the rename and the
//     COMMIT): it is put back into the pool;
//   - its row is gone (the process died after the COMMIT): it is deleted.
//
// Nothing else ever deletes an original.

// PurgeRelDir holds pool files whose rows a purge has deleted or is
// deleting.
const PurgeRelDir = ".system/purge"

func (s *Store) purgePath(sha string) string {
	return filepath.Join(s.root, filepath.FromSlash(PurgeRelDir), sha)
}

func prefixOf(sha string) (int, error) {
	if len(sha) != 64 || !isHex(sha) {
		return 0, errors.New("blobstore: not a SHA-256 hex digest")
	}
	p, err := strconv.ParseUint(sha[:2], 16, 8)
	return int(p), err
}

// moveFile renames from to to and syncs both directories.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if err := storage.SyncDir(filepath.Dir(to)); err != nil {
		return err
	}
	return storage.SyncDir(filepath.Dir(from))
}

// DetachOriginals moves the pool files of shas into .system/purge, each
// under the promote lock of its pool prefix (so it never races a Promote
// of the same bytes). A missing pool file is skipped. If a move fails the
// files already moved are put back and the error is returned. undo puts
// every moved file back (the caller's COMMIT failed).
func (s *Store) DetachOriginals(shas []string) (undo func(), err error) {
	if err := s.vol.Check(); err != nil {
		return nil, err
	}
	if err := storage.MkdirIn(filepath.Join(s.root, filepath.FromSlash(PurgeRelDir))); err != nil {
		return nil, err
	}
	var moved []string
	undo = func() {
		for _, sha := range moved {
			if err := s.reattach(sha); err != nil {
				// RecoverPurged puts it back at the next start.
				log.Printf("purge: cannot put original %s back into the pool (restored at the next start): %v", sha, err)
			}
		}
	}
	for _, sha := range shas {
		p, err := prefixOf(sha)
		if err != nil {
			undo()
			return nil, err
		}
		from, to := s.PoolPath(sha), s.purgePath(sha)
		s.promoteMu[p].Lock()
		err = os.Rename(from, to)
		s.promoteMu[p].Unlock()
		switch {
		case err == nil:
			moved = append(moved, sha)
		case errors.Is(err, os.ErrNotExist):
			// Already missing (reported by the startup check); nothing to move.
		default:
			undo()
			return nil, fmt.Errorf("detach %s: %w", sha, err)
		}
	}
	// Make the renames durable before the caller commits: after a crash the
	// database and the directories must agree.
	dirs := map[string]bool{filepath.Join(s.root, filepath.FromSlash(PurgeRelDir)): true}
	for _, sha := range moved {
		dirs[filepath.Dir(s.PoolPath(sha))] = true
	}
	for dir := range dirs {
		if err := storage.SyncDir(dir); err != nil {
			undo()
			return nil, fmt.Errorf("detach: sync %s: %w", filepath.Base(dir), err)
		}
	}
	return undo, nil
}

// reattach moves a detached file back into the pool unless the pool holds
// a file of that name again, in which case the detached one is quarantined
// (never deleted).
func (s *Store) reattach(sha string) error {
	p, err := prefixOf(sha)
	if err != nil {
		return err
	}
	s.promoteMu[p].Lock()
	defer s.promoteMu[p].Unlock()
	from, dest := s.purgePath(sha), s.PoolPath(sha)
	if _, err := os.Lstat(dest); err == nil {
		_, err := s.quarantine(from, sha, "purge-duplicate")
		return err
	}
	return moveFile(from, dest)
}

// RemoveDetached deletes the detached original of a purged blob and its
// derived files (preview, playback copy). Missing files are fine.
func (s *Store) RemoveDetached(sha string) error {
	if _, err := prefixOf(sha); err != nil {
		return err
	}
	if err := s.vol.Check(); err != nil {
		return err
	}
	var errs []error
	for _, path := range []string{s.purgePath(sha), s.PreviewPath(sha), s.PlaybackPath(sha)} {
		if err := os.Remove(path); err == nil {
			errs = append(errs, storage.SyncDir(filepath.Dir(path)))
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// PurgeRecovery is what RecoverPurged did.
type PurgeRecovery struct {
	Restored   int // put back into the pool (the purge did not commit)
	Removed    int // deleted (the purge committed)
	Unexpected int // entries WithAlbum did not create, left alone
}

// RecoverPurged settles the files left in .system/purge by a purge that
// was interrupted (see the package comment above). has reports whether the
// database still records a blob. It must run at startup, under the
// instance lock and before Reconcile (which would otherwise report the
// detached originals of an uncommitted purge as missing).
func (s *Store) RecoverPurged(ctx context.Context, has func(sha string) (bool, error)) (PurgeRecovery, error) {
	var r PurgeRecovery
	if err := s.vol.Check(); err != nil {
		return r, err
	}
	dir := filepath.Join(s.root, filepath.FromSlash(PurgeRelDir))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		sha := e.Name()
		if _, err := prefixOf(sha); err != nil || !e.Type().IsRegular() {
			r.Unexpected++
			continue
		}
		kept, err := has(sha)
		if err != nil {
			return r, err
		}
		if kept {
			if err := s.reattach(sha); err != nil {
				return r, fmt.Errorf("put back %s: %w", sha, err)
			}
			r.Restored++
			continue
		}
		if err := s.RemoveDetached(sha); err != nil {
			return r, fmt.Errorf("remove purged %s: %w", sha, err)
		}
		r.Removed++
	}
	return r, nil
}
