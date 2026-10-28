package sqlitestore

import (
	"context"
	"database/sql"

	"github.com/aaaicu/withalbum/internal/meta"
)

// FolderDigest implements meta.SyncDigests. The feed position and the
// assets are read in one transaction, so the digest is the folder as of
// that position. The like count, the caller's own like and the comment
// count are the ones SocialSummaries gives the asset JSON.
func (s *Store) FolderDigest(ctx context.Context, folderID, userID string, view meta.View) (meta.FolderDigest, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return meta.FolderDigest{}, err
	}
	defer tx.Rollback()
	st, err := s.feedState(ctx, tx, folderID, view)
	if err != nil {
		return meta.FolderDigest{}, err
	}
	// #nosec G202 -- visibleIn and readerCond are constant SQL; values are arguments
	rows, err := tx.QueryContext(ctx, `SELECT fa.asset_id, a.original_sha256,
			(SELECT count(*) FROM asset_likes l WHERE l.folder_id = fa.folder_id AND l.asset_id = fa.asset_id AND `+readerCond("l.folder_id", "l.user_id")+`),
			EXISTS(SELECT 1 FROM asset_likes l WHERE l.folder_id = fa.folder_id AND l.asset_id = fa.asset_id AND l.user_id = ?),
			(SELECT count(*) FROM asset_comments c WHERE c.folder_id = fa.folder_id AND c.asset_id = fa.asset_id)
		FROM folder_assets fa JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
		WHERE fa.folder_id = ? AND fa.removed_at IS NULL AND `+visibleIn(view)+` ORDER BY fa.asset_id`, userID, folderID)
	if err != nil {
		return meta.FolderDigest{}, err
	}
	defer rows.Close()
	b := meta.NewDigestBuilder()
	for rows.Next() {
		var id, sha string
		var likes, comments int
		var mine bool
		if err := rows.Scan(&id, &sha, &likes, &mine, &comments); err != nil {
			return meta.FolderDigest{}, err
		}
		b.Add(id, sha, likes, mine, comments)
	}
	if err := rows.Err(); err != nil {
		return meta.FolderDigest{}, err
	}
	return meta.FolderDigest{State: st, Count: b.Count(), Digest: b.Sum()}, nil
}
