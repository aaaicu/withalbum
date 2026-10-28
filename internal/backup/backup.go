// Package backup copies a WithAlbum storage volume to a backup target and
// restores it ("withalbum-server backup" and "withalbum-server restore").
//
// Backup can run while the server is running. The database is copied with
// SQLite's VACUUM INTO, which writes a consistent snapshot as of one read
// transaction (WAL content included) without blocking uploads. Originals
// are then copied for exactly the blobs that snapshot lists. Because an
// upload writes and fsyncs its pool file before its database row commits
// (internal/blobstore), every original the snapshot refers to already
// exists, and pool files never change once written, so the copy matches the
// snapshot even while uploads go on. Each original is hashed while it is
// copied and must match its name; a damaged source original fails the
// backup instead of replacing a good copy.
//
// Originals and previews are shared by all snapshots of a target and named
// by their hash, so later backups only copy new files (incremental).
//
// Backups contain password hashes, session token hashes and every original:
// treat the target like the library itself. Files are created 0600/0400 and
// directories 0700.
package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/storage"
)

var (
	// ErrSourceDamaged means an original in the source pool does not match
	// its hash (or is missing) and the target holds no good copy of it.
	ErrSourceDamaged = errors.New("source original damaged or missing")
	// ErrBackupDamaged means a snapshot failed verification.
	ErrBackupDamaged = errors.New("backup snapshot is damaged or incomplete")
	// ErrTargetInsideSource means the target and the storage root overlap.
	ErrTargetInsideSource = errors.New("backup target and storage root overlap")
)

// Crash points of Backup, in procedure order (tests only).
const (
	PointAfterDBSnapshot = "after_db_snapshot"
	PointAfterBlobCopy   = "after_blob_copy"
	PointBeforeFinalize  = "before_finalize"
)

// Options configures Backup.
type Options struct {
	// Rehash re-hashes originals already in the target instead of trusting
	// their size; damaged copies are moved to damaged/ and copied again.
	Rehash bool
	// ServerVersion is recorded in the manifest.
	ServerVersion string
	// Progress, if set, receives a line every few seconds.
	Progress io.Writer
	// Keep, when positive, removes the oldest finished snapshots after the
	// new one is complete, so that Keep snapshots remain (the new one
	// included). A failed backup never removes anything. Originals in the
	// target pool are shared by all snapshots and are not removed.
	Keep int
	// hook is called at the Point* names; an error aborts there (tests).
	hook func(point string) error
}

// Report is the result of Backup.
type Report struct {
	ID            string   `json:"id"`
	Path          string   `json:"path"` // snapshot directory
	Blobs         int      `json:"blobs"`
	BlobBytes     int64    `json:"blob_bytes"`
	Copied        int      `json:"copied"`
	CopiedBytes   int64    `json:"copied_bytes"`
	Reused        int      `json:"reused"`
	Replaced      int      `json:"replaced_damaged_copies"`
	Previews      int      `json:"previews"`
	PreviewsLost  []string `json:"previews_missing_in_source,omitempty"`
	SourceDamaged []string `json:"source_damaged,omitempty"`
	// SourceWarnings lists source originals that are missing or have the
	// wrong size although the target already holds a good copy: the
	// snapshot is complete, but the library needs repair (see verify).
	SourceWarnings []string `json:"source_warnings,omitempty"`
	DBBytes        int64    `json:"db_bytes"`
	SameDevice     bool     `json:"same_device_as_storage"`
	Seconds        float64  `json:"seconds"`
	// Pruned lists the snapshots removed for Options.Keep (oldest first).
	Pruned []string `json:"pruned,omitempty"`
	// PruneError is set when the new snapshot is complete but removing
	// old snapshots failed; the snapshots that were not removed are intact.
	PruneError string `json:"prune_error,omitempty"`
}

// target is an opened backup target.
type target struct {
	vol  *storage.Volume
	root string
}

func (t target) poolPath(sha string) string {
	return filepath.Join(t.root, "pool", sha[:2], sha)
}

