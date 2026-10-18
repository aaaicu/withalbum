package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
)

// Resumable upload sessions (migration 0008). The rows only record how far
// an upload got; its bytes are in .system/uploads (internal/blobstore) and
// the commit goes through CommitUpload with CommitRequest.UploadSessionID.

// expiry returns ts + meta.UploadSessionTTL in the format of now().
func expiry(ts string) string {
	t, err := time.Parse(timeLayout, ts)
	if err != nil {
		t = time.Now().UTC()
	}
	return t.Add(meta.UploadSessionTTL).UTC().Format(timeLayout)
}

const uploadColumns = `upload_id, user_id, device_id, folder_id, idempotency_key, sha256, total_bytes, received_bytes,
	filename, device_label, status, created_at, expires_at`

func scanUpload(row scanner) (meta.UploadSession, error) {
	var u meta.UploadSession
	err := row.Scan(&u.ID, &u.UserID, &u.DeviceID, &u.FolderID, &u.Key, &u.SHA256, &u.TotalBytes, &u.Received,
		&u.Filename, &u.DeviceLabel, &u.Status, &u.CreatedAt, &u.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.UploadSession{}, meta.ErrUploadNotFound
	}
	return u, err
}

// reservedUploads sums what open, unexpired sessions other than exceptID
// still expect.
func reservedUploads(ctx context.Context, q queryer, exceptID, ts string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_bytes - received_bytes), 0) FROM upload_sessions
		WHERE status = 'open' AND expires_at > ? AND upload_id != ?`, ts, exceptID).Scan(&n)
	return n, err
}

// CreateUploadSession implements meta.MetadataStore.
func (s *Store) CreateUploadSession(ctx context.Context, req meta.NewUploadSession) (meta.UploadSession, bool, string, error) {
	us, created, replaced, err := s.createUploadSession(ctx, req)
	return us, created, replaced, classify(err)
}

func (s *Store) createUploadSession(ctx context.Context, req meta.NewUploadSession) (meta.UploadSession, bool, string, error) {
	if req.Authorize == nil || req.Reserve == nil {
		return meta.UploadSession{}, false, "", errors.New("sqlitestore: upload session needs Authorize and Reserve")
	}
	if req.UserID == "" || req.DeviceID == "" || req.Key == "" || req.TotalBytes <= 0 || len(req.SHA256) != 64 {
		return meta.UploadSession{}, false, "", errors.New("sqlitestore: incomplete upload session request")
	}
	id, err := newID()
	if err != nil {
		return meta.UploadSession{}, false, "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.UploadSession{}, false, "", err
	}
	defer tx.Rollback()
	access, err := getFolderAccess(ctx, tx, req.FolderID, req.UserID)
	if err != nil {
		return meta.UploadSession{}, false, "", err
	}
	p, err := principalTx(ctx, tx, req.UserID, req.SessionID)
	if err != nil {
		return meta.UploadSession{}, false, "", err
	}
	if err := req.Authorize(p, access.Access); err != nil {
		return meta.UploadSession{}, false, "", fmt.Errorf("authorize: %w", err)
	}
	ts := now()
	replaced := ""
	old, err := scanUpload(tx.QueryRowContext(ctx, "SELECT "+uploadColumns+` FROM upload_sessions
		WHERE user_id = ? AND device_id = ? AND folder_id = ? AND idempotency_key = ?`, req.UserID, req.DeviceID, req.FolderID, req.Key))
	switch {
	case err == nil && old.ExpiresAt > ts:
		if old.SHA256 != req.SHA256 || old.TotalBytes != req.TotalBytes {
			return meta.UploadSession{}, false, "", meta.ErrIdempotencyMismatch
		}
		return old, false, "", nil
	case err == nil:
		// Expired and not yet pruned: start over under the same key.
		if _, err := tx.ExecContext(ctx, "DELETE FROM upload_sessions WHERE upload_id = ?", old.ID); err != nil {
			return meta.UploadSession{}, false, "", err
		}
		replaced = old.ID
	case !errors.Is(err, meta.ErrUploadNotFound):
		return meta.UploadSession{}, false, "", err
	}
	switch rc, err := lookupReceipt(ctx, tx, req.UserID, req.FolderID, meta.Idempotency{DeviceID: req.DeviceID, Key: req.Key}); {
	case err == nil && rc.SHA256 != req.SHA256:
		return meta.UploadSession{}, false, "", meta.ErrIdempotencyMismatch
	case err == nil:
		// Committed already (its session, if any, is gone): answer the
		// stored outcome like a replayed commit, read in this transaction.
		a, err := assetByID(ctx, tx, rc.AssetID)
		if err != nil {
			return meta.UploadSession{}, false, "", err
		}
		return meta.UploadSession{}, false, "", &meta.UploadCommittedError{Result: meta.CommitResult{Result: rc.Result, Asset: a, Replayed: true}}
	case !errors.Is(err, meta.ErrReceiptNotFound):
		return meta.UploadSession{}, false, "", err
	}
	var open int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_sessions WHERE user_id = ? AND status = 'open' AND expires_at > ?",
		req.UserID, ts).Scan(&open); err != nil {
		return meta.UploadSession{}, false, "", err
	}
	if open >= meta.MaxOpenUploads {
		return meta.UploadSession{}, false, "", meta.ErrTooManyUploads
	}
	// Under the write lock: no other session can be created between this
	// sum and the insert, so reservations never overlap.
	reserved, err := reservedUploads(ctx, tx, "", ts)
	if err != nil {
		return meta.UploadSession{}, false, "", err
	}
	if err := req.Reserve(reserved); err != nil {
		return meta.UploadSession{}, false, "", fmt.Errorf("reserve: %w", err)
	}
	us := meta.UploadSession{ID: id, UserID: req.UserID, DeviceID: req.DeviceID, FolderID: req.FolderID, Key: req.Key,
		SHA256: req.SHA256, TotalBytes: req.TotalBytes, Filename: req.Filename, DeviceLabel: req.DeviceLabel,
		Status: meta.UploadOpen, CreatedAt: ts, ExpiresAt: expiry(ts)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO upload_sessions (upload_id, user_id, device_id, folder_id, idempotency_key, sha256,
		total_bytes, received_bytes, filename, device_label, status, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 'open', ?, ?, ?)`,
		us.ID, us.UserID, us.DeviceID, us.FolderID, us.Key, us.SHA256, us.TotalBytes, us.Filename, us.DeviceLabel, ts, ts, us.ExpiresAt); err != nil {
		return meta.UploadSession{}, false, "", fmt.Errorf("insert upload session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return meta.UploadSession{}, false, "", err
	}
	return us, true, replaced, nil
}

