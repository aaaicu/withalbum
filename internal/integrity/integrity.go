// Package integrity checks a storage volume without changing it: the SQLite
// database (PRAGMA integrity_check and foreign_key_check) and every original
// recorded in it (present in .system/pool, right size and, optionally, right
// SHA-256). It is the engine of "withalbum-server verify".
//
// Startup reconciliation (blobstore.Reconcile) only compares sizes, so it
// cannot see an original whose bytes changed without changing its length (a
// bad sector, a bit flip). Run with Options.Hash regularly to catch that
// while a good copy still exists in a backup.
//
// Run only reads, so it is safe while the server is running. It pages
// through the blobs table in short queries instead of holding one read
// transaction for the whole (possibly hours long) scan, which would keep the
// server's WAL from being checkpointed. Pool files that appear without a
// database row are reported as unreferenced but are not an error: an upload
// in progress writes its file shortly before its row.
package integrity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Problem kinds.
const (
	KindMissing      = "missing"       // recorded original not in the pool
	KindSizeMismatch = "size_mismatch" // pool file has another size
	KindHashMismatch = "hash_mismatch" // pool file has another SHA-256 (Options.Hash)
	KindUnreadable   = "unreadable"    // the pool file could not be read
)

// maxListed bounds the per-kind example lists; counts are always exact.
const maxListed = 100

// pageSize is the number of blobs read per query.
const pageSize = 500

// progressInterval is how often Run reports progress.
const progressInterval = 5 * time.Second

// Options configures Run.
type Options struct {
	// Hash re-reads every original and compares its SHA-256 with its name.
	// This reads the whole library.
	Hash bool
	// Progress, if set, receives a progress line every few seconds.
	Progress io.Writer
}

// AssetRef identifies an asset whose original is affected by a problem.
type AssetRef struct {
	FolderID string `json:"folder_id,omitempty"`
	AssetID  string `json:"asset_id"`
	Filename string `json:"original_filename"`
}

// Problem is one damaged or missing original.
type Problem struct {
	Kind   string     `json:"kind"`
	SHA256 string     `json:"sha256"`
	Path   string     `json:"path"` // relative to the storage root
	Detail string     `json:"detail,omitempty"`
	Assets []AssetRef `json:"assets"`
}

// Report is the result of Run. Counts are exact; lists are bounded.
type Report struct {
	StartedAt     string   `json:"started_at"`
	Seconds       float64  `json:"seconds"`
	HashChecked   bool     `json:"hash_checked"`
	SchemaVersion int      `json:"schema_version"`
	DBIntegrity   []string `json:"db_integrity"` // ["ok"] when healthy
	ForeignKeyErr int      `json:"foreign_key_violations"`

	Blobs        int   `json:"blobs"`
	BlobBytes    int64 `json:"blob_bytes"`
	Hashed       int   `json:"hashed"`
	Missing      int   `json:"missing"`
	SizeMismatch int   `json:"size_mismatch"`
	HashMismatch int   `json:"hash_mismatch"`
	Unreadable   int   `json:"unreadable"`
	// Problems lists up to maxListed problems of each kind with the assets
	// that use the original.
	Problems []Problem `json:"problems"`

	// Warnings: not errors of the library itself.
	PreviewsMissing     int      `json:"previews_missing"`
	PreviewsMissingList []string `json:"previews_missing_sample,omitempty"` // sha256
	Unreferenced        int      `json:"unreferenced_pool_files"`
	UnreferencedList    []string `json:"unreferenced_pool_files_sample,omitempty"`
	Unexpected          int      `json:"unexpected_entries"`
	UnexpectedList      []string `json:"unexpected_entries_sample,omitempty"`
	IncomingFiles       int      `json:"incoming_files"`
	OrphanFiles         int      `json:"orphan_files"`
	OrphanBytes         int64    `json:"orphan_bytes"`
	problemCounts       map[string]int
}

// Healthy reports whether the database is sound and every original is
// present and intact (as far as checked).
func (r *Report) Healthy() bool {
	return len(r.DBIntegrity) == 1 && r.DBIntegrity[0] == "ok" && r.ForeignKeyErr == 0 &&
		r.Missing == 0 && r.SizeMismatch == 0 && r.HashMismatch == 0 && r.Unreadable == 0
}

func (r *Report) addProblem(p Problem) {
	if r.problemCounts == nil {
		r.problemCounts = map[string]int{}
	}
	r.problemCounts[p.Kind]++
	if r.problemCounts[p.Kind] <= maxListed {
		r.Problems = append(r.Problems, p)
	}
}

func addSample(list *[]string, v string) {
	if len(*list) < maxListed {
		*list = append(*list, v)
	}
}

