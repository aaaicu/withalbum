package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aaaicu/withalbum/internal/audit"
	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Restore procedure (the server must be stopped; it takes the instance lock):
//
//  1. Verify the snapshot: the database file matches the manifest checksum
//     and passes integrity_check, its blobs table equals the manifest, and
//     every original is copied and hashed against its name.
//  2. Build the complete new .system in <root>/.system-restore (staging):
//     database, originals (read-only), previews. Then migrate the staged
//     database to this binary's schema, revoke every session and device
//     (sessions revoked after the backup was taken must not come back) and
//     record the restore in the audit log.
//  3. Write the journal <root>/.withalbum-restore.json. From here on every
//     WithAlbum command except restore refuses the volume
//     (storage.ErrRestorePending) until the swap is finished.
//  4. Swap: rename the current .system to .system.before-restore-<UTC> (it
//     is kept, never deleted, unless it held nothing but the lock file), then
//     rename the staging directory to .system, and remove the journal.
//
// Crash recovery: before the journal exists the old .system is untouched
// and a rerun discards the staging directory (only copies). Once the
// journal exists, a rerun of restore finishes the swap from where it
// stopped without copying again.

// Crash points of Restore, in procedure order (tests only).
const (
	PointAfterStaging  = "after_staging"
	PointAfterJournal  = "after_journal"
	PointAfterPreserve = "after_preserve"
	PointAfterSwap     = "after_swap"
	stagingName        = ".system-restore"
	stagingSentinel    = ".withalbum-restore-staging"
	restoreLockName    = ".withalbum-restore.lock"
	preservedPrefix    = ".system.before-restore-"
	journalFormat      = 1
	restoreAuditAction = "storage.restore"
	restoreAuditTarget = "storage"
	restoreTimeFormat  = "20060102T150405Z"
	restoreProgress    = 5 * time.Second
)

var (
	// ErrTargetNotEmpty means the storage root already holds a library and
	// RestoreOptions.Replace was not set.
	ErrTargetNotEmpty = errors.New("storage root already holds data")
	// ErrJournal means the restore journal does not match the directories.
	ErrJournal = errors.New("restore journal does not match the storage root")
)

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// CheckOnly verifies the snapshot completely (every original hashed)
	// and changes nothing. The storage root is not needed.
	CheckOnly bool
	// Replace allows restoring over an existing library, which is kept as
	// .system.before-restore-<UTC>.
	Replace bool
	// ExpectedVolumeID pins the storage volume (WITHALBUM_VOLUME_ID).
	ExpectedVolumeID string
	Progress         io.Writer
	hook             func(point string) error
}

// RestoreReport is the result of Restore.
type RestoreReport struct {
	BackupID        string   `json:"backup_id,omitempty"`
	CreatedAt       string   `json:"backup_created_at,omitempty"`
	Blobs           int      `json:"blobs"`
	BlobBytes       int64    `json:"blob_bytes"`
	Previews        int      `json:"previews"`
	PreviewsSkipped []string `json:"previews_skipped,omitempty"`
	CheckOnly       bool     `json:"check_only"`
	Resumed         bool     `json:"resumed_interrupted_restore"`
	SessionsRevoked int64    `json:"sessions_revoked"`
	PreservedPath   string   `json:"preserved_previous_data,omitempty"`
	Seconds         float64  `json:"seconds"`
}

// journal is the content of storage.RestoreJournalName.
type journal struct {
	Format    int    `json:"format"`
	BackupID  string `json:"backup_id"`
	Staging   string `json:"staging"`
	Preserved string `json:"preserved"`
	StartedAt string `json:"started_at"`
}

// snapshot is an opened snapshot directory.
type snapshot struct {
	dir string
	t   target
	m   *Manifest
}

