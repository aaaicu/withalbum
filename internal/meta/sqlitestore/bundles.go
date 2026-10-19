package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Upload bundles (migration 0011): several originals uploaded through one
// upload session each and committed as one asset. The bundle row carries
// the client's Idempotency-Key; its component sessions carry an internal
// key (componentKey), so the client key names exactly one request, a
// single original or a bundle.

// componentKey is the idempotency_key of a bundle's component session. "#"
// and "/" are outside the client key syntax, so it never equals one.
func componentKey(bundleID, role string) string { return "#b/" + bundleID + "/" + role }

const bundleColumns = `bundle_id, user_id, device_id, folder_id, idempotency_key, kind, status, device_label, created_at, expires_at`

func scanBundle(row scanner) (meta.Bundle, error) {
	var b meta.Bundle
	err := row.Scan(&b.ID, &b.UserID, &b.DeviceID, &b.FolderID, &b.Key, &b.Kind, &b.Status, &b.DeviceLabel, &b.CreatedAt, &b.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Bundle{}, meta.ErrBundleNotFound
	}
	return b, err
}

// rowsQueryer is satisfied by *sql.DB and *sql.Tx.
type rowsQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// bundleComponents reads the component sessions of a bundle, still first.
func bundleComponents(ctx context.Context, q rowsQueryer, bundleID string) ([]meta.UploadSession, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+uploadColumns+` FROM upload_sessions WHERE bundle_id = ?
		ORDER BY CASE role WHEN 'still' THEN 0 ELSE 1 END, upload_id`, bundleID)
	if err != nil {
		return nil, err
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

// bundleMatches reports whether the bundle's components are the declared
// originals (same role, SHA-256 and size).
func bundleMatches(have []meta.UploadSession, want []meta.NewBundleComponent) bool {
	if len(have) != len(want) {
		return false
	}
	for _, w := range want {
		found := false
		for _, h := range have {
			if h.Role == w.Role && h.SHA256 == w.SHA256 && h.TotalBytes == w.Bytes {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// liveComponents returns the still and the paired video of a Live Photo
// bundle request, or an error unless it holds exactly those two.
func liveComponents(cs []meta.NewBundleComponent) (still, video meta.NewBundleComponent, err error) {
	var n [2]int
	for _, c := range cs {
		switch c.Role {
		case meta.RoleStill:
			still = c
			n[0]++
		case meta.RolePairedVideo:
			video = c
			n[1]++
		default:
			n[0] = 9
		}
		if c.Bytes <= 0 || len(c.SHA256) != 64 {
			n[0] = 9
		}
	}
	if len(cs) != 2 || n != [2]int{1, 1} {
		return still, video, errors.New("sqlitestore: a live photo bundle needs one still and one paired video")
	}
	return still, video, nil
}

// CreateBundle implements meta.MetadataStore.
func (s *Store) CreateBundle(ctx context.Context, req meta.NewBundle) (meta.Bundle, bool, []string, error) {
	b, created, replaced, err := s.createBundle(ctx, req)
	return b, created, replaced, classify(err)
}

func (s *Store) createBundle(ctx context.Context, req meta.NewBundle) (meta.Bundle, bool, []string, error) {
	if req.Authorize == nil || req.Reserve == nil {
		return meta.Bundle{}, false, nil, errors.New("sqlitestore: upload bundle needs Authorize and Reserve")
	}
	if req.UserID == "" || req.DeviceID == "" || req.Key == "" || req.Kind != meta.BundleLivePhoto {
		return meta.Bundle{}, false, nil, errors.New("sqlitestore: incomplete upload bundle request")
	}
	still, video, err := liveComponents(req.Components)
	if err != nil {
		return meta.Bundle{}, false, nil, err
	}
	ids := make([]string, 3)
	for i := range ids {
		if ids[i], err = newID(); err != nil {
			return meta.Bundle{}, false, nil, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.Bundle{}, false, nil, err
	}
	defer tx.Rollback()
	access, err := getFolderAccess(ctx, tx, req.FolderID, req.UserID)
	if err != nil {
		return meta.Bundle{}, false, nil, err
	}
	p, err := principalTx(ctx, tx, req.UserID, req.SessionID)
	if err != nil {
		return meta.Bundle{}, false, nil, err
	}
	if err := req.Authorize(p, access.Access); err != nil {
		return meta.Bundle{}, false, nil, fmt.Errorf("authorize: %w", err)
	}
	ts := now()
	var replaced []string
	old, err := scanBundle(tx.QueryRowContext(ctx, "SELECT "+bundleColumns+` FROM upload_bundles
		WHERE user_id = ? AND device_id = ? AND folder_id = ? AND idempotency_key = ?`, req.UserID, req.DeviceID, req.FolderID, req.Key))
	switch {
	case err == nil:
		if old.Components, err = bundleComponents(ctx, tx, old.ID); err != nil {
			return meta.Bundle{}, false, nil, err
		}
		if old.ExpiresAt > ts {
			if !bundleMatches(old.Components, req.Components) {
				return meta.Bundle{}, false, nil, meta.ErrIdempotencyMismatch
			}
			return old, false, nil, nil
		}
		// Expired and not yet pruned: start over under the same key.
		for _, c := range old.Components {
			replaced = append(replaced, c.ID)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM upload_bundles WHERE bundle_id = ?", old.ID); err != nil {
			return meta.Bundle{}, false, nil, err
		}
	case !errors.Is(err, meta.ErrBundleNotFound):
		return meta.Bundle{}, false, nil, err
	}
	// The key of a single original (an open session) is not a bundle key.
	single, err := scanUpload(tx.QueryRowContext(ctx, "SELECT "+uploadColumns+` FROM upload_sessions
		WHERE user_id = ? AND device_id = ? AND folder_id = ? AND idempotency_key = ?`, req.UserID, req.DeviceID, req.FolderID, req.Key))
	switch {
	case err == nil && single.ExpiresAt > ts:
		return meta.Bundle{}, false, nil, meta.ErrIdempotencyMismatch
	case err == nil:
		if _, err := tx.ExecContext(ctx, "DELETE FROM upload_sessions WHERE upload_id = ?", single.ID); err != nil {
			return meta.Bundle{}, false, nil, err
		}
		replaced = append(replaced, single.ID)
	case !errors.Is(err, meta.ErrUploadNotFound):
		return meta.Bundle{}, false, nil, err
	}
	switch rc, err := lookupReceipt(ctx, tx, req.UserID, req.FolderID, meta.Idempotency{DeviceID: req.DeviceID, Key: req.Key}); {
	case err == nil && (rc.SHA256 != still.SHA256 || rc.PairedSHA256 != video.SHA256):
		return meta.Bundle{}, false, nil, meta.ErrIdempotencyMismatch
	case err == nil:
		// Committed already (its bundle is gone): answer the stored
		// outcome like a replayed commit, read in this transaction.
		a, err := assetByID(ctx, tx, rc.AssetID)
		if err != nil {
			return meta.Bundle{}, false, nil, err
		}
		return meta.Bundle{}, false, nil, &meta.UploadCommittedError{Result: meta.CommitResult{Result: rc.Result, Asset: a, Replayed: true}}
	case !errors.Is(err, meta.ErrReceiptNotFound):
		return meta.Bundle{}, false, nil, err
	}
	var open int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_sessions WHERE user_id = ? AND status = 'open' AND expires_at > ?",
		req.UserID, ts).Scan(&open); err != nil {
		return meta.Bundle{}, false, nil, err
	}
	if open+2 > meta.MaxOpenUploads {
		return meta.Bundle{}, false, nil, meta.ErrTooManyUploads
	}
	reserved, err := reservedUploads(ctx, tx, "", ts)
	if err != nil {
		return meta.Bundle{}, false, nil, err
	}
	if err := req.Reserve(reserved); err != nil {
		return meta.Bundle{}, false, nil, fmt.Errorf("reserve: %w", err)
	}
	b := meta.Bundle{ID: ids[0], UserID: req.UserID, DeviceID: req.DeviceID, FolderID: req.FolderID, Key: req.Key, Kind: req.Kind,
		Status: meta.UploadOpen, DeviceLabel: req.DeviceLabel, CreatedAt: ts, ExpiresAt: expiry(ts)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO upload_bundles (bundle_id, user_id, device_id, folder_id, idempotency_key, kind, status,
		device_label, created_at, updated_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?)`,
		b.ID, b.UserID, b.DeviceID, b.FolderID, b.Key, b.Kind, b.DeviceLabel, ts, ts, b.ExpiresAt); err != nil {
		return meta.Bundle{}, false, nil, fmt.Errorf("insert upload bundle: %w", err)
	}
	for i, c := range []meta.NewBundleComponent{still, video} {
		us := meta.UploadSession{ID: ids[i+1], UserID: req.UserID, DeviceID: req.DeviceID, FolderID: req.FolderID,
			Key: componentKey(b.ID, c.Role), SHA256: c.SHA256, TotalBytes: c.Bytes, Filename: c.Filename, DeviceLabel: req.DeviceLabel,
			Status: meta.UploadOpen, CreatedAt: ts, ExpiresAt: b.ExpiresAt, BundleID: b.ID, Role: c.Role}
		if _, err := tx.ExecContext(ctx, `INSERT INTO upload_sessions (upload_id, user_id, device_id, folder_id, idempotency_key, sha256,
			total_bytes, received_bytes, filename, device_label, status, created_at, updated_at, expires_at, bundle_id, role)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 'open', ?, ?, ?, ?, ?)`,
			us.ID, us.UserID, us.DeviceID, us.FolderID, us.Key, us.SHA256, us.TotalBytes, us.Filename, us.DeviceLabel, ts, ts, us.ExpiresAt,
			us.BundleID, us.Role); err != nil {
			return meta.Bundle{}, false, nil, fmt.Errorf("insert component session: %w", err)
		}
		b.Components = append(b.Components, us)
	}
	if err := tx.Commit(); err != nil {
		return meta.Bundle{}, false, nil, err
	}
	return b, true, replaced, nil
}

// GetBundle implements meta.MetadataStore.
func (s *Store) GetBundle(ctx context.Context, ref meta.BundleRef) (meta.Bundle, error) {
	b, err := scanBundle(s.db.QueryRowContext(ctx, "SELECT "+bundleColumns+` FROM upload_bundles
		WHERE bundle_id = ? AND user_id = ? AND device_id = ? AND folder_id = ? AND expires_at > ?`,
		ref.ID, ref.UserID, ref.DeviceID, ref.FolderID, now()))
	if err != nil {
		return meta.Bundle{}, classify(err)
	}
	b.Components, err = bundleComponents(ctx, s.db, b.ID)
	return b, classify(err)
}

// DeleteBundle implements meta.MetadataStore.
func (s *Store) DeleteBundle(ctx context.Context, ref meta.BundleRef) ([]string, error) {
	ids, err := s.deleteBundle(ctx, ref)
	return ids, classify(err)
}

func (s *Store) deleteBundle(ctx context.Context, ref meta.BundleRef) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := queryIDs(ctx, tx, "SELECT upload_id FROM upload_sessions WHERE bundle_id = ? ORDER BY upload_id", ref.ID)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM upload_bundles WHERE bundle_id = ? AND user_id = ? AND device_id = ? AND folder_id = ?",
		ref.ID, ref.UserID, ref.DeviceID, ref.FolderID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, meta.ErrBundleNotFound
	}
	return ids, tx.Commit()
}

// CommitBundle implements meta.MetadataStore. Everything happens in one
// BEGIN IMMEDIATE transaction, like CommitUpload.
func (s *Store) CommitBundle(ctx context.Context, req meta.BundleCommit) (meta.CommitResult, error) {
	res, err := s.commitBundle(ctx, req)
	return res, classify(err)
}

func (s *Store) commitBundle(ctx context.Context, req meta.BundleCommit) (meta.CommitResult, error) {
	ref, still, video := req.Ref, req.Still, req.Video
	if req.Authorize == nil {
		return meta.CommitResult{}, errors.New("sqlitestore: a bundle commit needs Authorize")
	}
	if len(still.SHA256) != 64 || len(video.SHA256) != 64 || still.Bytes <= 0 || video.Bytes <= 0 {
		return meta.CommitResult{}, errors.New("sqlitestore: incomplete bundle commit")
	}
	if req.Pairing != meta.PairingVerified && req.Pairing != meta.PairingUnverified {
		return meta.CommitResult{}, fmt.Errorf("sqlitestore: unknown pairing %q", req.Pairing)
	}
	previewReady, previewState, err := previewColumns(still)
	if err != nil {
		return meta.CommitResult{}, err
	}
	newAssetID, err := newID()
	if err != nil {
		return meta.CommitResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.CommitResult{}, err
	}
	defer tx.Rollback()
	// Under the write lock from the first statement: what is read here
	// stays as it is until COMMIT (see commitUpload).
	access, err := getFolderAccess(ctx, tx, ref.FolderID, ref.UserID)
	if err != nil {
		return meta.CommitResult{}, err
	}
	p, err := principalTx(ctx, tx, ref.UserID, req.SessionID)
	if err != nil {
		return meta.CommitResult{}, err
	}
	if err := req.Authorize(p, access.Access); err != nil {
		return meta.CommitResult{}, fmt.Errorf("authorize: %w", err)
	}
	addOnConflict := req.AddOnConflict != nil && req.AddOnConflict()
	b, err := scanBundle(tx.QueryRowContext(ctx, "SELECT "+bundleColumns+` FROM upload_bundles
		WHERE bundle_id = ? AND user_id = ? AND device_id = ? AND folder_id = ?`, ref.ID, ref.UserID, ref.DeviceID, ref.FolderID))
	if err != nil {
		return meta.CommitResult{}, err
	}
	ts := now()
	// completeBundle marks the bundle and its sessions completed; every
	// outcome below calls it before COMMIT.
	completeBundle := func() error {
		if b.Status != meta.UploadOpen {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE upload_bundles SET status = 'completed', updated_at = ?, expires_at = ?
			WHERE bundle_id = ? AND status = 'open' AND expires_at > ?`, ts, expiry(ts), b.ID, ts)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return meta.ErrBundleNotFound
		}
		_, err = tx.ExecContext(ctx, `UPDATE upload_sessions SET status = 'completed', received_bytes = total_bytes, updated_at = ?, expires_at = ?
			WHERE bundle_id = ? AND status = 'open'`, ts, expiry(ts), b.ID)
		return err
	}
	idem := meta.Idempotency{DeviceID: ref.DeviceID, Key: b.Key}
	switch rc, err := lookupReceipt(ctx, tx, ref.UserID, ref.FolderID, idem); {
	case err == nil && (rc.SHA256 != still.SHA256 || rc.PairedSHA256 != video.SHA256):
		return meta.CommitResult{}, meta.ErrIdempotencyMismatch
	case err == nil:
		// The same request again: answer what was committed then.
		a, err := assetByID(ctx, tx, rc.AssetID)
		if err != nil {
			return meta.CommitResult{}, err
		}
		if b.Status == meta.UploadOpen {
			if err := completeBundle(); err != nil {
				return meta.CommitResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return meta.CommitResult{}, err
			}
		}
		return meta.CommitResult{Result: rc.Result, Asset: a, Replayed: true}, nil
	case !errors.Is(err, meta.ErrReceiptNotFound):
		return meta.CommitResult{}, err
	}
	if b.Status != meta.UploadOpen || b.ExpiresAt <= ts {
		return meta.CommitResult{}, meta.ErrBundleNotFound
	}

	// The folder's assets with this still (removed ones do not count).
	rows, err := tx.QueryContext(ctx, "SELECT "+assetColumns+`, (SELECT count(*) FROM folder_assets o WHERE o.asset_id = a.asset_id AND o.folder_id <> fa.folder_id)
		FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND a.original_sha256 = ? AND fa.removed_at IS NULL ORDER BY fa.rowid LIMIT 100`, ref.FolderID, still.SHA256)
	if err != nil {
		return meta.CommitResult{}, err
	}
	var same, upgradable *meta.Asset
	held := false
	for rows.Next() {
		var a meta.Asset
		var state, captured, uploader, kind, pairing, paired sql.NullString
		var shared int
		if err := rows.Scan(&a.ID, &a.SHA256, &a.Filename, &a.MimeType, &a.Bytes, &a.PreviewReady, &state, &a.UploadedAt, &captured, &uploader,
			&kind, &pairing, &paired, &shared); err != nil {
			rows.Close()
			return meta.CommitResult{}, err
		}
		a.PreviewStatus = previewStatus(a.PreviewReady, state)
		a.CapturedAt, a.UploaderName = captured.String, uploader.String
		if err := setLivePhoto(&a, kind, pairing, paired); err != nil {
			rows.Close()
			return meta.CommitResult{}, err
		}
		held = true
		switch {
		case a.PairedVideo.SHA256 == video.SHA256 && same == nil:
			same = &a
		case a.PairedVideo.SHA256 == "" && a.MediaKind == meta.MediaImage && shared == 0 && upgradable == nil:
			upgradable = &a
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return meta.CommitResult{}, err
	}

	insertBlob := func(sha, mime string, size int64) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO blobs (sha256, bytes, mime_type, relative_path, verified_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(sha256) DO NOTHING`, sha, size, mime, blobstore.PoolRelPath(sha), ts, ts)
		if err != nil {
			return fmt.Errorf("insert blob: %w", err)
		}
		return nil
	}
	insertComponent := func(assetID string) error {
		if err := insertBlob(video.SHA256, video.MimeType, video.Bytes); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_components (asset_id, role, sha256, filename, pairing_id, added_at)
			VALUES (?, 'paired_video', ?, ?, ?, ?)`, assetID, video.SHA256, video.Filename, nullable(req.PairingID), ts); err != nil {
			return fmt.Errorf("insert component: %w", err)
		}
		return nil
	}
	event := func(kind, assetID string, label any) error {
		seq, err := nextSeq(ctx, tx, ref.FolderID)
		if err != nil {
			return fmt.Errorf("next seq: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?, ?)`, ref.FolderID, seq, kind, assetID, ts, label); err != nil {
			return fmt.Errorf("insert change: %w", err)
		}
		return nil
	}

	var result, assetID string
	switch {
	case same != nil:
		result, assetID = meta.ResultAlreadyPresent, same.ID
	case upgradable != nil:
		result, assetID = meta.ResultUpgraded, upgradable.ID
		if err := insertComponent(assetID); err != nil {
			return meta.CommitResult{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE assets SET media_kind = 'live_photo', pairing_state = ? WHERE asset_id = ?",
			req.Pairing, assetID); err != nil {
			return meta.CommitResult{}, err
		}
		// Like a preview update: the event carries the label of the device
		// that added the asset to this folder.
		var label sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT uploader_device_label FROM folder_assets WHERE folder_id = ? AND asset_id = ?",
			ref.FolderID, assetID).Scan(&label); err != nil {
			return meta.CommitResult{}, err
		}
		if err := event(meta.EventAssetUpdated, assetID, label); err != nil {
			return meta.CommitResult{}, err
		}
		if err := recordAlbum(ctx, tx, ref.UserID, "asset.attach_component", "asset", assetID,
			map[string]any{"folder_id": ref.FolderID, "role": meta.RolePairedVideo, "pairing": req.Pairing}); err != nil {
			return meta.CommitResult{}, err
		}
	case held && !addOnConflict:
		return meta.CommitResult{}, meta.ErrComponentConflict
	default:
		result, assetID = meta.ResultCreated, newAssetID
		if err := insertBlob(still.SHA256, still.MimeType, still.Bytes); err != nil {
			return meta.CommitResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO assets (asset_id, original_sha256, original_filename, media_kind, preview_ready, preview_state,
			uploaded_by_user_id, created_at, pairing_state) VALUES (?, ?, ?, 'live_photo', ?, ?, ?, ?, ?)`,
			assetID, still.SHA256, still.Filename, previewReady, previewState, nullable(ref.UserID), ts, req.Pairing); err != nil {
			return meta.CommitResult{}, fmt.Errorf("insert asset: %w", err)
		}
		if err := insertComponent(assetID); err != nil {
			return meta.CommitResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_assets (folder_id, asset_id, added_by_user_id, added_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?)`, ref.FolderID, assetID, nullable(ref.UserID), ts, req.DeviceLabel); err != nil {
			return meta.CommitResult{}, fmt.Errorf("link asset: %w", err)
		}
		if err := event(meta.EventAssetAdded, assetID, req.DeviceLabel); err != nil {
			return meta.CommitResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO upload_receipts (user_id, device_id, folder_id, idempotency_key, sha256, bytes, result, asset_id,
		created_at, paired_sha256) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ref.UserID, ref.DeviceID, ref.FolderID, b.Key, still.SHA256, still.Bytes, result, assetID, ts, video.SHA256); err != nil {
		return meta.CommitResult{}, fmt.Errorf("insert receipt: %w", err)
	}
	if err := completeBundle(); err != nil {
		return meta.CommitResult{}, err
	}
	asset, err := assetByID(ctx, tx, assetID)
	if err != nil {
		return meta.CommitResult{}, err
	}
	if req.BeforeCommit != nil {
		if err := req.BeforeCommit(); err != nil {
			return meta.CommitResult{}, fmt.Errorf("before commit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return meta.CommitResult{}, err
	}
	return meta.CommitResult{Result: result, Asset: asset}, nil
}