// GetUploadSession implements meta.MetadataStore.
func (s *Store) GetUploadSession(ctx context.Context, ref meta.UploadRef) (meta.UploadSession, error) {
	us, err := scanUpload(s.db.QueryRowContext(ctx, "SELECT "+uploadColumns+` FROM upload_sessions
		WHERE upload_id = ? AND user_id = ? AND device_id = ? AND folder_id = ? AND expires_at > ?`,
		ref.ID, ref.UserID, ref.DeviceID, ref.FolderID, now()))
	return us, classify(err)
}

// AdvanceUploadSession implements meta.MetadataStore. It is one statement,
// so it needs no explicit transaction.
func (s *Store) AdvanceUploadSession(ctx context.Context, ref meta.UploadRef, from, to int64) error {
	ts := now()
	res, err := s.db.ExecContext(ctx, `UPDATE upload_sessions SET received_bytes = ?, updated_at = ?, expires_at = ?
		WHERE upload_id = ? AND user_id = ? AND device_id = ? AND folder_id = ? AND status = 'open' AND expires_at > ?
		AND received_bytes = ? AND ? <= total_bytes`,
		to, ts, expiry(ts), ref.ID, ref.UserID, ref.DeviceID, ref.FolderID, ts, from, to)
	if err != nil {
		return classify(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	if _, err := s.GetUploadSession(ctx, ref); err != nil {
		return err
	}
	return meta.ErrUploadConflict
}

// DeleteUploadSession implements meta.MetadataStore.
func (s *Store) DeleteUploadSession(ctx context.Context, ref meta.UploadRef) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM upload_sessions WHERE upload_id = ? AND user_id = ? AND device_id = ? AND folder_id = ?",
		ref.ID, ref.UserID, ref.DeviceID, ref.FolderID)
	if err != nil {
		return classify(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return meta.ErrUploadNotFound
	}
	return nil
}

// ReservedUploadBytes implements meta.MetadataStore.
func (s *Store) ReservedUploadBytes(ctx context.Context, exceptID string) (int64, error) {
	n, err := reservedUploads(ctx, s.db, exceptID, now())
	return n, classify(err)
}

// ListUploadSessions implements meta.MetadataStore.
func (s *Store) ListUploadSessions(ctx context.Context) ([]meta.UploadSession, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+uploadColumns+" FROM upload_sessions ORDER BY upload_id")
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []meta.UploadSession{}
	for rows.Next() {
		us, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, us)
	}
	return out, rows.Err()
}