func (t target) previewPath(sha string) string {
	return filepath.Join(t.root, "previews", sha[:2], sha+".jpg")
}

func (t target) dir(name string) string { return filepath.Join(t.root, name) }

func newSnapshotID(now time.Time) (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}

// overlap reports whether one of the resolved directories contains the other.
func overlap(a, b string) bool {
	within := func(p, dir string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(a, b) || within(b, a)
}

// Backup writes a new snapshot of the volume src (whose database is srcDB,
// opened by the caller) into the backup target directory targetDir, which
// must have been registered with storage.InitBackupTarget. It does not take
// the volume's instance lock, so the server may keep running; it takes the
// target's lock, so two backups never write the same target at once.
func Backup(ctx context.Context, src *storage.Volume, srcDB *sql.DB, targetDir string, opt Options) (*Report, error) {
	start := time.Now()
	hook := opt.hook
	if hook == nil {
		hook = func(string) error { return nil }
	}
	r := &Report{}
	if err := src.Check(); err != nil {
		return r, err
	}
	tv, err := storage.OpenBackupTarget(targetDir, true)
	if err != nil {
		return r, fmt.Errorf("backup target %s: %w", targetDir, err)
	}
	t := target{vol: tv, root: tv.Root()}
	if overlap(t.root, src.Root()) {
		return r, ErrTargetInsideSource
	}
	if same, known, err := storage.SameDevice(t.root, src.Root()); err == nil && known && same {
		r.SameDevice = true
	}
	lock, err := tv.Lock()
	if err != nil {
		return r, err
	}
	defer lock.Release()

	if err := prepareTarget(t); err != nil {
		return r, err
	}
	if r.ID, err = newSnapshotID(start); err != nil {
		return r, err
	}
	partial := filepath.Join(t.dir(SnapshotsDir), partialPrefix+r.ID)
	if err := os.Mkdir(partial, 0700); err != nil {
		return r, err
	}

	// 1. Database snapshot. VACUUM INTO needs a missing or empty file;
	// creating it first gives it mode 0600.
	dbPath := filepath.Join(partial, DBName)
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return r, err
	}
	f.Close()
	if err := tv.Check(); err != nil {
		return r, err
	}
	if _, err := srcDB.ExecContext(ctx, "VACUUM INTO ?", dbPath); err != nil {
		return r, fmt.Errorf("database snapshot: %w", err)
	}
	if err := syncFile(dbPath); err != nil {
		return r, err
	}
	info, err := inspectSnapshot(ctx, dbPath)
	if err != nil {
		return r, err
	}
	m := &Manifest{Format: formatVersion, Kind: manifestKind, ID: r.ID, CreatedAt: start.UTC().Format(time.RFC3339),
		ServerVersion: opt.ServerVersion, SourceVolumeID: src.ID(), SchemaVersion: info.schema,
		Blobs: info.blobs, Previews: []PreviewEntry{}}
	if m.Blobs == nil {
		m.Blobs = []BlobEntry{}
	}
	if m.Database.Bytes, m.Database.SHA256, err = hashRegular(dbPath); err != nil {
		return r, err
	}
	m.Database.Name = DBName
	r.DBBytes = m.Database.Bytes
	if err := hook(PointAfterDBSnapshot); err != nil {
		return r, err
	}

	// 2. Space for the originals not yet in the target.
	var need int64
	for _, b := range m.Blobs {
		m.BlobBytes += b.Bytes
		if fi, err := os.Lstat(t.poolPath(b.SHA256)); err != nil || fi.Size() != b.Bytes {
			need += b.Bytes
		}
	}
	r.Blobs, r.BlobBytes = len(m.Blobs), m.BlobBytes
	if free, err := tv.FreeBytes(); err == nil && uint64(need) >= free {
		return r, fmt.Errorf("%w: the backup target needs %d more bytes, %d are free", storage.ErrNoSpace, need, free)
	}

	// 3. Originals.
	last := time.Now()
	for _, b := range m.Blobs {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if err := copyBlob(src, t, b, opt.Rehash, r); err != nil {
			return r, err
		}
		if r.Copied == 1 {
			if err := hook(PointAfterBlobCopy); err != nil {
				return r, err
			}
		}
		if opt.Progress != nil && time.Since(last) >= 5*time.Second {
			last = time.Now()
			fmt.Fprintf(opt.Progress, "backup: %d/%d originals (%d copied, %d bytes)\n", r.Copied+r.Reused, r.Blobs, r.Copied, r.CopiedBytes)
		}
	}
	if len(r.SourceDamaged) > 0 {
		_ = os.RemoveAll(partial)
		return r, errorsOf(fmt.Errorf("%w: %d original(s) cannot be backed up; nothing was finalized (run withalbum-server verify -hash)", ErrSourceDamaged, len(r.SourceDamaged)), r.SourceDamaged)
	}

	// 4. Previews (derived data: a missing source preview is only reported).
	for _, sha := range info.previewReady {
		p, err := copyPreview(src, t, sha)
		if errors.Is(err, fs.ErrNotExist) {
			r.PreviewsLost = append(r.PreviewsLost, sha)
			continue
		}
		if err != nil {
			return r, fmt.Errorf("preview %s: %w", sha, err)
		}
		m.Previews = append(m.Previews, p)
	}
	r.Previews = len(m.Previews)

	// 5. Manifest, then the snapshot becomes visible under its final name.
	if err := tv.Check(); err != nil {
		return r, err
	}
	if err := writeJSONAtomic(filepath.Join(partial, ManifestName), m); err != nil {
		return r, err
	}
	if err := hook(PointBeforeFinalize); err != nil {
		return r, err
	}
	final := filepath.Join(t.dir(SnapshotsDir), r.ID)
	if err := os.Rename(partial, final); err != nil {
		return r, err
	}
	if err := syncDir(t.dir(SnapshotsDir)); err != nil {
		return r, err
	}
	r.Path = final
	if opt.Keep > 0 {
		if err := prune(t, r.ID, opt.Keep, r); err != nil {
			r.PruneError = err.Error()
		}
	}
	r.Seconds = time.Since(start).Seconds()
	return r, tv.Check()
}

