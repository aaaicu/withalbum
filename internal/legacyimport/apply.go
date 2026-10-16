package legacyimport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

var (
	// ErrNoLegacyState means there is no .system/state.json to import.
	ErrNoLegacyState = errors.New("no .system/state.json to import")
	// ErrDatabaseExists means withalbum.db exists and does not record the
	// current state.json as imported; importing over it is refused.
	ErrDatabaseExists = errors.New("metadata database already exists")
	// ErrInvalidState means state.json is unreadable, incomplete or
	// internally inconsistent (see Report.Problems).
	ErrInvalidState = errors.New("state.json is invalid or inconsistent")
	// ErrFilesInvalid means an original or preview file is missing or does
	// not match state.json (see Report.Problems).
	ErrFilesInvalid = errors.New("original or preview files do not match state.json")
	// ErrStateChanged means state.json changed while the import ran (an old
	// server still running?).
	ErrStateChanged = errors.New("state.json changed during the import")
	// ErrVerifyFailed means the imported database did not match state.json.
	ErrVerifyFailed = errors.New("imported database does not match state.json")
	// ErrStaleFiles means database side files without a database exist.
	ErrStaleFiles = errors.New("stale database files")
)

// Options configures Run.
type Options struct {
	// Apply performs the import. Without it Run only reads and reports.
	Apply bool
	// Progress receives progress lines while originals are re-hashed (nil:
	// none).
	Progress io.Writer
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	// hook, if set, is called at named points of the apply procedure. A
	// non-nil result stops Run right there without any cleanup, which is
	// how tests simulate a crash.
	hook func(point string) error
}

// Crash points passed to Options.hook, in procedure order.
const (
	pointAfterBackup      = "after-backup"
	pointBeforeCommit     = "before-commit"
	pointAfterCommit      = "after-commit"
	pointAfterVerify      = "after-verify"
	pointAfterDBRename    = "after-db-rename"
	pointAfterStateRename = "after-state-rename"
)

// crashError marks an error returned by Options.hook.
type crashError struct{ error }

func (e crashError) Unwrap() error { return e.error }

// File name suffix timestamp: UTC, second resolution.
const tsFormat = "20060102T150405Z"

// paths are the files Run touches, all inside <root>/.system.
type paths struct {
	root, sys, state, db, tmp string
}

func newPaths(root string) paths {
	dbPath := sqlitestore.DBPath(root)
	return paths{root: root, sys: filepath.Dir(dbPath), state: jsonstore.StatePath(root), db: dbPath, tmp: dbPath + ".importing"}
}

