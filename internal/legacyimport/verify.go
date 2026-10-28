package legacyimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
)

const sep = "\x1f"

func line(fields ...string) string { return strings.Join(fields, sep) }

func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// queryLines runs q and renders each row as its columns joined by sep (NULL
// as "<NULL>"), sorted.
func queryLines(ctx context.Context, sqlDB *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		fields := make([]string, len(vals))
		for i, v := range vals {
			fields[i] = "<NULL>"
			if v.Valid {
				fields[i] = v.String
			}
		}
		out = append(out, line(fields...))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// diffLines describes the first differences between want and got.
func diffLines(what string, want, got []string) []string {
	slices.Sort(want)
	if slices.Equal(want, got) {
		return nil
	}
	gotSet := make(map[string]bool, len(got))
	for _, g := range got {
		gotSet[g] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, w := range want {
		wantSet[w] = true
	}
	out := []string{fmt.Sprintf("%s: want %d rows, database has %d", what, len(want), len(got))}
	shown := 0
	for _, w := range want {
		if !gotSet[w] && shown < 3 {
			out = append(out, fmt.Sprintf("%s: missing or different: %q", what, strings.ReplaceAll(w, sep, " | ")))
			shown++
		}
	}
	for _, g := range got {
		if !wantSet[g] && shown < 6 {
			out = append(out, fmt.Sprintf("%s: unexpected: %q", what, strings.ReplaceAll(g, sep, " | ")))
			shown++
		}
	}
	return out
}

// verifyTemp reopens the closed temporary database and compares every
// imported row with state.json, then checkpoints the WAL so that the main
// file alone holds all data before it is renamed.
func verifyTemp(ctx context.Context, path string, st *state, srcSHA string) error {
	sqlDB, err := db.Open(path)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	diffs, err := compare(ctx, sqlDB, st, srcSHA)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerifyFailed, err)
	}
	if len(diffs) > 0 {
		return fmt.Errorf("%w: %s", ErrVerifyFailed, strings.Join(diffs, "; "))
	}
	var busy, logFrames, checkpointed int
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("temporary database: WAL checkpoint did not complete")
	}
	if err := sqlDB.Close(); err != nil {
		return err
	}
	// Closing the last connection removes the WAL. An empty leftover is
	// harmless; one with data would be lost by the rename, so refuse.
	if info, err := os.Lstat(path + "-wal"); err == nil {
		if info.Size() != 0 {
			return errors.New("temporary database: WAL still holds data after checkpoint")
		}
		if err := os.Remove(path + "-wal"); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(path + "-shm"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// compare returns a description of every difference between the database
// and the parsed state.json.
func compare(ctx context.Context, sqlDB *sql.DB, st *state, srcSHA string) ([]string, error) {
	var diffs []string
	if v, err := db.SchemaVersion(ctx, sqlDB); err != nil {
		return nil, err
	} else if v != db.LatestVersion() {
		diffs = append(diffs, fmt.Sprintf("schema version %d, want %d", v, db.LatestVersion()))
	}
	check, err := queryLines(ctx, sqlDB, "PRAGMA integrity_check")
	if err != nil {
		return nil, err
	}
	if !slices.Equal(check, []string{"ok"}) {
		diffs = append(diffs, "integrity_check: "+strings.Join(check, ", "))
	}
	fk, err := queryLines(ctx, sqlDB, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, err
	}
	if len(fk) > 0 {
		diffs = append(diffs, fmt.Sprintf("foreign_key_check: %d violation(s)", len(fk)))
	}

	// Raw row counts catch rows that the joins below would not show.
	counts := []struct {
		table string
		want  int
	}{
		{"users", 1}, {"folders", len(st.Folders)}, {"folder_sequences", len(st.Folders)},
		{"blobs", len(st.Assets)}, {"assets", len(st.Assets)}, {"legacy_asset_map", len(st.Assets)},
		{"folder_assets", st.linkCount()}, {"folder_changes", st.eventCount()}, {"legacy_imports", 1},
	}
	for _, c := range counts {
		var n int
		// Table names are constants above.
		if err := sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+c.table).Scan(&n); err != nil {
			return nil, err
		}
		if n != c.want {
			diffs = append(diffs, fmt.Sprintf("table %s has %d rows, want %d", c.table, n, c.want))
		}
	}

	type table struct {
		what string
		want []string
		q    string
	}
	var folders, assets, links, events []string
	for _, fid := range sortedKeys(st.Folders) {
		f := st.Folders[fid]
		folders = append(folders, line(f.ID, f.Name, f.CreatedAt, strconv.FormatInt(st.NextSeq[fid], 10), "1", "1"))
	}
	for _, sha := range sortedKeys(st.Assets) {
		a := st.Assets[sha]
		assets = append(assets, line(sha, sha, a.Filename, a.MimeType, strconv.FormatInt(a.Bytes, 10),
			boolField(a.PreviewReady), a.UploadedAt, blobstore.PoolRelPath(sha), sqlitestore.MediaKind(a.MimeType), "<NULL>", "<NULL>"))
	}
	for _, fid := range sortedKeys(st.Links) {
		for _, sha := range sortedKeys(st.Links[fid]) {
			l := st.Links[fid][sha]
			links = append(links, line(fid, sha, l.AddedAt, l.UploaderDeviceID, "<NULL>", "<NULL>"))
		}
	}
	for _, fid := range sortedKeys(st.Events) {
		for _, e := range st.Events[fid] {
			events = append(events, line(fid, strconv.FormatInt(e.Seq, 10), e.Kind, e.AssetID, e.OccurredAt, e.UploaderDeviceID))
		}
	}
	for _, t := range []table{
		{"folders", folders, `SELECT f.folder_id, f.folder_name, f.created_at, s.last_seq,
			f.owner_user_id = (SELECT user_id FROM account_bootstrap WHERE singleton = 1),
			f.archived_at IS NULL
			FROM folders f LEFT JOIN folder_sequences s ON s.folder_id = f.folder_id`},
		{"assets", assets, `SELECT m.legacy_sha256, a.original_sha256, a.original_filename, b.mime_type, b.bytes,
			a.preview_ready, a.created_at, b.relative_path, a.media_kind, a.preview_sha256, a.uploaded_by_user_id
			FROM legacy_asset_map m JOIN assets a ON a.asset_id = m.asset_id JOIN blobs b ON b.sha256 = a.original_sha256`},
		{"folder links", links, `SELECT fa.folder_id, m.legacy_sha256, fa.added_at, fa.uploader_device_label,
			fa.removed_at, fa.added_by_user_id
			FROM folder_assets fa JOIN legacy_asset_map m ON m.asset_id = fa.asset_id`},
		{"events", events, `SELECT c.folder_id, c.seq, c.event_type, m.legacy_sha256, c.occurred_at, c.uploader_device_label
			FROM folder_changes c JOIN legacy_asset_map m ON m.asset_id = c.asset_id`},
		{"legacy import", []string{srcSHA}, "SELECT source_sha256 FROM legacy_imports"},
	} {
		got, err := queryLines(ctx, sqlDB, t.q)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.what, err)
		}
		diffs = append(diffs, diffLines(t.what, t.want, got)...)
	}
	return diffs, nil
}
