package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Album invite links (meta.InviteLinks). A link is found by the SHA-256 of
// its token; the token itself is never stored, logged or audited. A
// redemption is one BEGIN IMMEDIATE transaction, so two accounts racing
// for a link's last use are serialized and only one gets it.

var _ meta.InviteLinks = (*Store)(nil)

// linkQuery selects a linkRow (scanLink). It is completed by a WHERE clause.
const linkQuery = `SELECT l.link_id, l.folder_id, f.folder_name, l.creator_user_id, u.display_name, l.role,
		l.max_uses, l.use_count, l.created_at, l.expires_at, COALESCE(l.last_used_at, ''),
		COALESCE(l.revoked_at, ''), COALESCE(l.revoke_reason, ''),
		f.owner_user_id, f.archived_at IS NOT NULL, f.deleted_at IS NOT NULL
	FROM invite_links l
	JOIN folders f ON f.folder_id = l.folder_id
	JOIN users u ON u.user_id = l.creator_user_id
	WHERE `

// linkRow is an invite link with the state of its folder.
type linkRow struct {
	meta.InviteLink
	ownerID                       string
	folderArchived, folderDeleted bool
}

func scanLink(row scanner, t time.Time) (linkRow, error) {
	var r linkRow
	l := &r.InviteLink
	var role string
	err := row.Scan(&l.ID, &l.FolderID, &l.FolderName, &l.CreatorID, &l.CreatorName, &role, &l.MaxUses, &l.UseCount,
		&l.CreatedAt, &l.ExpiresAt, &l.LastUsedAt, &l.RevokedAt, &l.Reason, &r.ownerID, &r.folderArchived, &r.folderDeleted)
	if errors.Is(err, sql.ErrNoRows) {
		return linkRow{}, meta.ErrInviteLinkNotFound
	}
	if err != nil {
		return linkRow{}, err
	}
	l.Role = authz.AlbumRole(role)
	switch {
	case l.RevokedAt != "":
		l.Status = meta.LinkRevoked
	case l.UseCount >= l.MaxUses:
		l.Status = meta.LinkUsedUp
	case expiredAt(l.ExpiresAt, t):
		l.Status = meta.LinkExpired
	default:
		l.Status = meta.LinkActive
	}
	return r, nil
}

// inactiveSince is when a link stopped being usable ("" while it is).
func (r linkRow) inactiveSince() string {
	switch r.Status {
	case meta.LinkRevoked:
		return r.RevokedAt
	case meta.LinkUsedUp:
		return r.LastUsedAt
	case meta.LinkExpired:
		return r.ExpiresAt
	}
	return ""
}

// gone is why the link can no longer be redeemed, or "".
func (r linkRow) gone() string {
	switch r.Status {
	case meta.LinkRevoked:
		return r.Reason
	case meta.LinkUsedUp:
		return meta.GoneUsedUp
	case meta.LinkExpired:
		return meta.GoneExpired
	}
	switch {
	case r.folderDeleted:
		return meta.ReasonAlbumDeleted
	case r.folderArchived:
		return meta.ReasonAlbumArchived
	case r.CreatorID != r.ownerID:
		return meta.ReasonOwnerChanged
	}
	return ""
}

func linkByHash(ctx context.Context, q queryer, tokenHash string, t time.Time) (linkRow, error) {
	return scanLink(q.QueryRowContext(ctx, linkQuery+"l.token_sha256 = ?", tokenHash), t)
}

func validTokenHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CreateInviteLink implements meta.InviteLinks.
func (s *Store) CreateInviteLink(ctx context.Context, req meta.NewInviteLink) (meta.InviteLink, error) {
	c := req.Change
	if !validRole(c.Role) {
		return meta.InviteLink{}, meta.ErrInvalidRole
	}
	if req.TTL <= 0 || req.TTL > meta.InviteLinkMaxTTL || req.MaxUses < 1 || req.MaxUses > meta.InviteLinkMaxUses || !validTokenHash(req.TokenHash) {
		return meta.InviteLink{}, meta.ErrInvalidInviteLink
	}
	id, err := newID()
	if err != nil {
		return meta.InviteLink{}, err
	}
	var link meta.InviteLink
	err = s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		created := time.Now().UTC()
		ts := created.Format(timeLayout)
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invite_links WHERE folder_id = ? AND revoked_at IS NULL
			AND use_count < max_uses AND expires_at > ?`, c.FolderID, ts).Scan(&active); err != nil {
			return err
		}
		if active >= meta.InviteLinkMaxActive {
			return meta.ErrTooManyInviteLinks
		}
		expires := created.Add(req.TTL).Format(timeLayout)
		if _, err := tx.ExecContext(ctx, `INSERT INTO invite_links
			(link_id, folder_id, creator_user_id, role, token_sha256, max_uses, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, c.FolderID, c.ActorID, string(c.Role), req.TokenHash, req.MaxUses, ts, expires); err != nil {
			return err
		}
		if err := recordAlbum(ctx, tx, c.ActorID, "invite_link.create", "invite_link", id, map[string]any{
			"folder_id": c.FolderID, "role": string(c.Role), "max_uses": req.MaxUses, "expires_at": expires}); err != nil {
			return err
		}
		r, err := scanLink(tx.QueryRowContext(ctx, linkQuery+"l.link_id = ?", id), created)
		link = r.InviteLink
		return err
	})
	return link, err
}