// openSnapshotDir resolves snapshots/<id> inside a backup target and loads
// its manifest. The backup disk may be read-only.
func openSnapshotDir(dir string) (*snapshot, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if filepath.Base(filepath.Dir(resolved)) != SnapshotsDir {
		return nil, fmt.Errorf("%s is not a snapshot directory (expected <backup target>/%s/<id>)", dir, SnapshotsDir)
	}
	tv, err := storage.OpenBackupTarget(filepath.Dir(filepath.Dir(resolved)), false)
	if err != nil {
		return nil, fmt.Errorf("backup target of %s: %w", dir, err)
	}
	m, err := readManifest(filepath.Join(resolved, ManifestName))
	if err != nil {
		return nil, err
	}
	if m.ID != filepath.Base(resolved) {
		return nil, fmt.Errorf("%w: manifest id %q does not match the directory name", ErrBackupDamaged, m.ID)
	}
	return &snapshot{dir: resolved, t: target{vol: tv, root: tv.Root()}, m: m}, nil
}

// verifyDB checks the snapshot database file against the manifest and
// returns what it contains.
func (s *snapshot) verifyDB(ctx context.Context) (*snapshotInfo, error) {
	path := filepath.Join(s.dir, DBName)
	size, sum, err := hashRegular(path)
	if err != nil {
		return nil, fmt.Errorf("%w: database: %v", ErrBackupDamaged, err)
	}
	if size != s.m.Database.Bytes || sum != s.m.Database.SHA256 {
		return nil, fmt.Errorf("%w: database file does not match its manifest checksum", ErrBackupDamaged)
	}
	info, err := inspectSnapshot(ctx, path)
	if err != nil {
		return nil, err
	}
	if info.schema != s.m.SchemaVersion {
		return nil, fmt.Errorf("%w: schema version %d, manifest says %d", ErrBackupDamaged, info.schema, s.m.SchemaVersion)
	}
	if info.schema > db.LatestVersion() {
		return nil, fmt.Errorf("%w: backup schema %d, this binary supports up to %d; use a newer withalbum-server", db.ErrSchemaTooNew, info.schema, db.LatestVersion())
	}
	if err := sameBlobs(s.m.Blobs, info.blobs); err != nil {
		return nil, err
	}
	return info, nil
}

