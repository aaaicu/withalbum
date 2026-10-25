package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Likes, comments and mentions (meta.Social, migration 0014).
//
// Writes go through changeTx like the other album writes: the caller is
// re-checked under the write lock, the asset must still be in the folder,
// and the event and the audit row commit with the change. Reads are plain
// queries. Names are resolved at read time with readerCond, so people who
// lost access to the folder disappear from what others see without any
// write.

var _ meta.Social = (*Store)(nil)

// readerCond is true when account U can log in and can read folder F now:
// its owner, or a CONTRIBUTOR or VIEWER member (authz.ListAlbumAssets;
// TestReaderCondMatchesAuthz pins the two together). F and U are SQL
// expressions.
func readerCond(f, u string) string {
	return `EXISTS(SELECT 1 FROM users ru WHERE ru.user_id = ` + u + ` AND ru.status = 'active' AND substr(ru.password_hash, 1, 1) <> '` + password.LockedPrefix + `'
		AND (EXISTS(SELECT 1 FROM folders rf WHERE rf.folder_id = ` + f + ` AND rf.owner_user_id = ` + u + `)
			OR EXISTS(SELECT 1 FROM folder_members rm WHERE rm.folder_id = ` + f + ` AND rm.user_id = ` + u + ` AND rm.role IN ('CONTRIBUTOR','VIEWER'))))`
}

// readerName selects the display name of account U while it can read
// folder F (readerCond), else NULL.
func readerName(f, u string) string {
	return `(SELECT nu.display_name FROM users nu WHERE nu.user_id = ` + u + ` AND ` + readerCond(f, u) + `)`
}

// assetInFolder returns meta.ErrAssetNotFound unless the asset is linked to
// the folder and not removed.
func assetInFolder(ctx context.Context, q queryer, folderID, assetID string) error {
	var ok bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM folder_assets WHERE folder_id = ? AND asset_id = ? AND removed_at IS NULL)",
		folderID, assetID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return meta.ErrAssetNotFound
	}
	return nil
}

