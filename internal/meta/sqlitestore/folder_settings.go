package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aaaicu/withalbum/internal/meta"
)

// SetHideLocation implements meta.Memberships. Like the other album
// changes it re-authorizes the caller inside the transaction and writes an
// audit_log row (folder.set_hide_location) when the value changes.
func (s *Store) SetHideLocation(ctx context.Context, c meta.AlbumChange, hide bool) (meta.FolderAccess, error) {
	var out meta.FolderAccess
	err := s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		out = fa
		if fa.Folder.HideLocation == hide {
			return nil // unchanged: nothing written
		}
		if _, err := tx.ExecContext(ctx, "UPDATE folders SET hide_location = ? WHERE folder_id = ? AND archived_at IS NULL", hide, c.FolderID); err != nil {
			return err
		}
		out.Folder.HideLocation = hide
		// Every asset of the folder with a location (its own or its paired
		// video's) changes for members: one asset_updated each, carrying the
		// uploader label of the link like its asset_added event.
		rows, err := tx.QueryContext(ctx, `SELECT fa.asset_id, fa.uploader_device_label FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id
			JOIN blobs b ON b.sha256 = a.original_sha256
			WHERE fa.folder_id = ? AND fa.removed_at IS NULL AND (b.location_lat IS NOT NULL OR EXISTS(
				SELECT 1 FROM asset_components ac JOIN blobs cb ON cb.sha256 = ac.sha256 WHERE ac.asset_id = a.asset_id AND cb.location_lat IS NOT NULL))
			ORDER BY fa.asset_id`, c.FolderID)
		if err != nil {
			return err
		}
		type link struct {
			asset  string
			device sql.NullString
		}
		var links []link
		for rows.Next() {
			var l link
			if err := rows.Scan(&l.asset, &l.device); err != nil {
				rows.Close()
				return err
			}
			links = append(links, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		ts := now()
		for _, l := range links {
			seq, err := nextSeq(ctx, tx, c.FolderID)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
				VALUES (?, ?, ?, ?, ?, ?)`, c.FolderID, seq, meta.EventAssetUpdated, l.asset, ts, l.device); err != nil {
				return err
			}
		}
		return recordAlbum(ctx, tx, c.ActorID, "folder.set_hide_location", "folder", c.FolderID,
			map[string]any{"hide_location": hide, "assets_updated": len(links)})
	})
	if err != nil {
		return meta.FolderAccess{}, err
	}
	return out, nil
}

// HideMyLocation implements meta.Memberships.
func (s *Store) HideMyLocation(ctx context.Context, userID string) (bool, error) {
	var hide bool
	err := s.db.QueryRowContext(ctx, "SELECT hide_my_location FROM users WHERE user_id = ?", userID).Scan(&hide)
	if errors.Is(err, sql.ErrNoRows) {
		return false, meta.ErrUserNotFound
	}
	return hide, classify(err)
}

// SetHideMyLocation implements meta.Memberships.
func (s *Store) SetHideMyLocation(ctx context.Context, userID string, hide bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback()
	var cur bool
	err = tx.QueryRowContext(ctx, "SELECT hide_my_location FROM users WHERE user_id = ?", userID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.ErrUserNotFound
	}
	if err != nil {
		return classify(err)
	}
	if cur == hide {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET hide_my_location = ? WHERE user_id = ?", hide, userID); err != nil {
		return classify(err)
	}
	// Every live folder link of an asset this account uploaded that has a
	// location (its own or its paired video's) changes for other readers.
	rows, err := tx.QueryContext(ctx, `SELECT fa.folder_id, fa.asset_id, fa.uploader_device_label FROM folder_assets fa
		JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		JOIN folders f ON f.folder_id = fa.folder_id
		WHERE a.uploaded_by_user_id = ? AND fa.removed_at IS NULL AND f.archived_at IS NULL AND (b.location_lat IS NOT NULL OR EXISTS(
			SELECT 1 FROM asset_components ac JOIN blobs cb ON cb.sha256 = ac.sha256 WHERE ac.asset_id = a.asset_id AND cb.location_lat IS NOT NULL))
		ORDER BY fa.folder_id, fa.asset_id`, userID)
	if err != nil {
		return classify(err)
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
			return classify(err)
		}
		links = append(links, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return classify(err)
	}
	ts := now()
	for _, l := range links {
		seq, err := nextSeq(ctx, tx, l.folder)
		if err != nil {
			return classify(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?, ?)`, l.folder, seq, meta.EventAssetUpdated, l.asset, ts, l.device); err != nil {
			return classify(err)
		}
	}
	if err := recordAlbum(ctx, tx, userID, "user.set_hide_my_location", "user", userID,
		map[string]any{"hide_my_location": hide, "assets_updated": len(links)}); err != nil {
		return classify(err)
	}
	return classify(tx.Commit())
}