// Restore verifies the snapshot directory snapshotDir and, unless
// opt.CheckOnly, restores it into the storage root root (see the procedure
// above).
func Restore(ctx context.Context, snapshotDir, root string, opt RestoreOptions) (*RestoreReport, error) {
	start := time.Now()
	hook := opt.hook
	if hook == nil {
		hook = func(string) error { return nil }
	}
	r := &RestoreReport{CheckOnly: opt.CheckOnly}
	if opt.CheckOnly {
		err := checkOnly(ctx, snapshotDir, opt, r)
		r.Seconds = time.Since(start).Seconds()
		return r, err
	}
	vol, err := storage.Open(root, storage.OpenOptions{ExpectedVolumeID: opt.ExpectedVolumeID, AllowPendingRestore: true})
	if err != nil {
		return r, err
	}
	rlock, err := storage.LockFile(filepath.Join(vol.Root(), restoreLockName))
	if err != nil {
		return r, err
	}
	defer rlock.Release()

	// An interrupted restore is finished first, whatever snapshot was asked for.
	jpath := filepath.Join(vol.Root(), storage.RestoreJournalName)
	if ok, err := exists(jpath); err != nil {
		return r, err
	} else if ok {
		j, err := readJournal(jpath)
		if err != nil {
			return r, err
		}
		r.Resumed, r.BackupID = true, j.BackupID
		r.PreservedPath, err = finishSwap(vol, j, hook)
		r.Seconds = time.Since(start).Seconds()
		return r, err
	}

	// The server and every other command hold this lock while they use the
	// volume.
	lock, err := vol.Lock()
	if err != nil {
		return r, err
	}
	defer lock.Release()
	sys := filepath.Join(vol.Root(), ".system")
	if empty, err := onlyLockFile(sys); err != nil {
		return r, err
	} else if !empty && !opt.Replace {
		return r, fmt.Errorf("%w: %s is not empty. Restore into a newly initialized volume, or pass -replace to keep the current data as %s<UTC> and restore over it", ErrTargetNotEmpty, sys, preservedPrefix)
	}

	s, err := openSnapshotDir(snapshotDir)
	if err != nil {
		return r, err
	}
	r.BackupID, r.CreatedAt = s.m.ID, s.m.CreatedAt
	if _, err := s.verifyDB(ctx); err != nil {
		return r, err
	}
	var need int64 = s.m.Database.Bytes + s.m.BlobBytes
	for _, p := range s.m.Previews {
		need += p.Bytes
	}
	if free, err := vol.FreeBytes(); err == nil && uint64(need) >= free {
		return r, fmt.Errorf("%w: the restore needs %d bytes, %d are free", storage.ErrNoSpace, need, free)
	}

	staging := filepath.Join(vol.Root(), stagingName)
	if err := newStaging(vol, staging); err != nil {
		return r, err
	}
	if err := stage(ctx, vol, s, staging, opt.Progress, r); err != nil {
		return r, err
	}
	if r.SessionsRevoked, err = finalizeDB(ctx, filepath.Join(staging, DBName), s.m); err != nil {
		return r, err
	}
	if err := syncDir(staging); err != nil {
		return r, err
	}
	if err := hook(PointAfterStaging); err != nil {
		return r, err
	}

	j := journal{Format: journalFormat, BackupID: s.m.ID, Staging: stagingName,
		Preserved: preservedPrefix + start.UTC().Format(restoreTimeFormat), StartedAt: start.UTC().Format(time.RFC3339)}
	if ok, err := exists(filepath.Join(vol.Root(), j.Preserved)); err != nil || ok {
		return r, errors.Join(fmt.Errorf("%s already exists; try again in a second", j.Preserved), err)
	}
	if err := vol.Check(); err != nil {
		return r, err
	}
	if err := writeJSONAtomic(jpath, j); err != nil {
		return r, err
	}
	if err := hook(PointAfterJournal); err != nil {
		return r, err
	}
	preserved, err := finishSwap(vol, j, hook)
	if err != nil {
		return r, err
	}
	// The previous .system held only our lock file (an empty volume): there
	// is nothing to keep.
	if empty, err := onlyLockFile(preserved); err == nil && empty {
		if err := os.RemoveAll(preserved); err == nil {
			preserved = ""
			_ = syncDir(vol.Root())
		}
	}
	r.PreservedPath = preserved
	r.Seconds = time.Since(start).Seconds()
	return r, nil
}

// checkOnly verifies a snapshot completely without writing anything.
func checkOnly(ctx context.Context, snapshotDir string, opt RestoreOptions, r *RestoreReport) error {
	s, err := openSnapshotDir(snapshotDir)
	if err != nil {
		return err
	}
	r.BackupID, r.CreatedAt = s.m.ID, s.m.CreatedAt
	if _, err := s.verifyDB(ctx); err != nil {
		return err
	}
	var problems []string
	last := time.Now()
	for _, b := range s.m.Blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		size, sum, err := hashRegular(s.t.poolPath(b.SHA256))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("original %s: %v", b.SHA256, err))
		case size != b.Bytes || sum != b.SHA256:
			problems = append(problems, fmt.Sprintf("original %s: backup copy is damaged (%d bytes, SHA-256 %s)", b.SHA256, size, sum))
		default:
			r.Blobs++
			r.BlobBytes += size
		}
		if opt.Progress != nil && time.Since(last) >= restoreProgress {
			last = time.Now()
			fmt.Fprintf(opt.Progress, "restore -check: %d/%d originals verified\n", r.Blobs, len(s.m.Blobs))
		}
	}
	for _, p := range s.m.Previews {
		if size, sum, err := hashRegular(s.t.previewPath(p.SHA256)); err != nil || size != p.Bytes || sum != p.FileSHA256 {
			r.PreviewsSkipped = append(r.PreviewsSkipped, p.SHA256)
			continue
		}
		r.Previews++
	}
	return errorsOf(ErrBackupDamaged, problems)
}

