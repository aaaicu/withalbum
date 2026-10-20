package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aaaicu/withalbum/internal/meta"
)

// PendingCaptures implements meta.MetadataStore.
func (s *Store) PendingCaptures(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, meta.ErrInvalidLimit
	}
	// Two partial indexes, one per state; UNION removes the duplicates.
	rows, err := s.db.QueryContext(ctx, `SELECT sha256 FROM blobs WHERE capture_state = 'pending'
		UNION SELECT sha256 FROM blobs WHERE duration_state = 'pending' ORDER BY sha256 LIMIT ?`, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, classify(rows.Err())
}

// SetCaptureTimes implements meta.MetadataStore.
func (s *Store) SetCaptureTimes(ctx context.Context, results []meta.CaptureResult) (int, error) {
	n, err := s.setCaptureTimes(ctx, results)
	return n, classify(err)
}

func (s *Store) setCaptureTimes(ctx context.Context, results []meta.CaptureResult) (int, error) {
	if len(results) == 0 {
		return 0, nil
	}
	for _, r := range results {
		if r.CapturedAt == "" && r.CapturedUTC != "" {
			return 0, errors.New("sqlitestore: a capture instant without a capture time")
		}
		if r.DurationMS < 0 {
			return 0, errors.New("sqlitestore: a negative duration")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	events := 0
	ts := now()
	for _, r := range results {
		res, err := tx.ExecContext(ctx, "UPDATE blobs SET capture_state = 'done', captured_at = ?, captured_utc = ? WHERE sha256 = ? AND capture_state = 'pending'",
			nullable(r.CapturedAt), nullable(r.CapturedUTC), r.SHA256)
		if err != nil {
			return 0, err
		}
		captured, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		var dur any
		if r.DurationMS > 0 {
			dur = r.DurationMS
		}
		res, err = tx.ExecContext(ctx, "UPDATE blobs SET duration_state = 'done', duration_ms = ? WHERE sha256 = ? AND duration_state = 'pending'", dur, r.SHA256)
		if err != nil {
			return 0, err
		}
		timed, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		newTime, newDuration := captured > 0 && r.CapturedAt != "", timed > 0 && r.DurationMS > 0
		if !newTime && !newDuration {
			continue // settled already, or nothing a client sees changed
		}
		// Every folder link of an asset of these bytes. The event carries
		// the uploader label of the link, like its asset_added event.
		// A duration is also shown on the component it belongs to (the
		// paired video of a Live Photo), so its assets change too.
		rows, err := tx.QueryContext(ctx, `SELECT fa.folder_id, fa.asset_id, fa.uploader_device_label FROM assets a JOIN folder_assets fa ON fa.asset_id = a.asset_id
			WHERE fa.removed_at IS NULL AND (a.original_sha256 = ? OR (? AND a.asset_id IN (SELECT asset_id FROM asset_components WHERE sha256 = ?)))
			ORDER BY fa.folder_id, fa.asset_id`, r.SHA256, newDuration, r.SHA256)
		if err != nil {
			return 0, err
		}
		type link struct {
			folder, asset string
			device        sql.NullString
		}
		var links []link
		for rows.Next() {
			var l link
			if err := rows.Scan(&l.folder, &l.asset, &l.device); err != nil {
				rows.Close()
				return 0, err
			}
			links = append(links, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		for _, l := range links {
			seq, err := nextSeq(ctx, tx, l.folder)
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
				VALUES (?, ?, ?, ?, ?, ?)`, l.folder, seq, meta.EventAssetUpdated, l.asset, ts, l.device); err != nil {
				return 0, err
			}
			events++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return events, nil
}