// WriteSummary prints a human-readable report.
func (r *Report) WriteSummary(w io.Writer) {
	status := "OK"
	if !r.Healthy() {
		status = "PROBLEMS FOUND"
	}
	fmt.Fprintf(w, "verify: %s\n", status)
	fmt.Fprintf(w, "  database: integrity_check=%s foreign_key_violations=%d schema_version=%d\n",
		strings.Join(r.DBIntegrity, "; "), r.ForeignKeyErr, r.SchemaVersion)
	hash := "size only (run with -hash to compare SHA-256)"
	if r.HashChecked {
		hash = fmt.Sprintf("SHA-256 of %d checked", r.Hashed)
	}
	fmt.Fprintf(w, "  originals: %d (%d bytes), %s\n", r.Blobs, r.BlobBytes, hash)
	fmt.Fprintf(w, "  missing=%d size_mismatch=%d hash_mismatch=%d unreadable=%d\n", r.Missing, r.SizeMismatch, r.HashMismatch, r.Unreadable)
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  %s: %s", strings.ToUpper(p.Kind), p.Path)
		if p.Detail != "" {
			fmt.Fprintf(w, " (%s)", p.Detail)
		}
		fmt.Fprintln(w)
		for _, a := range p.Assets {
			fmt.Fprintf(w, "      asset %s %q in folder %s\n", a.AssetID, a.Filename, a.FolderID)
		}
	}
	if r.PreviewsMissing > 0 {
		fmt.Fprintf(w, "  warning: %d preview(s) marked ready are missing (previews are derived data)\n", r.PreviewsMissing)
	}
	if r.Unreferenced > 0 {
		fmt.Fprintf(w, "  warning: %d pool file(s) without a database row (an upload in progress, or moved to %s at the next start)\n", r.Unreferenced, blobstore.OrphansRelDir)
	}
	if r.Unexpected > 0 {
		fmt.Fprintf(w, "  warning: %d unexpected entries in .system/pool, e.g. %v\n", r.Unexpected, r.UnexpectedList)
	}
	if r.IncomingFiles > 0 {
		fmt.Fprintf(w, "  info: %d file(s) in .system/incoming (uploads in progress or left by a crash; removed at the next start)\n", r.IncomingFiles)
	}
	if r.OrphanFiles > 0 {
		fmt.Fprintf(w, "  info: %s holds %d file(s), %d bytes. Nothing there is referenced by the database and nothing is deleted automatically; see docs/BACKUP_RESTORE.md before removing them.\n",
			blobstore.OrphansRelDir, r.OrphanFiles, r.OrphanBytes)
	}
	if !r.Healthy() && (r.Missing+r.SizeMismatch+r.HashMismatch) > 0 {
		fmt.Fprintln(w, "  Damaged originals are left untouched. Recover them from a backup (docs/BACKUP_RESTORE.md) or by uploading the same file again.")
	}
}

// OpenDB opens the existing database of a verified volume. Unlike
// sqlitestore.Open it never creates or migrates the database.
func OpenDB(vol *storage.Volume) (*sql.DB, error) {
	path := filepath.Join(vol.Root(), ".system", "withalbum.db")
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no database at %s: nothing to check", path)
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return db.Open(path)
}

// CheckDB runs PRAGMA integrity_check and foreign_key_check on sqlDB.
func CheckDB(ctx context.Context, sqlDB *sql.DB) (integrity []string, fkViolations int, err error) {
	rows, err := sqlDB.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, 0, err
		}
		if len(integrity) < maxListed {
			integrity = append(integrity, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	rows, err = sqlDB.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		fkViolations++
	}
	rows.Close()
	return integrity, fkViolations, rows.Err()
}

// Run checks the volume. It returns an error only when the check itself
// could not run; problems found are in the report (see Report.Healthy).
func Run(ctx context.Context, vol *storage.Volume, sqlDB *sql.DB, opt Options) (*Report, error) {
	start := time.Now()
	r := &Report{StartedAt: start.UTC().Format(time.RFC3339), HashChecked: opt.Hash}
	if err := vol.Check(); err != nil {
		return r, err
	}
	var err error
	if r.SchemaVersion, err = db.SchemaVersion(ctx, sqlDB); err != nil {
		return r, err
	}
	if r.DBIntegrity, r.ForeignKeyErr, err = CheckDB(ctx, sqlDB); err != nil {
		return r, fmt.Errorf("database check: %w", err)
	}
	if len(r.DBIntegrity) != 1 || r.DBIntegrity[0] != "ok" {
		// Reading rows of a damaged database may fail or mislead; stop here.
		r.Seconds = time.Since(start).Seconds()
		return r, nil
	}
	if err := checkBlobs(ctx, vol, sqlDB, opt, r); err != nil {
		return r, err
	}
	if err := checkPool(ctx, vol, sqlDB, r); err != nil {
		return r, err
	}
	if err := countOther(vol, r); err != nil {
		return r, err
	}
	r.Seconds = time.Since(start).Seconds()
	return r, nil
}

type blobRow struct {
	sha          string
	size         int64
	previewReady bool
}

