package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aaaicu/withalbum/internal/integrity"
)

// Layout of a backup target directory (registered with
// storage.InitBackupTarget):
//
//	.withalbum-backup-target.json   marker
//	withalbum.lock                  held while a backup writes
//	pool/<sha[:2]>/<sha>            originals, shared by all snapshots
//	previews/<sha[:2]>/<sha>.jpg    previews, shared by all snapshots
//	snapshots/<id>/withalbum.db     consistent copy of the database
//	snapshots/<id>/manifest.json    what the snapshot contains, with checksums
//	snapshots/.partial-<id>/        a snapshot being written (never restored)
//	tmp/                            copies in progress
//	damaged/                        backup copies found damaged (kept, not deleted)
//
// A snapshot directory appears under its final name only after its database,
// every original and its manifest are written and fsynced.
const (
	ManifestName  = "manifest.json"
	DBName        = "withalbum.db"
	SnapshotsDir  = "snapshots"
	partialPrefix = ".partial-"
	manifestKind  = "withalbum-backup"
	formatVersion = 1
)

// FileEntry describes the database file of a snapshot.
type FileEntry struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// BlobEntry is one original. Its pool file name is its SHA-256.
type BlobEntry struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// PreviewEntry is the preview of the original SHA256; FileSHA256 is the
// hash of the preview file itself.
type PreviewEntry struct {
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	FileSHA256 string `json:"file_sha256"`
}

// Manifest is snapshots/<id>/manifest.json.
type Manifest struct {
	Format         int            `json:"format"`
	Kind           string         `json:"kind"`
	ID             string         `json:"id"`
	CreatedAt      string         `json:"created_at"`
	ServerVersion  string         `json:"server_version"`
	SourceVolumeID string         `json:"source_volume_id"`
	SchemaVersion  int            `json:"schema_version"`
	Database       FileEntry      `json:"database"`
	BlobBytes      int64          `json:"blob_bytes"`
	Blobs          []BlobEntry    `json:"blobs"`
	Previews       []PreviewEntry `json:"previews"`
}

// readManifest loads and validates a manifest.
func readManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", ErrBackupDamaged, err)
	}
	bad := func(format string, args ...any) (*Manifest, error) {
		return nil, fmt.Errorf("%w: manifest: %s", ErrBackupDamaged, fmt.Sprintf(format, args...))
	}
	if m.Kind != manifestKind {
		return bad("not a WithAlbum backup manifest")
	}
	if m.Format != formatVersion {
		return bad("unsupported format %d", m.Format)
	}
	if m.Database.Name != DBName || !isSHA(m.Database.SHA256) || m.Database.Bytes <= 0 {
		return bad("invalid database entry")
	}
	var total int64
	for i, b := range m.Blobs {
		if !isSHA(b.SHA256) || b.Bytes <= 0 || (i > 0 && m.Blobs[i-1].SHA256 >= b.SHA256) {
			return bad("invalid or unsorted blob entry %d", i)
		}
		total += b.Bytes
	}
	if total != m.BlobBytes {
		return bad("blob_bytes %d does not match the entries (%d)", m.BlobBytes, total)
	}
	for i, p := range m.Previews {
		if !isSHA(p.SHA256) || !isSHA(p.FileSHA256) || p.Bytes <= 0 {
			return bad("invalid preview entry %d", i)
		}
	}
	return &m, nil
}

// snapshotInfo is what a database snapshot says about its own contents.
type snapshotInfo struct {
	schema       int
	blobs        []BlobEntry
	previewReady []string // sha256 of originals with a preview
}

// openSnapshot opens a database file that nothing else uses, read-only and
// immutable: SQLite then creates no -wal/-shm file and takes no lock, so the
// file is not changed by reading it.
func openSnapshot(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1"}
	sqlDB, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	return sqlDB, nil
}

// inspectSnapshot runs integrity_check and foreign_key_check on a database
// snapshot and reads its blob list.
func inspectSnapshot(ctx context.Context, path string) (*snapshotInfo, error) {
	sqlDB, err := openSnapshot(path)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()
	ic, fk, err := integrity.CheckDB(ctx, sqlDB)
	if err != nil {
		return nil, fmt.Errorf("%w: database snapshot: %v", ErrBackupDamaged, err)
	}
	if len(ic) != 1 || ic[0] != "ok" || fk != 0 {
		return nil, fmt.Errorf("%w: database snapshot: integrity_check=%s foreign_key_violations=%d", ErrBackupDamaged, strings.Join(ic, "; "), fk)
	}
	info := &snapshotInfo{}
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&info.schema); err != nil {
		return nil, err
	}
	if info.schema < 1 {
		return nil, fmt.Errorf("%w: database snapshot has no WithAlbum schema", ErrBackupDamaged)
	}
	rows, err := sqlDB.QueryContext(ctx, "SELECT sha256, bytes FROM blobs ORDER BY sha256")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b BlobEntry
		if err := rows.Scan(&b.SHA256, &b.Bytes); err != nil {
			rows.Close()
			return nil, err
		}
		if !isSHA(b.SHA256) || b.Bytes <= 0 {
			rows.Close()
			return nil, fmt.Errorf("%w: database snapshot holds an invalid blob row", ErrBackupDamaged)
		}
		info.blobs = append(info.blobs, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// preview_ready exists from schema version 3 on.
	if info.schema >= 3 {
		rows, err = sqlDB.QueryContext(ctx, "SELECT DISTINCT original_sha256 FROM assets WHERE preview_ready = 1 ORDER BY original_sha256")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return nil, err
			}
			info.previewReady = append(info.previewReady, sha)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return info, nil
}

// sameBlobs fails unless a and b list the same blobs in the same order.
func sameBlobs(a, b []BlobEntry) error {
	if len(a) != len(b) {
		return fmt.Errorf("%w: manifest lists %d originals, database snapshot %d", ErrBackupDamaged, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Errorf("%w: manifest and database snapshot disagree on original %s", ErrBackupDamaged, a[i].SHA256)
		}
	}
	return nil
}

// errorsOf joins up to 20 messages of a list into one error.
func errorsOf(base error, msgs []string) error {
	if len(msgs) == 0 {
		return nil
	}
	shown := msgs
	if len(shown) > 20 {
		shown = append(shown[:20:20], fmt.Sprintf("... and %d more", len(msgs)-20))
	}
	return errors.Join(append([]error{base}, func() []error {
		out := make([]error, len(shown))
		for i, m := range shown {
			out[i] = errors.New(m)
		}
		return out
	}()...)...)
}
