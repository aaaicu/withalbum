package sqlitestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
)

// Sync feed state, snapshots, removals, cursor acknowledgements and upload
// receipts (sync API v2, migration 0007).

// folderEpoch derives a folder's feed epoch from the database epoch: 16 hex
// digits that differ between folders and change with the database epoch,
// which a restore rotates (internal/backup).
func (s *Store) folderEpoch(folderID string) string {
	sum := sha256.Sum256([]byte(s.epoch + "/" + folderID))
	return hex.EncodeToString(sum[:8])
}

func (s *Store) feedState(ctx context.Context, q queryer, folderID string) (meta.FeedState, error) {
	st := meta.FeedState{Epoch: s.folderEpoch(folderID)}
	err := q.QueryRowContext(ctx, `SELECT s.last_seq, s.pruned_seq FROM folders f JOIN folder_sequences s ON s.folder_id = f.folder_id
		WHERE f.folder_id = ?`, folderID).Scan(&st.LastSeq, &st.PrunedSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.FeedState{}, meta.ErrFolderNotFound
	}
	if err != nil {
		return meta.FeedState{}, err
	}
	return st, nil
}

// FeedState implements meta.MetadataStore.
func (s *Store) FeedState(ctx context.Context, folderID string) (meta.FeedState, error) {
	return s.feedState(ctx, s.db, folderID)
}

// ListFolderAssets implements meta.MetadataStore. The feed state is read
// before the assets, so applying the feed from State afterwards covers
// every change made while the snapshot is paged through.
func (s *Store) ListFolderAssets(ctx context.Context, folderID, after string, limit int) (meta.AssetPage, error) {
	if limit <= 0 {
		return meta.AssetPage{}, meta.ErrInvalidLimit
	}
	state, err := s.FeedState(ctx, folderID)
	if err != nil {
		return meta.AssetPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+assetColumns+` FROM folder_assets fa
		JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND fa.removed_at IS NULL AND fa.asset_id > ? ORDER BY fa.asset_id LIMIT ?`, folderID, after, limit+1)
	if err != nil {
		return meta.AssetPage{}, err
	}
	defer rows.Close()
	page := meta.AssetPage{Assets: []meta.Asset{}, State: state}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return meta.AssetPage{}, err
		}
		if len(page.Assets) == limit {
			page.HasMore = true
			break
		}
		page.Assets = append(page.Assets, a)
	}
	return page, rows.Err()
}

// maxFindHashes bounds one FindFolderAssets call.
const maxFindHashes = 500

// FindFolderAssets implements meta.MetadataStore.
func (s *Store) FindFolderAssets(ctx context.Context, folderID string, shas []string) (map[string]meta.FolderMatch, error) {
	out := map[string]meta.FolderMatch{}
	if len(shas) == 0 {
		return out, nil
	}
	if len(shas) > maxFindHashes {
		return nil, errors.New("sqlitestore: too many hashes")
	}
	args := []any{folderID}
	for _, h := range shas {
		args = append(args, h)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.sha, m.id,
			COALESCE((SELECT c.sha256 FROM asset_components c WHERE c.asset_id = m.id AND c.role = 'paired_video'), '')
		FROM (SELECT a.original_sha256 AS sha, MIN(a.asset_id) AS id FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id
			WHERE fa.folder_id = ? AND fa.removed_at IS NULL AND a.original_sha256 IN (?`+strings.Repeat(",?", len(shas)-1)+`)
			GROUP BY a.original_sha256) m`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		var m meta.FolderMatch
		if err := rows.Scan(&h, &m.AssetID, &m.PairedSHA256); err != nil {
			return nil, err
		}
		out[h] = m
	}
	return out, rows.Err()
}

// nextSeq bumps and returns the folder's sequence inside tx.
func nextSeq(ctx context.Context, tx *sql.Tx, folderID string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO folder_sequences (folder_id, last_seq) VALUES (?, 1)
		ON CONFLICT(folder_id) DO UPDATE SET last_seq = last_seq + 1 RETURNING last_seq`, folderID).Scan(&seq)
	return seq, err
}

// RemoveAsset implements meta.MetadataStore: one transaction re-checks the
// caller, sets folder_assets.removed_at, appends the asset_removed event and
// writes the audit row. Nothing is deleted from disk: the asset row stays
// (old events still reference it) and the original stays in the pool.
func (s *Store) RemoveAsset(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		ts := now()
		res, err := tx.ExecContext(ctx, "UPDATE folder_assets SET removed_at = ? WHERE folder_id = ? AND asset_id = ? AND removed_at IS NULL",
			ts, c.FolderID, c.TargetID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return meta.ErrAssetNotFound
		}
		seq, err := nextSeq(ctx, tx, c.FolderID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?, NULL)`, c.FolderID, seq, meta.EventAssetRemoved, c.TargetID, ts); err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "asset.remove", "asset", c.TargetID,
			map[string]any{"folder_id": c.FolderID, "seq": seq})
	})
}

// AckSyncCursor implements meta.MetadataStore.
func (s *Store) AckSyncCursor(ctx context.Context, c meta.AlbumChange, deviceID string, seq int64) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		st, err := s.feedState(ctx, tx, c.FolderID)
		if err != nil {
			return err
		}
		if seq < 0 || seq > st.LastSeq {
			return meta.ErrCursorInvalid
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO device_sync_cursors (device_id, folder_id, applied_seq, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(device_id, folder_id) DO UPDATE SET applied_seq = excluded.applied_seq, updated_at = excluded.updated_at`,
			deviceID, c.FolderID, seq, now())
		return err
	})
}

func lookupReceipt(ctx context.Context, q queryer, userID, folderID string, id meta.Idempotency) (meta.Receipt, error) {
	var r meta.Receipt
	err := q.QueryRowContext(ctx, `SELECT sha256, bytes, result, asset_id, created_at, COALESCE(paired_sha256, '') FROM upload_receipts
		WHERE user_id = ? AND device_id = ? AND folder_id = ? AND idempotency_key = ?`, userID, id.DeviceID, folderID, id.Key).
		Scan(&r.SHA256, &r.Bytes, &r.Result, &r.AssetID, &r.CreatedAt, &r.PairedSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Receipt{}, meta.ErrReceiptNotFound
	}
	return r, err
}

// LookupReceipt implements meta.MetadataStore.
func (s *Store) LookupReceipt(ctx context.Context, userID, folderID string, id meta.Idempotency) (meta.Receipt, error) {
	return lookupReceipt(ctx, s.db, userID, folderID, id)
}

func insertReceipt(ctx context.Context, tx *sql.Tx, req meta.CommitRequest, result, assetID, ts string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO upload_receipts (user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.UserID, req.Idempotency.DeviceID, req.FolderID, req.Idempotency.Key, req.Asset.SHA256, req.Asset.Bytes, result, assetID, ts)
	return err
}

// PruneUploadReceipts implements meta.MetadataStore.
func (s *Store) PruneUploadReceipts(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM upload_receipts WHERE created_at < ?", before.UTC().Format(timeLayout))
	if err != nil {
		return 0, classify(err)
	}
	return res.RowsAffected()
}

// assetByID reads an asset whatever its folder links.
func assetByID(ctx context.Context, q queryer, assetID string) (meta.Asset, error) {
	return scanAsset(q.QueryRowContext(ctx, "SELECT "+assetColumns+" FROM assets a JOIN blobs b ON b.sha256 = a.original_sha256 WHERE a.asset_id = ?", assetID))
}