func checkBlobs(ctx context.Context, vol *storage.Volume, sqlDB *sql.DB, opt Options, r *Report) error {
	root := vol.Root()
	last := time.Now()
	var hashedBytes int64
	after := ""
	for {
		page, err := blobPage(ctx, sqlDB, after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, b := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.Blobs++
			r.BlobBytes += b.size
			rel := blobstore.PoolRelPath(b.sha)
			p := Problem{SHA256: b.sha, Path: rel}
			fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				r.Missing++
				p.Kind = KindMissing
			case err != nil:
				r.Unreadable++
				p.Kind, p.Detail = KindUnreadable, err.Error()
			case !fi.Mode().IsRegular():
				r.Unreadable++
				p.Kind, p.Detail = KindUnreadable, "not a regular file"
			case fi.Size() != b.size:
				r.SizeMismatch++
				p.Kind, p.Detail = KindSizeMismatch, fmt.Sprintf("%d bytes, database says %d", fi.Size(), b.size)
			case opt.Hash:
				sum, err := hashFile(filepath.Join(root, filepath.FromSlash(rel)))
				r.Hashed++
				hashedBytes += b.size
				if err != nil {
					r.Unreadable++
					p.Kind, p.Detail = KindUnreadable, err.Error()
				} else if sum != b.sha {
					r.HashMismatch++
					p.Kind, p.Detail = KindHashMismatch, "content changed: SHA-256 is "+sum
				}
			}
			if p.Kind != "" {
				if p.Assets, err = assetsOf(ctx, sqlDB, b.sha); err != nil {
					return err
				}
				r.addProblem(p)
			}
			if b.previewReady {
				if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(blobstore.PreviewRelPath(b.sha)))); err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
					r.PreviewsMissing++
					addSample(&r.PreviewsMissingList, b.sha)
				}
			}
			if opt.Progress != nil && time.Since(last) >= progressInterval {
				last = time.Now()
				fmt.Fprintf(opt.Progress, "verify: %d originals checked (%d bytes hashed)\n", r.Blobs, hashedBytes)
			}
		}
		after = page[len(page)-1].sha
	}
	return nil
}

// blobPage returns up to pageSize blobs after the given sha256, in order.
func blobPage(ctx context.Context, sqlDB *sql.DB, after string) ([]blobRow, error) {
	rows, err := sqlDB.QueryContext(ctx, `SELECT b.sha256, b.bytes,
		EXISTS(SELECT 1 FROM assets a WHERE a.original_sha256 = b.sha256 AND a.preview_ready = 1)
		FROM blobs b WHERE b.sha256 > ? ORDER BY b.sha256 LIMIT ?`, after, pageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []blobRow
	for rows.Next() {
		var b blobRow
		if err := rows.Scan(&b.sha, &b.size, &b.previewReady); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// assetsOf lists the assets (and their folders) that use the original sha.
func assetsOf(ctx context.Context, sqlDB *sql.DB, sha string) ([]AssetRef, error) {
	rows, err := sqlDB.QueryContext(ctx, `SELECT COALESCE(fa.folder_id, ''), a.asset_id, a.original_filename
		FROM assets a LEFT JOIN folder_assets fa ON fa.asset_id = a.asset_id
		WHERE a.original_sha256 = ? ORDER BY a.asset_id LIMIT 50`, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssetRef{}
	for rows.Next() {
		var a AssetRef
		if err := rows.Scan(&a.FolderID, &a.AssetID, &a.Filename); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// checkPool reports pool files without a blobs row and entries WithAlbum
// did not create.
func checkPool(ctx context.Context, vol *storage.Volume, sqlDB *sql.DB, r *Report) error {
	pool := filepath.Join(vol.Root(), ".system", "pool")
	prefixes, err := os.ReadDir(pool)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	rel := func(p string) string {
		s, _ := filepath.Rel(vol.Root(), p)
		return filepath.ToSlash(s)
	}
	for _, p := range prefixes {
		dir := filepath.Join(pool, p.Name())
		if !p.IsDir() || len(p.Name()) != 2 || !isHex(p.Name()) {
			r.Unexpected++
			addSample(&r.UnexpectedList, rel(dir))
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := f.Name()
			if strings.HasPrefix(name, blobstore.LinkPrefix) {
				continue // a second name of a pool file (upload in progress, or removed at the next start)
			}
			if !f.Type().IsRegular() || len(name) != 64 || !isHex(name) || name[:2] != p.Name() {
				r.Unexpected++
				addSample(&r.UnexpectedList, rel(filepath.Join(dir, name)))
				continue
			}
			var known bool
			if err := sqlDB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256 = ?)", name).Scan(&known); err != nil {
				return err
			}
			if !known {
				r.Unreferenced++
				addSample(&r.UnreferencedList, name)
			}
		}
	}
	return nil
}

// countOther counts files in .system/incoming and .system/orphans.
func countOther(vol *storage.Volume, r *Report) error {
	count := func(dir string, files *int, bytes *int64) error {
		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				*files++
				if bytes != nil {
					if info, err := d.Info(); err == nil {
						*bytes += info.Size()
					}
				}
			}
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := count(filepath.Join(vol.Root(), ".system", "incoming"), &r.IncomingFiles, nil); err != nil {
		return err
	}
	return count(filepath.Join(vol.Root(), filepath.FromSlash(blobstore.OrphansRelDir)), &r.OrphanFiles, &r.OrphanBytes)
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
