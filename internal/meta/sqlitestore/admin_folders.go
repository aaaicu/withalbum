package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Administration of folder ownership for account management. An account
// that owns folders cannot be disabled (trigger cannot_disable_owner), and
// an archived folder has no owner who could still transfer it, so a server
// administrator may reassign archived folders, and only those. Nothing here
// reads names, members or contents of a folder.

// OwnedFolders implements meta.Memberships.
func (s *Store) OwnedFolders(ctx context.Context, userID string) (meta.OwnedFolders, error) {
	out := meta.OwnedFolders{Archived: []meta.ArchivedFolder{}}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM folders WHERE owner_user_id = ? AND archived_at IS NULL", userID).Scan(&out.Active); err != nil {
		return meta.OwnedFolders{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT folder_id, created_at, archived_at FROM folders
		WHERE owner_user_id = ? AND archived_at IS NOT NULL ORDER BY archived_at, folder_id`, userID)
	if err != nil {
		return meta.OwnedFolders{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var f meta.ArchivedFolder
		if err := rows.Scan(&f.ID, &f.CreatedAt, &f.ArchivedAt); err != nil {
			return meta.OwnedFolders{}, err
		}
		out.Archived = append(out.Archived, f)
	}
	return out, rows.Err()
}

// ReassignArchivedOwner implements meta.Memberships. One write transaction:
// the caller is re-authorized on the account and session as stored now, the
// folder must still be archived, and the change is audited
// (folder.reassign_archived_owner) before COMMIT.
func (s *Store) ReassignArchivedOwner(ctx context.Context, c meta.AlbumChange) (meta.Reassignment, error) {
	r, err := s.reassignArchivedOwner(ctx, c)
	return r, classify(err)
}

func (s *Store) reassignArchivedOwner(ctx context.Context, c meta.AlbumChange) (meta.Reassignment, error) {
	if c.Authorize == nil {
		return meta.Reassignment{}, errors.New("sqlitestore: AlbumChange.Authorize is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.Reassignment{}, err
	}
	defer tx.Rollback()
	p, err := principalTx(ctx, tx, c.ActorID, c.SessionID)
	if err != nil {
		return meta.Reassignment{}, err
	}
	if err := c.Authorize(p, authz.AlbumAccess{}); err != nil {
		return meta.Reassignment{}, fmt.Errorf("authorize: %w", err)
	}
	var prev string
	var archived sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT owner_user_id, archived_at FROM folders WHERE folder_id = ?", c.FolderID).Scan(&prev, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Reassignment{}, meta.ErrFolderNotFound
	}
	if err != nil {
		return meta.Reassignment{}, err
	}
	if !archived.Valid {
		return meta.Reassignment{}, meta.ErrFolderNotArchived
	}
	if c.TargetID == prev {
		return meta.Reassignment{}, meta.ErrTargetIsOwner
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE user_id = ?)", c.TargetID).Scan(&exists); err != nil {
		return meta.Reassignment{}, err
	}
	if !exists {
		return meta.Reassignment{}, meta.ErrNewOwnerNotFound
	}
	if ok, err := accountActive(ctx, tx, c.TargetID); err != nil {
		return meta.Reassignment{}, err
	} else if !ok {
		return meta.Reassignment{}, meta.ErrNewOwnerInactive
	}
	oldRole, err := memberRole(ctx, tx, c.FolderID, c.TargetID)
	if err != nil && !errors.Is(err, meta.ErrMemberNotFound) {
		return meta.Reassignment{}, err
	}
	// The owner is never a member (triggers no_owner_membership_*).
	if _, err := tx.ExecContext(ctx, "DELETE FROM folder_members WHERE folder_id = ? AND user_id = ?", c.FolderID, c.TargetID); err != nil {
		return meta.Reassignment{}, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE folders SET owner_user_id = ? WHERE folder_id = ? AND owner_user_id = ? AND archived_at IS NOT NULL",
		c.TargetID, c.FolderID, prev)
	if err != nil {
		return meta.Reassignment{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return meta.Reassignment{}, fmt.Errorf("reassign owner: %d rows changed: %v", n, err)
	}
	n, err := revokePending(ctx, tx, c.FolderID)
	if err != nil {
		return meta.Reassignment{}, err
	}
	if err := recordAlbum(ctx, tx, c.ActorID, "folder.reassign_archived_owner", "folder", c.FolderID, map[string]any{
		"previous_owner_id": prev, "new_owner_id": c.TargetID, "new_owner_previous_role": string(oldRole),
		"invitations_revoked": n}); err != nil {
		return meta.Reassignment{}, err
	}
	if c.BeforeCommit != nil {
		if err := c.BeforeCommit(); err != nil {
			return meta.Reassignment{}, fmt.Errorf("before commit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return meta.Reassignment{}, err
	}
	return meta.Reassignment{FolderID: c.FolderID, PreviousOwnerID: prev, NewOwnerID: c.TargetID, ArchivedAt: archived.String}, nil
}