// newStaging creates an empty staging directory, discarding one left by an
// interrupted restore (it holds only copies). A directory of that name that
// restore did not create is refused.
func newStaging(vol *storage.Volume, staging string) error {
	if err := vol.Check(); err != nil {
		return err
	}
	if ok, err := exists(staging); err != nil {
		return err
	} else if ok {
		if ok, err := exists(filepath.Join(staging, stagingSentinel)); err != nil || !ok {
			return errors.Join(fmt.Errorf("%s exists but was not created by restore; move it away first", staging), err)
		}
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
	}
	if err := os.Mkdir(staging, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(staging, stagingSentinel), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	f.Close()
	for _, d := range []string{"incoming", "pool", "previews"} {
		if err := os.Mkdir(filepath.Join(staging, d), 0700); err != nil {
			return err
		}
	}
	return syncDir(vol.Root())
}

// stage copies the database, every original and every intact preview into
// the staging directory, hashing each file against the manifest.
func stage(ctx context.Context, vol *storage.Volume, s *snapshot, staging string, progress io.Writer, r *RestoreReport) error {
	tmpDir := filepath.Join(staging, "incoming")
	copyInto := func(src, dst string, size int64, sum string, readOnly bool) error {
		if err := vol.Check(); err != nil {
			return err
		}
		tmp, n, got, err := copyVerified(src, tmpDir)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrBackupDamaged, err)
		}
		if err != nil {
			return err
		}
		if n != size || got != sum {
			os.Remove(tmp)
			return fmt.Errorf("%w: %s has %d bytes with SHA-256 %s, expected %d bytes with %s", ErrBackupDamaged, src, n, got, size, sum)
		}
		if err := vol.Check(); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := place(tmp, dst, readOnly); err != nil {
			os.Remove(tmp)
			return err
		}
		return nil
	}
	if err := copyInto(filepath.Join(s.dir, DBName), filepath.Join(staging, DBName), s.m.Database.Bytes, s.m.Database.SHA256, false); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Join(staging, DBName), 0600)
	last := time.Now()
	for _, b := range s.m.Blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(staging, "pool", b.SHA256[:2], b.SHA256)
		if err := copyInto(s.t.poolPath(b.SHA256), dst, b.Bytes, b.SHA256, true); err != nil {
			return fmt.Errorf("original %s: %w", b.SHA256, err)
		}
		r.Blobs++
		r.BlobBytes += b.Bytes
		if progress != nil && time.Since(last) >= restoreProgress {
			last = time.Now()
			fmt.Fprintf(progress, "restore: %d/%d originals copied and verified\n", r.Blobs, len(s.m.Blobs))
		}
	}
	for _, p := range s.m.Previews {
		dst := filepath.Join(staging, "previews", p.SHA256[:2], p.SHA256+".jpg")
		if err := copyInto(s.t.previewPath(p.SHA256), dst, p.Bytes, p.FileSHA256, false); err != nil {
			if errors.Is(err, ErrBackupDamaged) || errors.Is(err, fs.ErrNotExist) {
				// Derived data: a damaged preview is not restored.
				r.PreviewsSkipped = append(r.PreviewsSkipped, p.SHA256)
				continue
			}
			return fmt.Errorf("preview %s: %w", p.SHA256, err)
		}
		r.Previews++
	}
	return syncDir(filepath.Join(staging, "pool"))
}