// snapshotIDRE matches the name of a finished snapshot (newSnapshotID).
var snapshotIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)

// prune removes the oldest finished snapshots other than current so that
// keep remain. Each one is first renamed to a .partial- name (atomic), so
// an interruption leaves a directory the next backup removes, never a
// half-deleted snapshot under a valid name. Only directories named like a
// snapshot and holding a manifest count; anything else is left alone.
func prune(t target, current string, keep int, r *Report) error {
	dir := t.dir(SnapshotsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var others []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == current || !snapshotIDRE.MatchString(e.Name()) {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(dir, e.Name(), ManifestName)); err == nil && fi.Mode().IsRegular() {
			others = append(others, e.Name())
		}
	}
	sort.Strings(others) // the id starts with the UTC time: oldest first
	extra := len(others) - (keep - 1)
	for i := 0; i < extra; i++ {
		id := others[i]
		if err := t.vol.Check(); err != nil {
			return err
		}
		doomed := filepath.Join(dir, partialPrefix+"pruned-"+id)
		if err := os.Rename(filepath.Join(dir, id), doomed); err != nil {
			return err
		}
		if err := syncDir(dir); err != nil {
			return err
		}
		r.Pruned = append(r.Pruned, id)
		if err := os.RemoveAll(doomed); err != nil {
			return err
		}
	}
	return nil
}