// PruneUploadSessions implements meta.MetadataStore.
func (s *Store) PruneUploadSessions(ctx context.Context, t time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "DELETE FROM upload_sessions WHERE expires_at < ? RETURNING upload_id", t.UTC().Format(timeLayout))
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, classify(rows.Err())
}

// PendingPreviews implements meta.MetadataStore.
func (s *Store) PendingPreviews(ctx context.Context, limit int) ([]meta.PreviewJob, error) {
	if limit <= 0 {
		return nil, meta.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.original_sha256, b.mime_type FROM assets a JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE a.preview_state = 'pending' GROUP BY a.original_sha256 ORDER BY MIN(a.created_at), a.original_sha256 LIMIT ?`, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []meta.PreviewJob{}
	for rows.Next() {
		var j meta.PreviewJob
		if err := rows.Scan(&j.SHA256, &j.MimeType); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetPreviewStatus implements meta.MetadataStore.
func (s *Store) SetPreviewStatus(ctx context.Context, sha, status string) (int, error) {
	n, err := s.setPreviewStatus(ctx, sha, status)
	return n, classify(err)
}

func (s *Store) setPreviewStatus(ctx context.Context, sha, status string) (int, error) {
	ready, state, err := previewColumns(meta.Asset{PreviewStatus: status})
	if err != nil {
		return 0, err
	}
	if status == meta.PreviewStatusPending {
		return 0, errors.New("sqlitestore: a preview cannot become pending again")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// The folders holding an asset of these bytes whose preview is pending,
	// read before the update changes which assets are pending.
	// Each event carries the uploader label of its folder link (the device
	// that added the asset there), like that asset's asset_added event.
	rows, err := tx.QueryContext(ctx, `SELECT fa.folder_id, fa.asset_id, fa.uploader_device_label FROM assets a JOIN folder_assets fa ON fa.asset_id = a.asset_id
		WHERE a.original_sha256 = ? AND a.preview_state = 'pending' AND fa.removed_at IS NULL ORDER BY fa.folder_id, fa.asset_id`, sha)
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
	res, err := tx.ExecContext(ctx, "UPDATE assets SET preview_ready = ?, preview_state = ? WHERE original_sha256 = ? AND preview_state = 'pending'",
		ready, state, sha)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	ts := now()
	for _, l := range links {
		seq, err := nextSeq(ctx, tx, l.folder)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?, ?)`, l.folder, seq, meta.EventAssetUpdated, l.asset, ts, l.device); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