// finalizeDB migrates the staged database, revokes every session, rotates
// the sync epoch (every client sync cursor becomes invalid), drops upload
// sessions and
// records the restore, then checkpoints the WAL so that the single database
// file is complete before it is renamed.
func finalizeDB(ctx context.Context, path string, m *Manifest) (int64, error) {
	sqlDB, err := db.Open(path)
	if err != nil {
		return 0, err
	}
	closed := false
	defer func() {
		if !closed {
			sqlDB.Close()
		}
	}()
	if err := db.Migrate(ctx, sqlDB); err != nil {
		return 0, err
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	n, err := session.RevokeAllTx(ctx, tx, session.ReasonRestore)
	if err == nil {
		// Sync cursors handed out before the restore point to events that
		// no longer exist (or will be numbered again): a new epoch makes
		// every client cursor invalid (409), so clients resync from a
		// snapshot instead of skipping events.
		_, err = tx.ExecContext(ctx, "UPDATE sync_state SET epoch = lower(hex(randomblob(16))) WHERE singleton = 1")
	}
	if err == nil {
		// Upload sessions belong to devices that were just revoked, and their
		// partial files are not in the backup: drop them (clients start a new
		// session under the same Idempotency-Key).
		_, err = tx.ExecContext(ctx, "DELETE FROM upload_sessions")
	}
	if err == nil {
		err = audit.Record(ctx, tx, audit.Entry{Action: restoreAuditAction, TargetType: restoreAuditTarget, Result: audit.Allowed,
			Details: map[string]any{"backup_id": m.ID, "backup_created_at": m.CreatedAt, "sessions_revoked": n}})
	}
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return 0, err
	}
	closed = true
	if err := sqlDB.Close(); err != nil {
		return 0, err
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return 0, fmt.Errorf("restored database still has %d bytes of WAL after checkpoint", fi.Size())
	}
	return n, syncFile(path)
}

func readJournal(path string) (journal, error) {
	var j journal
	data, err := os.ReadFile(path)
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, fmt.Errorf("%w: %v", ErrJournal, err)
	}
	if j.Format != journalFormat || j.Staging != stagingName || filepath.Base(j.Preserved) != j.Preserved ||
		len(j.Preserved) <= len(preservedPrefix) || j.Preserved[:len(preservedPrefix)] != preservedPrefix {
		return j, fmt.Errorf("%w: unsupported content", ErrJournal)
	}
	return j, nil
}

// finishSwap performs (or completes) step 4 and returns the path of the
// preserved previous .system ("" if there was none).
func finishSwap(vol *storage.Volume, j journal, hook func(string) error) (string, error) {
	root := vol.Root()
	sys := filepath.Join(root, ".system")
	staging := filepath.Join(root, j.Staging)
	preserved := filepath.Join(root, j.Preserved)
	hasStaging, err := exists(staging)
	if err != nil {
		return "", err
	}
	hasSys, err := exists(sys)
	if err != nil {
		return "", err
	}
	hasPreserved, err := exists(preserved)
	if err != nil {
		return "", err
	}
	if err := vol.Check(); err != nil {
		return "", err
	}
	if hasStaging {
		if hasSys {
			if hasPreserved {
				return "", fmt.Errorf("%w: %s, %s and %s all exist; check them by hand", ErrJournal, sys, staging, preserved)
			}
			if err := os.Rename(sys, preserved); err != nil {
				return "", err
			}
			hasPreserved = true
			if err := syncDir(root); err != nil {
				return "", err
			}
			if err := hook(PointAfterPreserve); err != nil {
				return "", err
			}
		}
		if err := os.Rename(staging, sys); err != nil {
			return "", err
		}
		if err := syncDir(root); err != nil {
			return "", err
		}
		if err := hook(PointAfterSwap); err != nil {
			return "", err
		}
	} else if !hasSys {
		return "", fmt.Errorf("%w: neither %s nor %s exists", ErrJournal, staging, sys)
	}
	if err := os.Remove(filepath.Join(sys, stagingSentinel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Remove(filepath.Join(root, storage.RestoreJournalName)); err != nil {
		return "", err
	}
	if err := syncDir(root); err != nil {
		return "", err
	}
	if !hasPreserved {
		return "", nil
	}
	return preserved, nil
}