// prepareTarget creates the target's directories and removes what an
// interrupted backup left: copies in tmp/ and unfinished snapshots. Both are
// only copies of the source; finished snapshots and pool files are kept.
func prepareTarget(t target) error {
	if err := t.vol.Check(); err != nil {
		return err
	}
	for _, d := range []string{"pool", "previews", SnapshotsDir, "tmp"} {
		if err := storage.MkdirIn(t.dir(d)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(t.dir("tmp"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(t.dir("tmp"), e.Name())); err != nil {
			return err
		}
	}
	snaps, err := os.ReadDir(t.dir(SnapshotsDir))
	if err != nil {
		return err
	}
	for _, e := range snaps {
		if e.IsDir() && strings.HasPrefix(e.Name(), partialPrefix) {
			if err := os.RemoveAll(filepath.Join(t.dir(SnapshotsDir), e.Name())); err != nil {
				return err
			}
		}
	}
	return syncDir(t.dir(SnapshotsDir))
}

// copyBlob makes sure the target pool holds a verified copy of b.
func copyBlob(src *storage.Volume, t target, b BlobEntry, rehash bool, r *Report) error {
	dst := t.poolPath(b.SHA256)
	srcPath := filepath.Join(src.Root(), filepath.FromSlash(blobstore.PoolRelPath(b.SHA256)))
	reuse := func() error {
		r.Reused++
		// Cheap check of the source too, so a backup does not hide a lost
		// original (content damage needs verify -hash).
		if fi, err := os.Lstat(srcPath); err != nil || !fi.Mode().IsRegular() || fi.Size() != b.Bytes {
			r.SourceWarnings = append(r.SourceWarnings, b.SHA256)
		}
		return nil
	}
	fi, err := os.Lstat(dst)
	switch {
	case err == nil && fi.Mode().IsRegular() && fi.Size() == b.Bytes && !rehash:
		return reuse()
	case err == nil && fi.Mode().IsRegular() && fi.Size() == b.Bytes:
		_, sum, err := hashRegular(dst)
		if err != nil {
			return err
		}
		if sum == b.SHA256 {
			return reuse()
		}
		fallthrough
	case err == nil:
		// A damaged backup copy: keep it aside, never delete it.
		if err := moveDamaged(t, dst, b.SHA256); err != nil {
			return err
		}
		r.Replaced++
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := t.vol.Check(); err != nil {
		return err
	}
	tmp, size, sum, err := copyVerified(srcPath, t.dir("tmp"))
	if errors.Is(err, fs.ErrNotExist) {
		r.SourceDamaged = append(r.SourceDamaged, b.SHA256+": missing in the source pool")
		return nil
	}
	if err != nil {
		return fmt.Errorf("copy original %s: %w", b.SHA256, err)
	}
	if size != b.Bytes || sum != b.SHA256 {
		os.Remove(tmp)
		r.SourceDamaged = append(r.SourceDamaged, fmt.Sprintf("%s: source has %d bytes with SHA-256 %s", b.SHA256, size, sum))
		return nil
	}
	if err := t.vol.Check(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := place(tmp, dst, true); err != nil {
		os.Remove(tmp)
		return err
	}
	r.Copied++
	r.CopiedBytes += size
	return nil
}

func moveDamaged(t target, path, sha string) error {
	if err := storage.MkdirIn(t.dir("damaged")); err != nil {
		return err
	}
	dst := filepath.Join(t.dir("damaged"), sha+"."+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	if err := syncDir(t.dir("damaged")); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// copyPreview copies the preview of sha into the target unless an identical
// one is there, and returns its manifest entry. Previews are derived data,
// so an outdated copy is simply replaced.
func copyPreview(src *storage.Volume, t target, sha string) (PreviewEntry, error) {
	srcPath := filepath.Join(src.Root(), filepath.FromSlash(blobstore.PreviewRelPath(sha)))
	size, sum, err := hashRegular(srcPath)
	if err != nil {
		return PreviewEntry{}, err
	}
	p := PreviewEntry{SHA256: sha, Bytes: size, FileSHA256: sum}
	dst := t.previewPath(sha)
	if dsize, dsum, err := hashRegular(dst); err == nil && dsize == size && dsum == sum {
		return p, nil
	}
	if err := t.vol.Check(); err != nil {
		return p, err
	}
	tmp, csize, csum, err := copyVerified(srcPath, t.dir("tmp"))
	if err != nil {
		return p, err
	}
	if csize != size || csum != sum {
		os.Remove(tmp)
		return p, fmt.Errorf("preview changed while copying")
	}
	if err := place(tmp, dst, false); err != nil {
		os.Remove(tmp)
		return p, err
	}
	return p, nil
}
