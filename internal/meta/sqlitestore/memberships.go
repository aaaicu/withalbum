package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aaaicu/withalbum/internal/audit"
	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Invitations, memberships, ownership transfer and archiving
// (meta.Memberships).
//
// Every write is one BEGIN IMMEDIATE transaction: it re-reads the caller's
// account and session and the folder's owner and memberships, lets the
// caller decide again (AlbumChange.Authorize), applies the change and writes
// its audit_log row. Accounts, sessions and memberships live in this same
// database, so nothing the decision depends on can change before COMMIT.
//
// The owner is folders.owner_user_id and never a folder_members row
// (triggers no_owner_membership_*). A transfer therefore deletes the new
// owner's membership first, then changes the owner, then inserts the
// previous owner as a member, so the folder has exactly one owner at every
// step and the triggers hold.

var _ meta.Memberships = (*Store)(nil)

// timeLayout is the fixed-width format of now().
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// expiredAt reports whether an expires_at value lies at or before t. A
// value that cannot be parsed counts as expired (fail closed).
func expiredAt(expires string, t time.Time) bool {
	e, err := time.Parse(timeLayout, expires)
	return err != nil || !t.Before(e)
}

func validRole(r authz.AlbumRole) bool {
	return r == authz.Contributor || r == authz.Viewer || r == authz.Uploader
}

// changeTx runs fn in one write transaction after re-authorizing the caller
// on folderID, then c.BeforeCommit, then COMMIT.
func (s *Store) changeTx(ctx context.Context, c meta.AlbumChange, folderID string, fn func(tx *sql.Tx, fa meta.FolderAccess) error) error {
	return s.changeTxView(ctx, c, folderID, func(tx *sql.Tx, fa meta.FolderAccess, _ meta.View) error { return fn(tx, fa) })
}