// ListInviteLinks implements meta.InviteLinks.
func (s *Store) ListInviteLinks(ctx context.Context, folderID string, t time.Time) ([]meta.InviteLink, error) {
	window := time.Duration(meta.MaxHistoryDays) * 24 * time.Hour
	since := t.Add(-window).UTC().Format(timeLayout)
	// A link stops being usable at most InviteLinkMaxTTL after it was made.
	rows, err := s.db.QueryContext(ctx, linkQuery+"l.folder_id = ? AND l.created_at >= ? ORDER BY l.created_at DESC, l.link_id DESC",
		folderID, t.Add(-window-meta.InviteLinkMaxTTL).UTC().Format(timeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []meta.InviteLink{}
	for rows.Next() && len(out) < meta.InviteLinkHistoryLimit {
		r, err := scanLink(rows, t)
		if err != nil {
			return nil, err
		}
		if at := r.inactiveSince(); r.Status != meta.LinkActive && at < since {
			continue
		}
		out = append(out, r.InviteLink)
	}
	return out, rows.Err()
}

// RevokeInviteLink implements meta.InviteLinks.
func (s *Store) RevokeInviteLink(ctx context.Context, c meta.AlbumChange) error {
	return s.changeTx(ctx, c, c.FolderID, func(tx *sql.Tx, fa meta.FolderAccess) error {
		r, err := scanLink(tx.QueryRowContext(ctx, linkQuery+"l.link_id = ? AND l.folder_id = ?", c.TargetID, c.FolderID), time.Now())
		if err != nil {
			return err
		}
		if r.Status == meta.LinkRevoked {
			return &meta.GoneError{Err: meta.ErrInviteLinkGone, Reason: r.Reason}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE invite_links SET revoked_at = ?, revoke_reason = ? WHERE link_id = ?",
			now(), meta.ReasonRevokedByOwner, r.ID); err != nil {
			return err
		}
		return recordAlbum(ctx, tx, c.ActorID, "invite_link.revoke", "invite_link", r.ID,
			map[string]any{"folder_id": r.FolderID, "role": string(r.Role), "use_count": r.UseCount, "status_before": r.Status})
	})
}

// PreviewInviteLink implements meta.InviteLinks.
func (s *Store) PreviewInviteLink(ctx context.Context, tokenHash string, t time.Time) (meta.LinkPreview, error) {
	if !validTokenHash(tokenHash) {
		return meta.LinkPreview{}, meta.ErrInviteLinkNotFound
	}
	r, err := linkByHash(ctx, s.db, tokenHash, t)
	if err != nil {
		return meta.LinkPreview{}, err
	}
	if reason := r.gone(); reason != "" {
		return meta.LinkPreview{}, &meta.GoneError{Err: meta.ErrInviteLinkGone, Reason: reason}
	}
	return meta.LinkPreview{FolderName: r.FolderName, InviterName: r.CreatorName, Role: r.Role, ExpiresAt: r.ExpiresAt}, nil
}

// RedeemInviteLink implements meta.InviteLinks.
func (s *Store) RedeemInviteLink(ctx context.Context, c meta.AlbumChange, tokenHash string) (meta.FolderAccess, error) {
	if c.Authorize == nil {
		return meta.FolderAccess{}, errors.New("sqlitestore: AlbumChange.Authorize is required")
	}
	if !validTokenHash(tokenHash) {
		return meta.FolderAccess{}, meta.ErrInviteLinkNotFound
	}
	fa, err := s.redeemInviteLink(ctx, c, tokenHash)
	return fa, classify(err)
}

func (s *Store) redeemInviteLink(ctx context.Context, c meta.AlbumChange, tokenHash string) (meta.FolderAccess, error) {
	invID, err := newID()
	if err != nil {
		return meta.FolderAccess{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	defer tx.Rollback()
	p, err := principalTx(ctx, tx, c.ActorID, c.SessionID)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	t := time.Now().UTC()
	r, err := linkByHash(ctx, tx, tokenHash, t)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	fa, err := getFolderAccess(ctx, tx, r.FolderID, c.ActorID)
	inUse := err == nil
	if err != nil && !errors.Is(err, meta.ErrFolderNotFound) {
		return meta.FolderAccess{}, err
	}
	if err := c.Authorize(p, fa.Access); err != nil {
		return meta.FolderAccess{}, fmt.Errorf("authorize: %w", err)
	}
	// Already in the album: nothing is spent, whatever the link's state.
	if inUse && (fa.Access.OwnerID == c.ActorID || fa.Access.HasMembership) {
		return fa, meta.ErrAlreadyMember
	}
	if reason := r.gone(); reason != "" {
		return meta.FolderAccess{}, &meta.GoneError{Err: meta.ErrInviteLinkGone, Reason: reason}
	}
	ts := t.Format(timeLayout)
	res, err := tx.ExecContext(ctx, "UPDATE invite_links SET use_count = use_count + 1, last_used_at = ? WHERE link_id = ? AND use_count < max_uses AND revoked_at IS NULL",
		ts, r.ID)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return meta.FolderAccess{}, err
	} else if n != 1 {
		return meta.FolderAccess{}, &meta.GoneError{Err: meta.ErrInviteLinkGone, Reason: meta.GoneUsedUp}
	}
	// A pending invitation of the same album is replaced by this answer.
	res, err = tx.ExecContext(ctx, "UPDATE folder_invitations SET status = 'revoked', revoked_at = ?, revoke_reason = ? WHERE folder_id = ? AND invited_user_id = ? AND status = 'pending'",
		ts, meta.ReasonReplaced, r.FolderID, c.ActorID)
	if err != nil {
		return meta.FolderAccess{}, err
	}
	replaced, err := res.RowsAffected()
	if err != nil {
		return meta.FolderAccess{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO folder_members (folder_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)",
		r.FolderID, c.ActorID, string(r.Role), ts); err != nil {
		return meta.FolderAccess{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO folder_invitations
		(invitation_id, folder_id, invited_user_id, inviter_user_id, role, status, expires_at, accepted_at, created_at, link_id)
		VALUES (?, ?, ?, ?, ?, 'accepted', ?, ?, ?, ?)`, invID, r.FolderID, c.ActorID, r.CreatorID, string(r.Role), r.ExpiresAt, ts, ts, r.ID); err != nil {
		return meta.FolderAccess{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM folder_members mm JOIN users u ON u.user_id = mm.user_id WHERE mm.folder_id = ? AND "+activeUser+
		") + (SELECT count(*) FROM users u WHERE u.user_id = ? AND "+activeUser+")", r.FolderID, fa.Access.OwnerID).Scan(&fa.MemberCount); err != nil {
		return meta.FolderAccess{}, err
	}
	if err := recordAlbum(ctx, tx, c.ActorID, "invite_link.redeem", "invite_link", r.ID, map[string]any{
		"folder_id": r.FolderID, "role": string(r.Role), "inviter_user_id": r.CreatorID, "invitation_id": invID,
		"use_count": r.UseCount + 1, "max_uses": r.MaxUses, "pending_replaced": replaced}); err != nil {
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
	fa.Access.HasMembership, fa.Access.MemberRole = true, r.Role
	return fa, nil
}