// appendSocialEvent appends one social event to the folder's feed.
func appendSocialEvent(ctx context.Context, tx *sql.Tx, folderID, kind, assetID, commentID, ts string) (int64, error) {
	seq, err := nextSeq(ctx, tx, folderID)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO folder_changes (folder_id, seq, event_type, asset_id, occurred_at, uploader_device_label, comment_id)
		VALUES (?, ?, ?, ?, ?, NULL, ?)`, folderID, seq, kind, assetID, ts, nullable(commentID))
	return seq, err
}

// summaryQuery selects (like_count, liked_by_me, comment_count) of one asset
// for one reader; arguments: folder, asset, user.
const summaryQuery = `SELECT
	(SELECT count(*) FROM asset_likes l WHERE l.folder_id = ?1 AND l.asset_id = ?2 AND ` + "%[1]s" + `),
	EXISTS(SELECT 1 FROM asset_likes l WHERE l.folder_id = ?1 AND l.asset_id = ?2 AND l.user_id = ?3),
	(SELECT count(*) FROM asset_comments c WHERE c.folder_id = ?1 AND c.asset_id = ?2)`

func summary(ctx context.Context, q queryer, folderID, assetID, userID string) (meta.SocialSummary, error) {
	var s meta.SocialSummary
	err := q.QueryRowContext(ctx, fmt.Sprintf(summaryQuery, readerCond("l.folder_id", "l.user_id")), folderID, assetID, userID).
		Scan(&s.LikeCount, &s.LikedByMe, &s.CommentCount)
	return s, err
}

// SetLike implements meta.Social.
func (s *Store) SetLike(ctx context.Context, c meta.AlbumChange, liked bool) (meta.SocialSummary, bool, error) {
	var sum meta.SocialSummary
	var changed bool
	err := s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if err := assetInFolder(ctx, tx, c.FolderID, c.TargetID); err != nil {
			return err
		}
		ts := now()
		var res sql.Result
		var err error
		if liked {
			res, err = tx.ExecContext(ctx, "INSERT INTO asset_likes (folder_id, asset_id, user_id, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING",
				c.FolderID, c.TargetID, c.ActorID, ts)
		} else {
			res, err = tx.ExecContext(ctx, "DELETE FROM asset_likes WHERE folder_id = ? AND asset_id = ? AND user_id = ?", c.FolderID, c.TargetID, c.ActorID)
		}
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if changed = n == 1; changed {
			seq, err := appendSocialEvent(ctx, tx, c.FolderID, meta.EventLikesUpdated, c.TargetID, "", ts)
			if err != nil {
				return err
			}
			action := "asset.like"
			if !liked {
				action = "asset.unlike"
			}
			if err := recordAlbum(ctx, tx, c.ActorID, action, "asset", c.TargetID, map[string]any{"folder_id": c.FolderID, "seq": seq}); err != nil {
				return err
			}
		}
		sum, err = summary(ctx, tx, c.FolderID, c.TargetID, c.ActorID)
		return err
	})
	return sum, changed, err
}

// ListLikes implements meta.Social.
func (s *Store) ListLikes(ctx context.Context, folderID, assetID string) ([]meta.Like, error) {
	if err := assetInFolder(ctx, s.db, folderID, assetID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.user_id, u.display_name, l.created_at FROM asset_likes l JOIN users u ON u.user_id = l.user_id
		WHERE l.folder_id = ? AND l.asset_id = ? AND `+readerCond("l.folder_id", "l.user_id")+` ORDER BY l.created_at, l.user_id`, folderID, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []meta.Like{}
	for rows.Next() {
		var l meta.Like
		if err := rows.Scan(&l.UserID, &l.DisplayName, &l.LikedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// maxSummaryAssets bounds one SocialSummaries query (sync pages hold at
// most 500 events).
const maxSummaryAssets = 500

// SocialSummaries implements meta.Social.
func (s *Store) SocialSummaries(ctx context.Context, folderID, userID string, assetIDs []string) (map[string]meta.SocialSummary, error) {
	out := map[string]meta.SocialSummary{}
	for len(assetIDs) > 0 {
		chunk := assetIDs[:min(len(assetIDs), maxSummaryAssets)]
		assetIDs = assetIDs[len(chunk):]
		args := []any{folderID, userID}
		for _, id := range chunk {
			args = append(args, id)
		}
		// Numbered parameters: both IN lists name the same ?3...
		marks := make([]string, len(chunk))
		for i := range chunk {
			marks[i] = fmt.Sprintf("?%d", i+3)
		}
		in := strings.Join(marks, ",")
		rows, err := s.db.QueryContext(ctx, `SELECT asset_id, sum(likes), max(mine), sum(comments) FROM (
				SELECT l.asset_id, `+readerCond("l.folder_id", "l.user_id")+` AS likes, l.user_id = ?2 AS mine, 0 AS comments
					FROM asset_likes l WHERE l.folder_id = ?1 AND l.asset_id IN (`+in+`)
				UNION ALL
				SELECT c.asset_id, 0, 0, 1 FROM asset_comments c WHERE c.folder_id = ?1 AND c.asset_id IN (`+in+`))
			GROUP BY asset_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var sum meta.SocialSummary
			if err := rows.Scan(&id, &sum.LikeCount, &sum.LikedByMe, &sum.CommentCount); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = sum
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// commentQuery selects a comment (scanComment) of asset_comments c; it is
// completed by a WHERE clause.
var commentQuery = `SELECT c.comment_id, c.folder_id, c.asset_id, c.author_user_id, COALESCE(` + readerName("c.folder_id", "c.author_user_id") + `, ''),
		c.body, c.created_at, COALESCE(c.edited_at, ''),
		(SELECT json_group_array(json_array(m.user_id, m.display_name, COALESCE(` + readerName("c.folder_id", "m.user_id") + `, ''),
			` + readerCond("c.folder_id", "m.user_id") + `)) FROM comment_mentions m WHERE m.comment_id = c.comment_id)
	FROM asset_comments c WHERE `

func scanComment(row scanner) (meta.Comment, error) {
	var cm meta.Comment
	var mentions string
	if err := row.Scan(&cm.ID, &cm.FolderID, &cm.AssetID, &cm.AuthorID, &cm.AuthorName, &cm.Body, &cm.CreatedAt, &cm.EditedAt, &mentions); err != nil {
		return meta.Comment{}, err
	}
	var raw [][]any
	if err := json.Unmarshal([]byte(mentions), &raw); err != nil {
		return meta.Comment{}, fmt.Errorf("comment %s mentions: %w", cm.ID, err)
	}
	for _, r := range raw {
		if len(r) != 4 {
			return meta.Comment{}, fmt.Errorf("comment %s: malformed mention", cm.ID)
		}
		m := meta.Mention{}
		m.UserID, _ = r[0].(string)
		m.WrittenName, _ = r[1].(string)
		m.Name, _ = r[2].(string)
		reader, _ := r[3].(float64)
		m.Reader = reader == 1
		cm.Mentions = append(cm.Mentions, m)
	}
	slices.SortFunc(cm.Mentions, func(a, b meta.Mention) int { return strings.Compare(a.UserID, b.UserID) })
	return cm, nil
}

func getComment(ctx context.Context, q queryer, folderID, assetID, commentID string) (meta.Comment, error) {
	cm, err := scanComment(q.QueryRowContext(ctx, commentQuery+"c.comment_id = ? AND c.folder_id = ? AND c.asset_id = ?", commentID, folderID, assetID))
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Comment{}, meta.ErrCommentNotFound
	}
	return cm, err
}

// mentionNames reads the display names of the mentioned accounts inside
// tx, requiring every one to be a reader of the folder.
func mentionNames(ctx context.Context, tx *sql.Tx, folderID string, ids []string) (map[string]string, error) {
	if len(ids) > meta.MaxMentions {
		return nil, errors.New("sqlitestore: too many mentions")
	}
	names := map[string]string{}
	for _, id := range ids {
		var name sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT "+readerName("?1", "?2"), folderID, id).Scan(&name); err != nil {
			return nil, err
		}
		if !name.Valid {
			return nil, meta.ErrMentionNotReader
		}
		names[id] = name.String
	}
	return names, nil
}

// writeMentions replaces the mentions of a comment.
func writeMentions(ctx context.Context, tx *sql.Tx, commentID string, names map[string]string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM comment_mentions WHERE comment_id = ?", commentID); err != nil {
		return err
	}
	for id, name := range names {
		if _, err := tx.ExecContext(ctx, "INSERT INTO comment_mentions (comment_id, user_id, display_name) VALUES (?, ?, ?)", commentID, id, name); err != nil {
			return err
		}
	}
	return nil
}

func checkCommentChange(c meta.CommentChange, needAuthor bool) error {
	if needAuthor && c.AuthorizeComment == nil {
		return errors.New("sqlitestore: CommentChange.AuthorizeComment is required")
	}
	if len(c.Body) > meta.MaxCommentBytes {
		return errors.New("sqlitestore: comment too long")
	}
	return nil
}

// prepareMentions validates the mentions of c inside tx.
func prepareMentions(ctx context.Context, tx *sql.Tx, c meta.CommentChange) (map[string]string, error) {
	names, err := mentionNames(ctx, tx, c.FolderID, c.Mentions)
	if err != nil {
		return nil, err
	}
	if c.CheckMentions != nil {
		if err := c.CheckMentions(names); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// CreateComment implements meta.Social.
func (s *Store) CreateComment(ctx context.Context, c meta.CommentChange) (meta.Comment, bool, error) {
	if err := checkCommentChange(c, false); err != nil {
		return meta.Comment{}, false, err
	}
	var cm meta.Comment
	var replayed bool
	err := s.changeTx(ctx, c.AlbumChange, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if err := assetInFolder(ctx, tx, c.FolderID, c.TargetID); err != nil {
			return err
		}
		if c.Key != "" {
			var id, folder, asset string
			err := tx.QueryRowContext(ctx, "SELECT comment_id, folder_id, asset_id FROM asset_comments WHERE author_user_id = ? AND client_key = ?",
				c.ActorID, c.Key).Scan(&id, &folder, &asset)
			if err == nil {
				if folder != c.FolderID || asset != c.TargetID {
					return meta.ErrIdempotencyMismatch
				}
				replayed = true
				cm, err = getComment(ctx, tx, c.FolderID, c.TargetID, id)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		names, err := prepareMentions(ctx, tx, c)
		if err != nil {
			return err
		}
		id, err := newID()
		if err != nil {
			return err
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_comments (comment_id, folder_id, asset_id, author_user_id, body, client_key, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, c.FolderID, c.TargetID, c.ActorID, c.Body, nullable(c.Key), ts); err != nil {
			return err
		}
		if err := writeMentions(ctx, tx, id, names); err != nil {
			return err
		}
		seq, err := appendSocialEvent(ctx, tx, c.FolderID, meta.EventCommentAdded, c.TargetID, id, ts)
		if err != nil {
			return err
		}
		if err := recordAlbum(ctx, tx, c.ActorID, "comment.create", "comment", id,
			map[string]any{"folder_id": c.FolderID, "asset_id": c.TargetID, "mentions": len(names), "seq": seq}); err != nil {
			return err
		}
		cm, err = getComment(ctx, tx, c.FolderID, c.TargetID, id)
		return err
	})
	return cm, replayed, err
}

// EditComment implements meta.Social.
func (s *Store) EditComment(ctx context.Context, c meta.CommentChange) (meta.Comment, error) {
	if err := checkCommentChange(c, true); err != nil {
		return meta.Comment{}, err
	}
	var cm meta.Comment
	err := s.changeTx(ctx, c.AlbumChange, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if err := assetInFolder(ctx, tx, c.FolderID, c.TargetID); err != nil {
			return err
		}
		old, err := getComment(ctx, tx, c.FolderID, c.TargetID, c.CommentID)
		if err != nil {
			return err
		}
		if err := c.AuthorizeComment(old.AuthorID); err != nil {
			return fmt.Errorf("authorize: %w", err)
		}
		names, err := prepareMentions(ctx, tx, c)
		if err != nil {
			return err
		}
		if old.Body == c.Body && sameMentions(old.Mentions, names) {
			cm = old
			return nil
		}
		ts := now()
		if _, err := tx.ExecContext(ctx, "UPDATE asset_comments SET body = ?, edited_at = ? WHERE comment_id = ?", c.Body, ts, c.CommentID); err != nil {
			return err
		}
		if err := writeMentions(ctx, tx, c.CommentID, names); err != nil {
			return err
		}
		seq, err := appendSocialEvent(ctx, tx, c.FolderID, meta.EventCommentUpdated, c.TargetID, c.CommentID, ts)
		if err != nil {
			return err
		}
		if err := recordAlbum(ctx, tx, c.ActorID, "comment.edit", "comment", c.CommentID,
			map[string]any{"folder_id": c.FolderID, "asset_id": c.TargetID, "mentions": len(names), "seq": seq}); err != nil {
			return err
		}
		cm, err = getComment(ctx, tx, c.FolderID, c.TargetID, c.CommentID)
		return err
	})
	return cm, err
}

// sameMentions reports whether the stored mentions are exactly the accounts
// of names with the same written names.
func sameMentions(old []meta.Mention, names map[string]string) bool {
	if len(old) != len(names) {
		return false
	}
	for _, m := range old {
		if n, ok := names[m.UserID]; !ok || n != m.WrittenName {
			return false
		}
	}
	return true
}

// DeleteComment implements meta.Social.
func (s *Store) DeleteComment(ctx context.Context, c meta.CommentChange) error {
	if err := checkCommentChange(c, true); err != nil {
		return err
	}
	return s.changeTx(ctx, c.AlbumChange, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if err := assetInFolder(ctx, tx, c.FolderID, c.TargetID); err != nil {
			return err
		}
		old, err := getComment(ctx, tx, c.FolderID, c.TargetID, c.CommentID)
		if err != nil {
			return err
		}
		if err := c.AuthorizeComment(old.AuthorID); err != nil {
			return fmt.Errorf("authorize: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM asset_comments WHERE comment_id = ?", c.CommentID); err != nil {
			return err
		}
		seq, err := appendSocialEvent(ctx, tx, c.FolderID, meta.EventCommentRemoved, c.TargetID, c.CommentID, now())
		if err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "comment.delete", "comment", c.CommentID,
			map[string]any{"folder_id": c.FolderID, "asset_id": c.TargetID, "author_id": old.AuthorID, "by_author": old.AuthorID == c.ActorID, "seq": seq})
	})
}

// ListComments implements meta.Social.
func (s *Store) ListComments(ctx context.Context, folderID, assetID string, after meta.SocialCursor, limit int) (meta.CommentPage, error) {
	if limit <= 0 {
		return meta.CommentPage{}, meta.ErrInvalidLimit
	}
	if err := assetInFolder(ctx, s.db, folderID, assetID); err != nil {
		return meta.CommentPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, commentQuery+`c.folder_id = ? AND c.asset_id = ? AND (c.created_at, c.comment_id) > (?, ?)
		ORDER BY c.created_at, c.comment_id LIMIT ?`, folderID, assetID, after.At, after.ID, limit+1)
	if err != nil {
		return meta.CommentPage{}, err
	}
	defer rows.Close()
	page := meta.CommentPage{Comments: []meta.Comment{}}
	for rows.Next() {
		cm, err := scanComment(rows)
		if err != nil {
			return meta.CommentPage{}, err
		}
		if len(page.Comments) == limit {
			page.HasMore = true
			break
		}
		page.Comments = append(page.Comments, cm)
		page.Next = meta.SocialCursor{At: cm.CreatedAt, ID: cm.ID}
	}
	return page, rows.Err()
}

// Mentionable implements meta.Social.
func (s *Store) Mentionable(ctx context.Context, folderID string) ([]meta.Member, error) {
	all, err := s.ListMembers(ctx, folderID)
	if err != nil {
		return nil, err
	}
	out := []meta.Member{}
	for _, m := range all {
		if m.Active && (m.Role == "OWNER" || m.Role == string(authz.Contributor) || m.Role == string(authz.Viewer)) {
			out = append(out, m)
		}
	}
	return out, nil
}

// activityQuery lists the activity of user ?1 newest first before (?2, ?3)
// (no bound when ?2 is ""), limit ?4. The folders are those ?1 can read now
// (not archived), the assets those still in them.
var activityQuery = `WITH mine AS (
		SELECT f.folder_id, f.folder_name FROM folders f
		WHERE f.archived_at IS NULL AND ` + readerCond("f.folder_id", "?1") + `
	), items AS (
		SELECT c.created_at AS at, 'c' || c.comment_id AS id,
			CASE WHEN EXISTS(SELECT 1 FROM comment_mentions cm WHERE cm.comment_id = c.comment_id AND cm.user_id = ?1) THEN 'mention' ELSE 'comment' END AS kind,
			c.folder_id AS folder_id, mine.folder_name AS folder_name, c.asset_id AS asset_id, c.author_user_id AS actor,
			COALESCE(` + readerName("c.folder_id", "c.author_user_id") + `, '') AS actor_name, c.comment_id AS comment_id
		FROM asset_comments c JOIN mine ON mine.folder_id = c.folder_id
		JOIN folder_assets fa ON fa.folder_id = c.folder_id AND fa.asset_id = c.asset_id AND fa.removed_at IS NULL
		JOIN assets a ON a.asset_id = c.asset_id
		WHERE c.author_user_id <> ?1 AND (a.uploaded_by_user_id = ?1
			OR EXISTS(SELECT 1 FROM comment_mentions cm WHERE cm.comment_id = c.comment_id AND cm.user_id = ?1))
		UNION ALL
		SELECT l.created_at, 'l' || l.folder_id || l.asset_id || l.user_id, 'like', l.folder_id, mine.folder_name, l.asset_id, l.user_id,
			COALESCE(` + readerName("l.folder_id", "l.user_id") + `, ''), ''
		FROM asset_likes l JOIN mine ON mine.folder_id = l.folder_id
		JOIN folder_assets fa ON fa.folder_id = l.folder_id AND fa.asset_id = l.asset_id AND fa.removed_at IS NULL
		JOIN assets a ON a.asset_id = l.asset_id
		WHERE a.uploaded_by_user_id = ?1 AND l.user_id <> ?1 AND ` + readerCond("l.folder_id", "l.user_id") + `
	)
	SELECT at, id, kind, folder_id, folder_name, asset_id, actor, actor_name, comment_id FROM items
	WHERE ?2 = '' OR (at, id) < (?2, ?3) ORDER BY at DESC, id DESC LIMIT ?4`

// ListActivity implements meta.Social.
func (s *Store) ListActivity(ctx context.Context, userID string, before meta.SocialCursor, limit int) (meta.ActivityPage, error) {
	if limit <= 0 {
		return meta.ActivityPage{}, meta.ErrInvalidLimit
	}
	rows, err := s.db.QueryContext(ctx, activityQuery, userID, before.At, before.ID, limit+1)
	if err != nil {
		return meta.ActivityPage{}, err
	}
	page := meta.ActivityPage{Items: []meta.Activity{}}
	var commentIDs []string
	for rows.Next() {
		var a meta.Activity
		var commentID string
		if err := rows.Scan(&a.At, &a.ID, &a.Kind, &a.FolderID, &a.FolderName, &a.AssetID, &a.ActorID, &a.ActorName, &commentID); err != nil {
			rows.Close()
			return meta.ActivityPage{}, err
		}
		if len(page.Items) == limit {
			page.HasMore = true
			break
		}
		if commentID != "" {
			a.Comment = &meta.Comment{ID: commentID}
			commentIDs = append(commentIDs, commentID)
		}
		page.Items = append(page.Items, a)
		page.Next = meta.SocialCursor{At: a.At, ID: a.ID}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return meta.ActivityPage{}, err
	}
	rows.Close()
	if len(commentIDs) == 0 {
		return page, nil
	}
	args := make([]any, len(commentIDs))
	for i, id := range commentIDs {
		args[i] = id
	}
	crows, err := s.db.QueryContext(ctx, commentQuery+"c.comment_id IN (?"+strings.Repeat(",?", len(commentIDs)-1)+")", args...)
	if err != nil {
		return meta.ActivityPage{}, err
	}
	defer crows.Close()
	byID := map[string]meta.Comment{}
	for crows.Next() {
		cm, err := scanComment(crows)
		if err != nil {
			return meta.ActivityPage{}, err
		}
		byID[cm.ID] = cm
	}
	if err := crows.Err(); err != nil {
		return meta.ActivityPage{}, err
	}
	for i := range page.Items {
		if c := page.Items[i].Comment; c != nil {
			// A comment deleted between the two queries is shown without
			// its text rather than skipped (the page stays in order).
			if cm, ok := byID[c.ID]; ok {
				page.Items[i].Comment = &cm
			}
		}
	}
	return page, nil
}

// dropLikes deletes userID's likes in the folder (the account lost read
// access) and appends a likes_updated event for each affected asset still
// in the folder. It returns the number of likes deleted.
func dropLikes(ctx context.Context, tx *sql.Tx, folderID, userID string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.asset_id, EXISTS(SELECT 1 FROM folder_assets fa WHERE fa.folder_id = l.folder_id AND fa.asset_id = l.asset_id AND fa.removed_at IS NULL)
		FROM asset_likes l WHERE l.folder_id = ? AND l.user_id = ? ORDER BY l.asset_id`, folderID, userID)
	if err != nil {
		return 0, err
	}
	var linked []string
	n := 0
	for rows.Next() {
		var id string
		var in bool
		if err := rows.Scan(&id, &in); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		if in {
			linked = append(linked, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if n == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM asset_likes WHERE folder_id = ? AND user_id = ?", folderID, userID); err != nil {
		return 0, err
	}
	ts := now()
	for _, id := range linked {
		if _, err := appendSocialEvent(ctx, tx, folderID, meta.EventLikesUpdated, id, "", ts); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// dropAssetSocial deletes the likes and comments (with their mentions) of an
// asset that leaves the folder. The asset_removed event that follows tells
// clients to drop them; no social events are written.
func dropAssetSocial(ctx context.Context, tx *sql.Tx, folderID, assetID string) (likes, comments int64, err error) {
	res, err := tx.ExecContext(ctx, "DELETE FROM asset_likes WHERE folder_id = ? AND asset_id = ?", folderID, assetID)
	if err != nil {
		return 0, 0, err
	}
	if likes, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	res, err = tx.ExecContext(ctx, "DELETE FROM asset_comments WHERE folder_id = ? AND asset_id = ?", folderID, assetID)
	if err != nil {
		return 0, 0, err
	}
	comments, err = res.RowsAffected()
	return likes, comments, err
}