// changeTxView is changeTx that also passes fn the caller's view of the
// folder's assets (meta.ViewOf), decided on the account, session and
// relation as stored in the transaction.
func (s *Store) changeTxView(ctx context.Context, c meta.AlbumChange, folderID string, fn func(tx *sql.Tx, fa meta.FolderAccess, view meta.View) error) error {
	if c.Authorize == nil {
		return errors.New("sqlitestore: AlbumChange.Authorize is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback()
	fa, err := getFolderAccess(ctx, tx, folderID, c.ActorID)
	if err != nil {
		return classify(err)
	}
	p, err := principalTx(ctx, tx, c.ActorID, c.SessionID)
	if err != nil {
		return classify(err)
	}
	if err := c.Authorize(p, fa.Access); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}
	if err := fn(tx, fa, meta.ViewOf(p, fa.Access)); err != nil {
		return classify(err)
	}
	if c.BeforeCommit != nil {
		if err := c.BeforeCommit(); err != nil {
			return fmt.Errorf("before commit: %w", err)
		}
	}
	return classify(tx.Commit())
}

func recordAlbum(ctx context.Context, tx *sql.Tx, actor, action, targetType, targetID string, details map[string]any) error {
	return audit.Record(ctx, tx, audit.Entry{ActorUserID: actor, Action: action, TargetType: targetType,
		TargetID: targetID, Result: audit.Allowed, Details: details})
}

// invitationQuery selects a meta.Invitation (scanInvitation). It is
// completed by a WHERE clause.
const invitationQuery = `SELECT i.invitation_id, i.folder_id, f.folder_name, i.invited_user_id, ue.display_name,
		i.inviter_user_id, ur.display_name, i.role, i.status, i.created_at, i.expires_at, COALESCE(i.accepted_at, '')
	FROM folder_invitations i
	JOIN folders f ON f.folder_id = i.folder_id
	JOIN users ue ON ue.user_id = i.invited_user_id
	JOIN users ur ON ur.user_id = i.inviter_user_id
	WHERE `

func scanInvitation(row scanner, t time.Time) (meta.Invitation, error) {
	var inv meta.Invitation
	var role string
	err := row.Scan(&inv.ID, &inv.FolderID, &inv.FolderName, &inv.InviteeID, &inv.InviteeName,
		&inv.InviterID, &inv.InviterName, &role, &inv.Status, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Invitation{}, meta.ErrInvitationNotFound
	}
	if err != nil {
		return meta.Invitation{}, err
	}
	inv.Role = authz.AlbumRole(role)
	if inv.Status == meta.InvitationPending && expiredAt(inv.ExpiresAt, t) {
		inv.Status = meta.InvitationExpired
	}
	return inv, nil
}

func getInvitation(ctx context.Context, q queryer, id string) (meta.Invitation, error) {
	return scanInvitation(q.QueryRowContext(ctx, invitationQuery+"i.invitation_id = ?", id), time.Now())
}

func (s *Store) listInvitations(ctx context.Context, where string, args ...any) ([]meta.Invitation, error) {
	rows, err := s.db.QueryContext(ctx, invitationQuery+where+" ORDER BY i.created_at, i.invitation_id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := time.Now()
	out := []meta.Invitation{}
	for rows.Next() {
		inv, err := scanInvitation(rows, t)
		if err != nil {
			return nil, err
		}
		if inv.Status == meta.InvitationPending {
			out = append(out, inv)
		}
	}
	return out, rows.Err()
}

// ListFolderInvitations implements meta.Memberships.
func (s *Store) ListFolderInvitations(ctx context.Context, folderID string) ([]meta.Invitation, error) {
	return s.listInvitations(ctx, "i.folder_id = ? AND i.status = 'pending'", folderID)
}

// ListInvitationsFor implements meta.Memberships. Invitations written by
// someone who no longer owns the folder, and those of archived or deleted
// folders, are left out (a transfer, archive or delete revokes them
// anyway).
func (s *Store) ListInvitationsFor(ctx context.Context, userID string) ([]meta.Invitation, error) {
	return s.listInvitations(ctx, "i.invited_user_id = ? AND i.status = 'pending' AND f.archived_at IS NULL AND f.deleted_at IS NULL AND i.inviter_user_id = f.owner_user_id", userID)
}

// accountActive reports whether userID exists, is active and can log in.
func accountActive(ctx context.Context, q queryer, userID string) (bool, error) {
	var status, hashPrefix string
	err := q.QueryRowContext(ctx, "SELECT status, substr(password_hash, 1, 1) FROM users WHERE user_id = ?", userID).Scan(&status, &hashPrefix)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && status == "active" && hashPrefix != password.LockedPrefix, err
}

// memberRole returns userID's folder_members role, or ErrMemberNotFound.
func memberRole(ctx context.Context, q queryer, folderID, userID string) (authz.AlbumRole, error) {
	var role string
	err := q.QueryRowContext(ctx, "SELECT role FROM folder_members WHERE folder_id = ? AND user_id = ?", folderID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", meta.ErrMemberNotFound
	}
	return authz.AlbumRole(role), err
}

// revokePending revokes every pending invitation of a folder.
func revokePending(ctx context.Context, tx *sql.Tx, folderID string) (int64, error) {
	res, err := tx.ExecContext(ctx, "UPDATE folder_invitations SET status = 'revoked' WHERE folder_id = ? AND status = 'pending'", folderID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CreateInvitation implements meta.Memberships.
func (s *Store) CreateInvitation(ctx context.Context, c meta.AlbumChange) (meta.Invitation, error) {
	if !validRole(c.Role) {
		return meta.Invitation{}, meta.ErrInvalidRole
	}
	id, err := newID()
	if err != nil {
		return meta.Invitation{}, err
	}
	var inv meta.Invitation
	err = s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if c.TargetID == fa.Access.OwnerID {
			return meta.ErrAlreadyMember
		}
		// Unknown, disabled and locked accounts look the same.
		if ok, err := accountActive(ctx, tx, c.TargetID); err != nil {
			return err
		} else if !ok {
			return meta.ErrInviteeNotFound
		}
		if _, err := memberRole(ctx, tx, c.FolderID, c.TargetID); err == nil {
			return meta.ErrAlreadyMember
		} else if !errors.Is(err, meta.ErrMemberNotFound) {
			return err
		}
		created := time.Now().UTC()
		details := map[string]any{"folder_id": c.FolderID, "invitee_user_id": c.TargetID, "role": string(c.Role)}
		var oldID, oldExpires string
		err := tx.QueryRowContext(ctx, "SELECT invitation_id, expires_at FROM folder_invitations WHERE folder_id = ? AND invited_user_id = ? AND status = 'pending'",
			c.FolderID, c.TargetID).Scan(&oldID, &oldExpires)
		switch {
		case err == nil && !expiredAt(oldExpires, created):
			return meta.ErrInvitationPending
		case err == nil:
			// Expiry is decided when read; record it before replacing.
			if _, err := tx.ExecContext(ctx, "UPDATE folder_invitations SET status = 'expired' WHERE invitation_id = ?", oldID); err != nil {
				return err
			}
			details["expired_invitation_id"] = oldID
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		ts := created.Format(timeLayout)
		expires := created.Add(meta.InvitationTTL).Format(timeLayout)
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_invitations
			(invitation_id, folder_id, invited_user_id, inviter_user_id, role, status, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`, id, c.FolderID, c.TargetID, c.ActorID, string(c.Role), expires, ts); err != nil {
			return err
		}
		details["expires_at"] = expires
		if err := recordAlbum(ctx, tx, c.ActorID, "invitation.create", "invitation", id, details); err != nil {
			return err
		}
		inv, err = getInvitation(ctx, tx, id)
		return err
	})
	return inv, err
}

// RevokeInvitation implements meta.Memberships.
func (s *Store) RevokeInvitation(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		inv, err := getInvitation(ctx, tx, c.TargetID)
		if err != nil {
			return err
		}
		if inv.FolderID != c.FolderID {
			return meta.ErrInvitationNotFound
		}
		if inv.Status != meta.InvitationPending {
			return meta.ErrInvitationGone
		}
		if _, err := tx.ExecContext(ctx, "UPDATE folder_invitations SET status = 'revoked' WHERE invitation_id = ?", inv.ID); err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "invitation.revoke", "invitation", inv.ID,
			map[string]any{"folder_id": inv.FolderID, "invitee_user_id": inv.InviteeID, "role": string(inv.Role)})
	})
}

// AcceptInvitation implements meta.Memberships. Nothing is written unless
// the invitation is accepted: an expired or stale invitation is reported
// (ErrInvitationGone) and left as it is.
func (s *Store) AcceptInvitation(ctx context.Context, c meta.AlbumChange) (meta.FolderAccess, error) {
	if c.Authorize == nil {
		return meta.FolderAccess{}, errors.New("sqlitestore: AlbumChange.Authorize is required")
	}
	fa, err := s.acceptInvitation(ctx, c)
	return fa, classify(err)
}

// inviteeTx starts the write transaction of an invitee's answer and decides
// it: the caller's account and session, the invitation (ErrInvitationNotFound),
// c.Authorize, the invitee (ErrNotInvitee) and whether the invitation can
// still be answered (ErrInvitationGone). The caller must roll tx back.
func (s *Store) inviteeTx(ctx context.Context, c meta.AlbumChange) (*sql.Tx, meta.Invitation, meta.FolderAccess, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, meta.Invitation{}, meta.FolderAccess{}, err
	}
	inv, fa, err := decideInvitee(ctx, tx, c)
	if err != nil {
		tx.Rollback()
		return nil, meta.Invitation{}, meta.FolderAccess{}, err
	}
	return tx, inv, fa, nil
}

func decideInvitee(ctx context.Context, tx *sql.Tx, c meta.AlbumChange) (meta.Invitation, meta.FolderAccess, error) {
	p, err := principalTx(ctx, tx, c.ActorID, c.SessionID)
	if err != nil {
		return meta.Invitation{}, meta.FolderAccess{}, err
	}
	inv, err := getInvitation(ctx, tx, c.TargetID)
	if err != nil {
		return meta.Invitation{}, meta.FolderAccess{}, err
	}
	fa, err := getFolderAccess(ctx, tx, inv.FolderID, c.ActorID)
	archived := errors.Is(err, meta.ErrFolderNotFound)
	if err != nil && !archived {
		return meta.Invitation{}, meta.FolderAccess{}, err
	}
	if err := c.Authorize(p, fa.Access); err != nil {
		return meta.Invitation{}, meta.FolderAccess{}, fmt.Errorf("authorize: %w", err)
	}
	if inv.InviteeID != c.ActorID {
		return meta.Invitation{}, meta.FolderAccess{}, meta.ErrNotInvitee
	}
	// Only the folder's current owner can have invited: a transfer revokes
	// pending invitations, and this also covers rows written otherwise.
	if archived || inv.Status != meta.InvitationPending || inv.InviterID != fa.Access.OwnerID {
		return meta.Invitation{}, meta.FolderAccess{}, meta.ErrInvitationGone
	}
	return inv, fa, nil
}

func (s *Store) acceptInvitation(ctx context.Context, c meta.AlbumChange) (meta.FolderAccess, error) {
	tx, inv, fa, err := s.inviteeTx(ctx, c)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	defer tx.Rollback()
	if fa.Access.OwnerID == c.ActorID || fa.Access.HasMembership {
		return meta.FolderAccess{}, meta.ErrAlreadyMember
	}
	ts := now()
	if _, err := tx.ExecContext(ctx, "INSERT INTO folder_members (folder_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)",
		inv.FolderID, c.ActorID, string(inv.Role), ts); err != nil {
		return meta.FolderAccess{}, err
	}
	// The member count as it is with the new member.
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM folder_members mm JOIN users u ON u.user_id = mm.user_id WHERE mm.folder_id = ? AND "+activeUser+
		") + (SELECT count(*) FROM users u WHERE u.user_id = ? AND "+activeUser+")", inv.FolderID, fa.Access.OwnerID).Scan(&fa.MemberCount); err != nil {
		return meta.FolderAccess{}, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE folder_invitations SET status = 'accepted', accepted_at = ? WHERE invitation_id = ? AND status = 'pending'", ts, inv.ID)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return meta.FolderAccess{}, fmt.Errorf("accept invitation: %d rows changed: %v", n, err)
	}
	if err := recordAlbum(ctx, tx, c.ActorID, "invitation.accept", "invitation", inv.ID,
		map[string]any{"folder_id": inv.FolderID, "role": string(inv.Role), "inviter_user_id": inv.InviterID}); err != nil {
		return meta.FolderAccess{}, err
	}
	if c.BeforeCommit != nil {
		if err := c.BeforeCommit(); err != nil {
			return meta.FolderAccess{}, fmt.Errorf("before commit: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return meta.FolderAccess{}, err
	}
	fa.Access.HasMembership, fa.Access.MemberRole = true, inv.Role
	return fa, nil
}

// DeclineInvitation implements meta.Memberships. Like AcceptInvitation,
// an expired or stale invitation is reported (ErrInvitationGone) and left as
// it is. The audit row names the folder, the role and the inviter.
func (s *Store) DeclineInvitation(ctx context.Context, c meta.AlbumChange) error {
	if c.Authorize == nil {
		return errors.New("sqlitestore: AlbumChange.Authorize is required")
	}
	return classify(s.declineInvitation(ctx, c))
}

func (s *Store) declineInvitation(ctx context.Context, c meta.AlbumChange) error {
	tx, inv, _, err := s.inviteeTx(ctx, c)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE folder_invitations SET status = 'declined' WHERE invitation_id = ? AND status = 'pending'", inv.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("decline invitation: %d rows changed: %v", n, err)
	}
	if err := recordAlbum(ctx, tx, c.ActorID, "invitation.decline", "invitation", inv.ID,
		map[string]any{"folder_id": inv.FolderID, "role": string(inv.Role), "inviter_user_id": inv.InviterID}); err != nil {
		return err
	}
	if c.BeforeCommit != nil {
		if err := c.BeforeCommit(); err != nil {
			return fmt.Errorf("before commit: %w", err)
		}
	}
	return tx.Commit()
}

// ListMembers implements meta.Memberships.
func (s *Store) ListMembers(ctx context.Context, folderID string) ([]meta.Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.user_id, u.display_name, 'OWNER', u.status, substr(u.password_hash, 1, 1), f.created_at, 0
			FROM folders f JOIN users u ON u.user_id = f.owner_user_id WHERE f.folder_id = ? AND f.archived_at IS NULL AND f.deleted_at IS NULL
		UNION ALL
		SELECT u.user_id, u.display_name, m.role, u.status, substr(u.password_hash, 1, 1), m.joined_at, 1
			FROM folder_members m JOIN folders f ON f.folder_id = m.folder_id JOIN users u ON u.user_id = m.user_id
			WHERE m.folder_id = ? AND f.archived_at IS NULL AND f.deleted_at IS NULL
		ORDER BY 7, 6, 1`, folderID, folderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []meta.Member{}
	for rows.Next() {
		var m meta.Member
		var status, hashPrefix string
		var order int
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Role, &status, &hashPrefix, &m.JoinedAt, &order); err != nil {
			return nil, err
		}
		m.Active = status == "active" && hashPrefix != password.LockedPrefix
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, meta.ErrFolderNotFound
	}
	return out, nil
}

func getMember(ctx context.Context, q queryer, folderID, userID string) (meta.Member, error) {
	var m meta.Member
	var status, hashPrefix string
	err := q.QueryRowContext(ctx, `SELECT u.user_id, u.display_name, m.role, u.status, substr(u.password_hash, 1, 1), m.joined_at
		FROM folder_members m JOIN users u ON u.user_id = m.user_id WHERE m.folder_id = ? AND m.user_id = ?`, folderID, userID).
		Scan(&m.UserID, &m.DisplayName, &m.Role, &status, &hashPrefix, &m.JoinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return meta.Member{}, meta.ErrMemberNotFound
	}
	m.Active = status == "active" && hashPrefix != password.LockedPrefix
	return m, err
}

// SetMemberRole implements meta.Memberships. Setting the current role
// changes nothing and records nothing.
func (s *Store) SetMemberRole(ctx context.Context, c meta.AlbumChange) (meta.Member, error) {
	if !validRole(c.Role) {
		return meta.Member{}, meta.ErrInvalidRole
	}
	var m meta.Member
	err := s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if c.TargetID == fa.Access.OwnerID {
			return meta.ErrTargetIsOwner
		}
		old, err := memberRole(ctx, tx, c.FolderID, c.TargetID)
		if err != nil {
			return err
		}
		if old != c.Role {
			if _, err := tx.ExecContext(ctx, "UPDATE folder_members SET role = ? WHERE folder_id = ? AND user_id = ?", string(c.Role), c.FolderID, c.TargetID); err != nil {
				return err
			}
			details := map[string]any{"user_id": c.TargetID, "from_role": string(old), "to_role": string(c.Role)}
			if c.Role == authz.Uploader {
				// An upload-only member sees nothing of the album, so its
				// likes go (its comments stay, shown without author).
				n, err := dropLikes(ctx, tx, c.FolderID, c.TargetID)
				if err != nil {
					return err
				}
				details["likes_removed"] = n
			}
			if err := recordAlbum(ctx, tx, c.ActorID, "member.role_change", "folder", c.FolderID, details); err != nil {
				return err
			}
		}
		m, err = getMember(ctx, tx, c.FolderID, c.TargetID)
		return err
	})
	return m, err
}

// RemoveMember implements meta.Memberships.
func (s *Store) RemoveMember(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if c.TargetID == fa.Access.OwnerID {
			return meta.ErrTargetIsOwner
		}
		old, err := memberRole(ctx, tx, c.FolderID, c.TargetID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM folder_members WHERE folder_id = ? AND user_id = ?", c.FolderID, c.TargetID); err != nil {
			return err
		}
		// Its likes go; its comments stay, shown without author.
		n, err := dropLikes(ctx, tx, c.FolderID, c.TargetID)
		if err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "member.remove", "folder", c.FolderID,
			map[string]any{"user_id": c.TargetID, "role": string(old), "likes_removed": n})
	})
}

