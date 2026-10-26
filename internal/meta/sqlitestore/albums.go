package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Deleted albums, the album trash and hidden assets (meta.AlbumManagement,
// migration 0017).
//
// Deleting an album only sets folders.deleted_at; GetFolderAccess and every
// list then leave it out, so every route answers it like an unknown
// folder. Its owner may restore it for meta.TrashRetention. The purge
// deletes the folder row (its members, invitations, feed, links, likes,
// comments, receipts and upload sessions go with it by ON DELETE CASCADE),
// then the assets no other folder links or references, then the blob rows
// no remaining asset holds; the pool files of those blobs are moved out of
// the pool by the caller's DetachFunc inside the same transaction, so a
// crash leaves either the rows and the files or neither (the caller's
// startup recovery puts back files whose rows survived).

var _ meta.AlbumManagement = (*Store)(nil)

// cutoff is the oldest removal or deletion time that can still be
// restored at now, in the fixed-width format of now().
func cutoff(t time.Time) string { return t.Add(-meta.TrashRetention).UTC().Format(timeLayout) }

// purgeAfter is when something removed or deleted at ts is purged.
func purgeAfter(ts string) string {
	t, err := time.Parse(timeLayout, ts)
	if err != nil {
		return ""
	}
	return t.Add(meta.TrashRetention).UTC().Format(timeLayout)
}

// DeleteFolder implements meta.AlbumManagement.
func (s *Store) DeleteFolder(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		res, err := tx.ExecContext(ctx, "UPDATE folders SET deleted_at = ? WHERE folder_id = ? AND deleted_at IS NULL AND archived_at IS NULL", now(), c.FolderID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return meta.ErrFolderNotFound
		}
		n, err := revokePending(ctx, tx, c.FolderID)
		if err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "folder.delete", "folder", c.FolderID,
			map[string]any{"folder_name": fa.Folder.Name, "invitations_revoked": n})
	})
}

// RestoreFolder implements meta.AlbumManagement.
func (s *Store) RestoreFolder(ctx context.Context, c meta.AlbumChange) (meta.FolderAccess, error) {
	if c.Authorize == nil {
		return meta.FolderAccess{}, errors.New("sqlitestore: AlbumChange.Authorize is required")
	}
	fa, err := s.restoreFolder(ctx, c)
	return fa, classify(err)
}

func (s *Store) restoreFolder(ctx context.Context, c meta.AlbumChange) (meta.FolderAccess, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	defer tx.Rollback()
	var owner string
	var deleted, archived sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT owner_user_id, deleted_at, archived_at FROM folders WHERE folder_id = ?", c.FolderID).Scan(&owner, &deleted, &archived)
	if errors.Is(err, sql.ErrNoRows) || err == nil && archived.Valid {
		return meta.FolderAccess{}, meta.ErrFolderNotFound
	}
	if err != nil {
		return meta.FolderAccess{}, err
	}
	p, err := principalTx(ctx, tx, c.ActorID, c.SessionID)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	access := authz.AlbumAccess{OwnerID: owner}
	if role, err := memberRole(ctx, tx, c.FolderID, c.ActorID); err == nil {
		access.HasMembership, access.MemberRole = true, role
	} else if !errors.Is(err, meta.ErrMemberNotFound) {
		return meta.FolderAccess{}, err
	}
	if err := c.Authorize(p, access); err != nil {
		return meta.FolderAccess{}, fmt.Errorf("authorize: %w", err)
	}
	if owner != c.ActorID {
		return meta.FolderAccess{}, meta.ErrFolderNotFound
	}
	if !deleted.Valid {
		return meta.FolderAccess{}, meta.ErrFolderNotDeleted
	}
	if deleted.String <= cutoff(time.Now()) {
		return meta.FolderAccess{}, meta.ErrFolderNotFound // due for purging
	}
	res, err := tx.ExecContext(ctx, "UPDATE folders SET deleted_at = NULL WHERE folder_id = ? AND deleted_at = ?", c.FolderID, deleted.String)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return meta.FolderAccess{}, fmt.Errorf("restore folder: %d rows changed: %v", n, err)
	}
	if err := recordAlbum(ctx, tx, c.ActorID, "folder.restore", "folder", c.FolderID, map[string]any{"deleted_at": deleted.String}); err != nil {
		return meta.FolderAccess{}, err
	}
	fa, err := getFolderAccess(ctx, tx, c.FolderID, c.ActorID)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	if c.BeforeCommit != nil {
		if err := c.BeforeCommit(); err != nil {
			return meta.FolderAccess{}, fmt.Errorf("before commit: %w", err)
		}
	}
	return fa, tx.Commit()
}