func (p paths) rel(path string) string {
	r, err := filepath.Rel(p.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// Run imports (Options.Apply) or checks (dry-run) the state.json of a
// verified volume. It always returns a report; err is non-nil whenever the
// import cannot be done or did not complete.
func Run(ctx context.Context, vol *storage.Volume, opt Options) (*Report, error) {
	r := &Report{Mode: "dry-run", Result: "failed", FolderSummaries: []FolderSummary{}, Warnings: []string{}, Problems: []string{}}
	if opt.Apply {
		r.Mode = "apply"
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.hook == nil {
		opt.hook = func(string) error { return nil }
	}
	err := run(ctx, vol, opt, r)
	if err != nil {
		r.Error = err.Error()
	} else {
		r.Result = "ok"
	}
	return r, err
}

func run(ctx context.Context, vol *storage.Volume, opt Options, r *Report) (err error) {
	crashAt := func(point string) error {
		if err := opt.hook(point); err != nil {
			return crashError{err}
		}
		return nil
	}
	if err := vol.Check(); err != nil {
		return err
	}
	p := newPaths(vol.Root())
	src, err := os.ReadFile(p.state)
	if errors.Is(err, fs.ErrNotExist) {
		if ok, _ := exists(p.db); ok {
			return fmt.Errorf("%w (%s exists; already imported?)", ErrNoLegacyState, p.rel(p.db))
		}
		return ErrNoLegacyState
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(src)
	srcSHA := hex.EncodeToString(sum[:])
	r.SourceSHA256, r.SourceBytes = srcSHA, int64(len(src))

	hasDB, err := exists(p.db)
	if err != nil {
		return err
	}
	if hasDB {
		// Only an import of exactly these bytes may continue: it was
		// verified and renamed into place, but state.json was not renamed.
		imported, err := recordsImport(ctx, p.db, srcSHA)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrDatabaseExists, p.rel(p.db), err)
		}
		if !imported {
			return fmt.Errorf("%w: %s does not record this state.json as imported; refusing to import over it", ErrDatabaseExists, p.rel(p.db))
		}
		r.Resumed = true
		r.DatabasePath = p.rel(p.db)
		if !opt.Apply {
			r.Warnings = append(r.Warnings, "an earlier apply imported this state.json but stopped before renaming it; apply will only rename it")
			return nil
		}
		return finish(vol, p, srcSHA, opt.Now(), r, crashAt)
	}
	if err := checkNoStaleDB(p); err != nil {
		return err
	}

	st, err := parseState(src)
	if err != nil {
		r.Problems = append(r.Problems, err.Error())
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	r.parsed = true
	r.Folders, r.Assets, r.Links, r.Events = len(st.Folders), len(st.Assets), st.linkCount(), st.eventCount()
	for _, fid := range sortedKeys(st.Folders) {
		r.FolderSummaries = append(r.FolderSummaries, FolderSummary{ID: fid, Name: st.Folders[fid].Name,
			Assets: len(st.Links[fid]), Events: len(st.Events[fid]), LastSeq: st.NextSeq[fid]})
	}
	problems, warnings := st.validate()
	r.Problems = append(r.Problems, problems...)
	r.Warnings = append(r.Warnings, warnings...)
	fc, err := verifyFiles(ctx, p.root, st, opt.Progress)
	if err != nil {
		return err
	}
	r.Problems = append(r.Problems, fc.problems...)
	r.Warnings = append(r.Warnings, fc.warnings...)
	r.OriginalBytes, r.PreviewsReady, r.HashSeconds = fc.bytes, fc.previewsReady, fc.elapsed.Seconds()
	leftovers, err := leftoverWarnings(p)
	if err != nil {
		return err
	}
	r.Warnings = append(r.Warnings, leftovers...)
	if len(problems) > 0 {
		return fmt.Errorf("%w: %d problem(s)", ErrInvalidState, len(r.Problems))
	}
	if len(fc.problems) > 0 {
		return fmt.Errorf("%w: %d problem(s)", ErrFilesInvalid, len(fc.problems))
	}
	if !opt.Apply {
		return nil
	}

	// From here on files are written. Anything that fails before the
	// database is renamed into place discards the temporary database; a
	// simulated crash leaves it for the next run to discard.
	placed := false
	defer func() {
		if err != nil && !placed && !errors.As(err, new(crashError)) {
			if rmErr := removeTemp(p); rmErr != nil {
				err = errors.Join(err, rmErr)
			}
		}
	}()

	// 1. Backup copy of state.json.
	if err := vol.Check(); err != nil {
		return err
	}
	ts := opt.Now().UTC()
	if r.BackupPath, err = writeBackup(p, src, ts); err != nil {
		return fmt.Errorf("backup state.json: %w", err)
	}
	if err := crashAt(pointAfterBackup); err != nil {
		return err
	}

	// 2. Build the temporary database: schema and all rows in one transaction.
	if err := vol.Check(); err != nil {
		return err
	}
	if err := removeTemp(p); err != nil { // left over by an interrupted run
		return err
	}
	stored := *r
	stored.Result = "ok"
	stored.DatabasePath = p.rel(p.db)
	reportJSON, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if err := buildTemp(ctx, p.tmp, st, srcSHA, string(reportJSON), ts.Format(time.RFC3339Nano), crashAt); err != nil {
		return err
	}
	if err := vol.Check(); err != nil {
		return err
	}
	if err := crashAt(pointAfterCommit); err != nil {
		return err
	}

	// 3. Reopen and compare every row with state.json.
	if err := verifyTemp(ctx, p.tmp, st, srcSHA); err != nil {
		return err
	}
	if err := syncFile(p.tmp); err != nil {
		return err
	}
	if err := syncDir(p.sys); err != nil {
		return err
	}
	if err := crashAt(pointAfterVerify); err != nil {
		return err
	}

	// 4. Move the database into place.
	if err := vol.Check(); err != nil {
		return err
	}
	if err := sameState(p, srcSHA); err != nil {
		return err
	}
	if ok, err := exists(p.db); err != nil || ok {
		return errors.Join(fmt.Errorf("%w: %s appeared during the import", ErrDatabaseExists, p.rel(p.db)), err)
	}
	if err := checkNoStaleDB(p); err != nil {
		return err
	}
	if err := os.Rename(p.tmp, p.db); err != nil {
		return err
	}
	// The database is in place and records the import. From here on nothing
	// is discarded; a re-run finishes the remaining step.
	placed = true
	r.DatabasePath = p.rel(p.db)
	if err := syncDir(p.sys); err != nil {
		return fmt.Errorf("sync %s after rename: %w (run apply again)", p.rel(p.sys), err)
	}
	if err := crashAt(pointAfterDBRename); err != nil {
		return err
	}

	// 5. Keep state.json under a new name so the server no longer sees it.
	return finish(vol, p, srcSHA, ts, r, crashAt)
}

// finish renames state.json to state.json.migrated-<UTC> once the database
// records its import.
func finish(vol *storage.Volume, p paths, srcSHA string, ts time.Time, r *Report, crashAt func(string) error) error {
	if err := vol.Check(); err != nil {
		return err
	}
	if err := removeTemp(p); err != nil {
		return err
	}
	if err := sameState(p, srcSHA); err != nil {
		return err
	}
	target := p.state + ".migrated-" + ts.UTC().Format(tsFormat)
	if ok, err := exists(target); err != nil || ok {
		return errors.Join(fmt.Errorf("%s already exists; not overwriting it", p.rel(target)), err)
	}
	if err := os.Rename(p.state, target); err != nil {
		return err
	}
	if err := syncDir(p.sys); err != nil {
		return err
	}
	r.MigratedPath = p.rel(target)
	if err := crashAt(pointAfterStateRename); err != nil {
		return err
	}
	return vol.Check()
}

// sameState fails with ErrStateChanged unless state.json still hashes to srcSHA.
func sameState(p paths, srcSHA string) error {
	b, err := os.ReadFile(p.state)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStateChanged, err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != srcSHA {
		return fmt.Errorf("%w: stop every WithAlbum server using this storage and run the import again", ErrStateChanged)
	}
	return nil
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

// recordsImport reports whether the database at path has a legacy_imports row
// for srcSHA. It only reads.
func recordsImport(ctx context.Context, path, srcSHA string) (bool, error) {
	sqlDB, err := db.Open(path)
	if err != nil {
		return false, err
	}
	defer sqlDB.Close()
	var ok bool
	err = sqlDB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM legacy_imports WHERE source_sha256 = ?)", srcSHA).Scan(&ok)
	return ok, err
}

// checkNoStaleDB refuses SQLite side files of withalbum.db when the database
// itself does not exist: a stale -wal would be replayed into the new file.
func checkNoStaleDB(p paths) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if ok, err := exists(p.db + suffix); err != nil || ok {
			return errors.Join(fmt.Errorf("%w: %s exists without %s; move it away after checking it",
				ErrStaleFiles, p.rel(p.db+suffix), p.rel(p.db)), err)
		}
	}
	return nil
}

// removeTemp deletes the temporary database and its side files. It never
// touches withalbum.db or state.json.
func removeTemp(p paths) error {
	var errs []error
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(p.tmp + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// backupTempPrefix differs from the PoC's own ".state-" temp prefix, so
// leftovers of each can be told apart.
const backupTempPrefix = ".import-backup-"

// leftoverWarnings reports files left by interrupted PoC writes or uploads.
// They are never removed.
func leftoverWarnings(p paths) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(p.sys)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			out = append(out, fmt.Sprintf("%s is a temp file left by an interrupted PoC save (left untouched)", p.rel(filepath.Join(p.sys, e.Name()))))
		}
		if e.Name() == filepath.Base(p.tmp) {
			out = append(out, fmt.Sprintf("%s is left from an interrupted import; apply discards it", p.rel(p.tmp)))
		}
	}
	incoming, err := os.ReadDir(filepath.Join(p.sys, "incoming"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if len(incoming) > 0 {
		out = append(out, fmt.Sprintf(".system/incoming holds %d unfinished upload file(s) (left untouched)", len(incoming)))
	}
	return out, nil
}

// writeBackup copies src to .system/state.json.bak-<UTC> (temp file, fsync,
// rename, directory fsync) and reads it back. A backup with identical bytes
// that already exists is reused; one with other bytes is never overwritten.
func writeBackup(p paths, src []byte, ts time.Time) (string, error) {
	final := p.state + ".bak-" + ts.Format(tsFormat)
	if b, err := os.ReadFile(final); err == nil {
		if bytes.Equal(b, src) {
			return p.rel(final), nil
		}
		return "", fmt.Errorf("%s already exists with other content", p.rel(final))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := removeBackupTemps(p); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(p.sys, backupTempPrefix+"*") // mode 0600
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(src); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if ok, err := exists(final); err != nil || ok {
		return "", errors.Join(fmt.Errorf("%s appeared during the backup", p.rel(final)), err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	if err := syncDir(p.sys); err != nil {
		return "", err
	}
	b, err := os.ReadFile(final)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(b, src) {
		return "", fmt.Errorf("backup %s does not read back identically", p.rel(final))
	}
	return p.rel(final), nil
}

// removeBackupTemps deletes partial backup copies of an interrupted run.
func removeBackupTemps(p paths) error {
	matches, err := filepath.Glob(filepath.Join(p.sys, backupTempPrefix+"*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
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

// syncDir fsyncs a directory so that renames in it are durable. File systems
// that do not support fsync on directories are tolerated.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildTemp creates the database file at path and writes the schema and every
// imported row in a single transaction.
func buildTemp(ctx context.Context, path string, st *state, srcSHA, reportJSON, ts string, crashAt func(string) error) error {
	// Same reason as sqlitestore.Open: metadata must not be world-readable.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	sqlDB, err := db.Open(path)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.MigrateNewTx(ctx, tx); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	owner, err := sqlitestore.EnsureBootstrapOwnerTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("bootstrap owner: %w", err)
	}
	exec := func(what, q string, rows func(stmt *sql.Stmt) error) error {
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		defer stmt.Close()
		if err := rows(stmt); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	if err := exec("folders", "INSERT INTO folders (folder_id, folder_name, owner_user_id, created_at) VALUES (?, ?, ?, ?)", func(stmt *sql.Stmt) error {
		for _, fid := range sortedKeys(st.Folders) {
			f := st.Folders[fid]
			if _, err := stmt.ExecContext(ctx, f.ID, f.Name, owner, f.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := exec("folder sequences", "INSERT INTO folder_sequences (folder_id, last_seq) VALUES (?, ?)", func(stmt *sql.Stmt) error {
		for _, fid := range sortedKeys(st.Folders) {
			if _, err := stmt.ExecContext(ctx, fid, st.NextSeq[fid]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	assetIDs := make(map[string]string, len(st.Assets)) // legacy sha -> asset_id
	for _, sha := range sortedKeys(st.Assets) {
		if assetIDs[sha], err = newID(); err != nil {
			return err
		}
	}
	if err := exec("blobs", `INSERT INTO blobs (sha256, bytes, mime_type, relative_path, verified_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, func(stmt *sql.Stmt) error {
		for _, sha := range sortedKeys(st.Assets) {
			a := st.Assets[sha]
			if _, err := stmt.ExecContext(ctx, sha, a.Bytes, a.MimeType, blobstore.PoolRelPath(sha), ts, a.UploadedAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := exec("assets", `INSERT INTO assets (asset_id, original_sha256, original_filename, media_kind, preview_ready, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, func(stmt *sql.Stmt) error {
		for _, sha := range sortedKeys(st.Assets) {
			a := st.Assets[sha]
			if _, err := stmt.ExecContext(ctx, assetIDs[sha], sha, a.Filename, sqlitestore.MediaKind(a.MimeType), a.PreviewReady, a.UploadedAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := exec("legacy asset map", "INSERT INTO legacy_asset_map (legacy_sha256, asset_id) VALUES (?, ?)", func(stmt *sql.Stmt) error {
		for _, sha := range sortedKeys(st.Assets) {
			if _, err := stmt.ExecContext(ctx, sha, assetIDs[sha]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := exec("folder links", `INSERT INTO folder_assets (folder_id, asset_id, added_at, uploader_device_label)
		VALUES (?, ?, ?, ?)`, func(stmt *sql.Stmt) error {
		for _, fid := range sortedKeys(st.Links) {
			for _, sha := range sortedKeys(st.Links[fid]) {
				l := st.Links[fid][sha]
				if _, err := stmt.ExecContext(ctx, fid, assetIDs[sha], l.AddedAt, l.UploaderDeviceID); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := exec("events", `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
		VALUES (?, ?, ?, ?, ?, ?)`, func(stmt *sql.Stmt) error {
		for _, fid := range sortedKeys(st.Events) {
			for _, e := range st.Events[fid] {
				if _, err := stmt.ExecContext(ctx, fid, e.Seq, e.Kind, assetIDs[e.AssetID], e.OccurredAt, e.UploaderDeviceID); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	importID, err := newID()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO legacy_imports (import_id, source_sha256, imported_at, report_json) VALUES (?, ?, ?, ?)",
		importID, srcSHA, ts, reportJSON); err != nil {
		return fmt.Errorf("legacy import record: %w", err)
	}
	if err := crashAt(pointBeforeCommit); err != nil {
		return err
	}
	return tx.Commit()
}