// PreviousOwnerRole is the role the previous owner keeps after a transfer.
const PreviousOwnerRole = authz.Contributor

// TransferOwnership implements meta.Memberships.
func (s *Store) TransferOwnership(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		prev := fa.Access.OwnerID
		if c.TargetID == prev {
			return meta.ErrTargetIsOwner
		}
		oldRole, err := memberRole(ctx, tx, c.FolderID, c.TargetID)
		if err != nil {
			return err
		}
		if ok, err := accountActive(ctx, tx, c.TargetID); err != nil {
			return err
		} else if !ok {
			return meta.ErrNewOwnerInactive
		}
		// Order matters for the triggers: the new owner must not be a
		// member when it becomes owner, and the previous owner can only
		// become a member once it is no longer the owner.
		if _, err := tx.ExecContext(ctx, "DELETE FROM folder_members WHERE folder_id = ? AND user_id = ?", c.FolderID, c.TargetID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE folders SET owner_user_id = ? WHERE folder_id = ? AND owner_user_id = ?", c.TargetID, c.FolderID, prev)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("transfer owner: %d rows changed: %v", n, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO folder_members (folder_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)",
			c.FolderID, prev, string(PreviousOwnerRole), now()); err != nil {
			return err
		}
		n, err := revokePending(ctx, tx, c.FolderID)
		if err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "folder.transfer_owner", "folder", c.FolderID, map[string]any{
			"previous_owner_id": prev, "new_owner_id": c.TargetID, "new_owner_previous_role": string(oldRole),
			"previous_owner_role": string(PreviousOwnerRole), "invitations_revoked": n})
	})
}

// ArchiveFolder implements meta.Memberships. The folder row, its assets and
// its memberships are kept; every route treats it as unknown.
func (s *Store) ArchiveFolder(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		if _, err := tx.ExecContext(ctx, "UPDATE folders SET archived_at = ? WHERE folder_id = ? AND archived_at IS NULL", now(), c.FolderID); err != nil {
			return err
		}
		n, err := revokePending(ctx, tx, c.FolderID)
		if err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "folder.archive", "folder", c.FolderID,
			map[string]any{"folder_name": fa.Folder.Name, "invitations_revoked": n})
	})
}