// ListDeletedFolders implements meta.AlbumManagement.
func (s *Store) ListDeletedFolders(ctx context.Context, ownerID string, t time.Time) ([]meta.DeletedFolder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT folder_id, folder_name, created_at, deleted_at FROM folders
		WHERE owner_user_id = ? AND deleted_at IS NOT NULL AND archived_at IS NULL AND deleted_at > ?
		ORDER BY deleted_at DESC, folder_id`, ownerID, cutoff(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []meta.DeletedFolder{}
	for rows.Next() {
		var f meta.DeletedFolder
		if err := rows.Scan(&f.ID, &f.Name, &f.CreatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		f.PurgeAfter = purgeAfter(f.DeletedAt)
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeletedFoldersDue implements meta.AlbumManagement.
func (s *Store) DeletedFoldersDue(ctx context.Context, before time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, meta.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, "SELECT folder_id FROM folders WHERE deleted_at IS NOT NULL AND deleted_at < ? ORDER BY deleted_at, folder_id LIMIT ?",
		before.UTC().Format(timeLayout), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeletedFolderCount implements meta.AlbumManagement.
func (s *Store) DeletedFolderCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE deleted_at IS NOT NULL").Scan(&n)
	return n, err
}

// queryStrings collects the single text column of a query.
func queryStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PurgeDeletedFolder implements meta.AlbumManagement.
func (s *Store) PurgeDeletedFolder(ctx context.Context, folderID string, before time.Time, detach meta.DetachFunc) (meta.PurgedFolder, error) {
	if detach == nil {
		return meta.PurgedFolder{}, errors.New("sqlitestore: PurgeDeletedFolder needs a DetachFunc")
	}
	p, undo, err := s.purgeDeletedFolder(ctx, folderID, before, detach)
	if err != nil && undo != nil {
		undo() // the commit failed: the rows are still there, so are the files
	}
	return p, classify(err)
}

func (s *Store) purgeDeletedFolder(ctx context.Context, folderID string, before time.Time, detach meta.DetachFunc) (meta.PurgedFolder, func(), error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.PurgedFolder{}, nil, err
	}
	defer tx.Rollback()
	var deleted sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT deleted_at FROM folders WHERE folder_id = ?", folderID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!deleted.Valid || deleted.String >= before.UTC().Format(timeLayout)) {
		return meta.PurgedFolder{}, nil, meta.ErrFolderNotFound
	}
	if err != nil {
		return meta.PurgedFolder{}, nil, err
	}
	out := meta.PurgedFolder{FolderID: folderID}
	assets, err := queryStrings(ctx, tx, "SELECT asset_id FROM folder_assets WHERE folder_id = ? ORDER BY asset_id", folderID)
	if err != nil {
		return meta.PurgedFolder{}, nil, err
	}
	if out.Uploads, err = queryStrings(ctx, tx, "SELECT upload_id FROM upload_sessions WHERE folder_id = ? ORDER BY upload_id", folderID); err != nil {
		return meta.PurgedFolder{}, nil, err
	}
	// Everything keyed by the folder goes with it (ON DELETE CASCADE).
	if _, err := tx.ExecContext(ctx, "DELETE FROM folders WHERE folder_id = ?", folderID); err != nil {
		return meta.PurgedFolder{}, nil, fmt.Errorf("delete folder: %w", err)
	}
	// An asset goes when nothing else refers to it: no link and no event of
	// another folder (PoC imports linked one asset to several folders), no
	// upload receipt.
	candidates := map[string]bool{}
	for _, id := range assets {
		var sha string
		var keep bool
		if err := tx.QueryRowContext(ctx, `SELECT a.original_sha256,
				EXISTS(SELECT 1 FROM folder_assets fa WHERE fa.asset_id = a.asset_id)
				OR EXISTS(SELECT 1 FROM folder_changes c WHERE c.asset_id = a.asset_id)
				OR EXISTS(SELECT 1 FROM upload_receipts r WHERE r.asset_id = a.asset_id)
			FROM assets a WHERE a.asset_id = ?`, id).Scan(&sha, &keep); err != nil {
			return meta.PurgedFolder{}, nil, fmt.Errorf("asset %s: %w", id, err)
		}
		if keep {
			continue
		}
		components, err := queryStrings(ctx, tx, "SELECT sha256 FROM asset_components WHERE asset_id = ?", id)
		if err != nil {
			return meta.PurgedFolder{}, nil, err
		}
		for _, q := range []string{"DELETE FROM asset_components WHERE asset_id = ?", "DELETE FROM legacy_asset_map WHERE asset_id = ?", "DELETE FROM assets WHERE asset_id = ?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return meta.PurgedFolder{}, nil, fmt.Errorf("asset %s: %w", id, err)
			}
		}
		out.Assets++
		candidates[sha] = true
		for _, c := range components {
			candidates[c] = true
		}
	}
	// An original goes when no remaining asset holds it.
	for sha := range candidates {
		var held bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE original_sha256 = ?1 OR preview_sha256 = ?1)
			OR EXISTS(SELECT 1 FROM asset_components WHERE sha256 = ?1)`, sha).Scan(&held); err != nil {
			return meta.PurgedFolder{}, nil, err
		}
		if held {
			continue
		}
		for _, q := range []string{"DELETE FROM blob_playback WHERE sha256 = ?", "DELETE FROM blobs WHERE sha256 = ?"} {
			if _, err := tx.ExecContext(ctx, q, sha); err != nil {
				return meta.PurgedFolder{}, nil, fmt.Errorf("blob %s: %w", sha, err)
			}
		}
		out.Blobs = append(out.Blobs, sha)
	}
	slices.Sort(out.Blobs)
	if err := recordAlbum(ctx, tx, "", "folder.purge", "folder", folderID, map[string]any{
		"deleted_at": deleted.String, "links": len(assets), "assets_deleted": out.Assets, "originals_deleted": len(out.Blobs)}); err != nil {
		return meta.PurgedFolder{}, nil, err
	}
	// The files leave the pool last, under the write lock: an upload of
	// the same bytes either committed before (and then holds the blob) or
	// finds no pool file before its commit and fails.
	undo, err := detach(out.Blobs)
	if err != nil {
		return meta.PurgedFolder{}, nil, fmt.Errorf("detach originals: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return meta.PurgedFolder{}, undo, err
	}
	return out, nil, nil
}

// SetAssetHidden implements meta.AlbumManagement.
func (s *Store) SetAssetHidden(ctx context.Context, c meta.AlbumChange, hidden bool) (meta.Asset, bool, error) {
	var a meta.Asset
	var changed bool
	err := s.changeTxView(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess, view meta.View) error {
		var hiddenAt, label sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT hidden_at, uploader_device_label FROM folder_assets WHERE folder_id = ? AND asset_id = ? AND removed_at IS NULL",
			c.FolderID, c.TargetID).Scan(&hiddenAt, &label)
		if errors.Is(err, sql.ErrNoRows) || err == nil && view != meta.ViewAll && hiddenAt.Valid {
			return meta.ErrAssetNotFound
		}
		if err != nil {
			return err
		}
		if changed = hiddenAt.Valid != hidden; changed {
			ts := now()
			kind, action := eventAssetUnhidden, "asset.unhide"
			var at, by any
			if hidden {
				kind, action, at, by = eventAssetHidden, "asset.hide", ts, c.ActorID
			}
			if _, err := tx.ExecContext(ctx, "UPDATE folder_assets SET hidden_at = ?, hidden_by_user_id = ? WHERE folder_id = ? AND asset_id = ?",
				at, by, c.FolderID, c.TargetID); err != nil {
				return err
			}
			seq, err := nextSeq(ctx, tx, c.FolderID)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
				VALUES (?, ?, ?, ?, ?, ?)`, c.FolderID, seq, kind, c.TargetID, ts, label); err != nil {
				return err
			}
			if err := recordAlbum(ctx, tx, c.ActorID, action, "asset", c.TargetID, map[string]any{"folder_id": c.FolderID, "seq": seq}); err != nil {
				return err
			}
		}
		a, err = getFolderAsset(ctx, tx, c.FolderID, c.TargetID, meta.ViewAll)
		return err
	})
	return a, changed, err
}

// trashColumns selects a meta.TrashedAsset (scanTrashed) from folder_assets
// fa JOIN assets a JOIN blobs b.
const trashColumns = linkedAssetColumns + `, fa.removed_at,
	COALESCE((SELECT u.display_name FROM users u WHERE u.user_id = fa.removed_by_user_id AND ` + activeUser + `), '')`

// inTrash is the condition on folder_assets fa for a link in the trash
// that can still be restored; its argument is cutoff(now).
const inTrash = "fa.removed_at IS NOT NULL AND fa.purged_at IS NULL AND fa.removed_at > ?"

func scanTrashed(row scanner) (meta.TrashedAsset, error) {
	var t meta.TrashedAsset
	var hidden bool
	a, err := scanAssetWith(row, &hidden, &t.RemovedAt, &t.RemovedByName)
	if err != nil {
		return meta.TrashedAsset{}, err
	}
	a.Hidden = hidden
	t.Asset, t.PurgeAfter = a, purgeAfter(t.RemovedAt)
	return t, nil
}

// ListTrash implements meta.AlbumManagement.
func (s *Store) ListTrash(ctx context.Context, folderID string, t time.Time, before meta.SocialCursor, limit int) (meta.TrashPage, error) {
	if limit <= 0 {
		return meta.TrashPage{}, meta.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+trashColumns+` FROM folder_assets fa
		JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND `+inTrash+` AND (? = '' OR (fa.removed_at, fa.asset_id) < (?, ?))
		ORDER BY fa.removed_at DESC, fa.asset_id DESC LIMIT ?`, folderID, cutoff(t), before.At, before.At, before.ID, limit+1)
	if err != nil {
		return meta.TrashPage{}, err
	}
	defer rows.Close()
	page := meta.TrashPage{Assets: []meta.TrashedAsset{}}
	for rows.Next() {
		ta, err := scanTrashed(rows)
		if err != nil {
			return meta.TrashPage{}, err
		}
		if len(page.Assets) == limit {
			page.HasMore = true
			break
		}
		page.Assets = append(page.Assets, ta)
		page.Next = meta.SocialCursor{At: ta.RemovedAt, ID: ta.ID}
	}
	return page, rows.Err()
}

// GetTrashedAsset implements meta.AlbumManagement.
func (s *Store) GetTrashedAsset(ctx context.Context, folderID, assetID string, t time.Time) (meta.TrashedAsset, error) {
	ta, err := scanTrashed(s.db.QueryRowContext(ctx, "SELECT "+trashColumns+` FROM folder_assets fa
		JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND fa.asset_id = ? AND `+inTrash, folderID, assetID, cutoff(t)))
	if errors.Is(err, sql.ErrNoRows) {
		return meta.TrashedAsset{}, meta.ErrAssetNotFound
	}
	return ta, err
}

// RestoreAsset implements meta.AlbumManagement. The relink trigger of
// migration 0016 queues a video's playback copy again if it was released.
func (s *Store) RestoreAsset(ctx context.Context, c meta.AlbumChange) (meta.Asset, error) {
	var a meta.Asset
	err := s.changeTxView(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess, view meta.View) error {
		var sha, removedAt string
		var label sql.NullString
		var hidden bool
		err := tx.QueryRowContext(ctx, `SELECT a.original_sha256, fa.removed_at, fa.uploader_device_label, fa.hidden_at IS NOT NULL
			FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id
			WHERE fa.folder_id = ? AND fa.asset_id = ? AND `+inTrash, c.FolderID, c.TargetID, cutoff(time.Now())).Scan(&sha, &removedAt, &label, &hidden)
		if errors.Is(err, sql.ErrNoRows) || err == nil && hidden && view != meta.ViewAll {
			return meta.ErrAssetNotFound
		}
		if err != nil {
			return err
		}
		var conflict bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id
			WHERE fa.folder_id = ? AND fa.removed_at IS NULL AND a.original_sha256 = ?)`, c.FolderID, sha).Scan(&conflict); err != nil {
			return err
		}
		if conflict {
			return meta.ErrAssetConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE folder_assets SET removed_at = NULL, removed_by_user_id = NULL WHERE folder_id = ? AND asset_id = ?",
			c.FolderID, c.TargetID); err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx, c.FolderID)
		if err != nil {
			return err
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label)
			VALUES (?, ?, ?, ?, ?, ?)`, c.FolderID, seq, meta.EventAssetAdded, c.TargetID, ts, label); err != nil {
			return err
		}
		if err := recordAlbum(ctx, tx, c.ActorID, "asset.restore", "asset", c.TargetID,
			map[string]any{"folder_id": c.FolderID, "seq": seq, "removed_at": removedAt}); err != nil {
			return err
		}
		a, err = getFolderAsset(ctx, tx, c.FolderID, c.TargetID, meta.ViewAll)
		return err
	})
	return a, err
}

// purgeBatch bounds the removals PurgeTrash makes final in one transaction.
const purgeBatch = 200

// PurgeTrash implements meta.AlbumManagement.
func (s *Store) PurgeTrash(ctx context.Context, before time.Time) (int, error) {
	total := 0
	for {
		n, err := s.purgeTrashBatch(ctx, before.UTC().Format(timeLayout))
		total += n
		if err != nil || n < purgeBatch {
			return total, classify(err)
		}
	}
}

func (s *Store) purgeTrashBatch(ctx context.Context, before string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT folder_id, asset_id FROM folder_assets
		WHERE removed_at IS NOT NULL AND purged_at IS NULL AND removed_at < ? ORDER BY removed_at, folder_id, asset_id LIMIT ?`, before, purgeBatch)
	if err != nil {
		return 0, err
	}
	type link struct{ folder, asset string }
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.folder, &l.asset); err != nil {
			rows.Close()
			return 0, err
		}
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(links) == 0 {
		return 0, nil
	}
	ts := now()
	var likes, comments int64
	folders := map[string]bool{}
	for _, l := range links {
		nl, nc, err := dropAssetSocial(ctx, tx, l.folder, l.asset)
		if err != nil {
			return 0, err
		}
		likes, comments = likes+nl, comments+nc
		if _, err := tx.ExecContext(ctx, "UPDATE folder_assets SET purged_at = ? WHERE folder_id = ? AND asset_id = ?", ts, l.folder, l.asset); err != nil {
			return 0, err
		}
		folders[l.folder] = true
	}
	if err := recordAlbum(ctx, tx, "", "trash.purge", "server", "", map[string]any{
		"assets": len(links), "folders": len(folders), "likes_removed": likes, "comments_removed": comments}); err != nil {
		return 0, err
	}
	return len(links), tx.Commit()
}

// HasBlob reports whether the blobs table records sha (startup recovery of
// purged originals).
func (s *Store) HasBlob(ctx context.Context, sha string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256 = ?)", sha).Scan(&ok)
	return ok, err
}
