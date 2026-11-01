package meta

import (
	"context"
	"errors"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
)

// Invitation history and album invite links (M57).

// Why an invitation or an invite link was revoked
// (folder_invitations.revoke_reason, invite_links.revoke_reason).
const (
	ReasonRevokedByOwner = "revoked_by_owner" // the owner revoked it
	ReasonAlbumDeleted   = "album_deleted"
	ReasonAlbumArchived  = "album_archived"
	ReasonOwnerChanged   = "owner_changed" // ownership transfer or reassignment
	// ReasonReplaced: a newer invitation of the same album and account
	// replaced it (CreateInvitation with ReplacePending, or a redeemed
	// invite link). Invitations only.
	ReasonReplaced = "replaced"
)

// Why an invitation can no longer be answered or an invite link no longer
// be redeemed, besides the revocation reasons above.
const (
	GoneAccepted = "accepted"
	GoneDeclined = "declined"
	GoneExpired  = "expired"
	// GoneRevoked: revoked before revocation reasons were recorded.
	GoneRevoked = "revoked"
	GoneUsedUp  = "used_up" // an invite link redeemed max_uses times
)

// GoneError is ErrInvitationGone or ErrInviteLinkGone with its reason
// (one of the Gone* or Reason* values). errors.Is matches the wrapped one.
type GoneError struct {
	Err    error
	Reason string
}

func (e *GoneError) Error() string { return e.Err.Error() + ": " + e.Reason }
func (e *GoneError) Unwrap() error { return e.Err }

// GoneReason returns the reason of a GoneError in err's chain, or "".
func GoneReason(err error) string {
	var g *GoneError
	if errors.As(err, &g) {
		return g.Reason
	}
	return ""
}

// InvitationNotJoined is the status the owner's history shows for a
// declined invitation: the owner learns that the account did not join,
// worded neutrally, like an expired one.
const InvitationNotJoined = "not_joined"

// MaxHistoryDays is the longest window of answered invitations a history
// shows.
const MaxHistoryDays = 30

// InvitationQuery selects a page of invitation history: the invitations
// addressed to InviteeID, or those of FolderID (the owner's view). Exactly
// one of the two is set.
type InvitationQuery struct {
	InviteeID string
	FolderID  string
	// Recent adds invitations answered (accepted, declined, expired or
	// revoked) at or after Since; otherwise only pending ones are listed.
	// Invitations revoked because a newer one replaced them are never
	// listed.
	Recent bool
	Since  time.Time
	Now    time.Time // decides expiry
	Limit  int       // > 0
	After  string    // InvitationPage.NextCursor of the previous page, or ""
}

// InvitationPage is one page of invitation history: the pending
// invitations first, newest first, then the answered ones, most recently
// answered first.
type InvitationPage struct {
	Invitations []Invitation // never nil
	// NextCursor continues after the last invitation; "" when there are
	// no more.
	NextCursor string
}

// ErrInvalidCursor: a cursor that no page returned.
var ErrInvalidCursor = errors.New("invalid cursor")

// Invite link limits.
const (
	InviteLinkDefaultTTL   = 7 * 24 * time.Hour
	InviteLinkMaxTTL       = 30 * 24 * time.Hour
	InviteLinkMaxUses      = 20
	InviteLinkMaxActive    = 20 // usable links per album at once
	InviteLinkHistoryLimit = 100
)

// Invite link statuses (effective, decided when read).
const (
	LinkActive  = "active"
	LinkExpired = "expired"
	LinkUsedUp  = "used_up"
	LinkRevoked = "revoked"
)

// InviteLink is an album invite link as its owner sees it. The token is
// never stored: only the reply that creates the link carries it.
type InviteLink struct {
	ID          string
	FolderID    string
	FolderName  string
	CreatorID   string
	CreatorName string // display name
	Role        authz.AlbumRole
	MaxUses     int
	UseCount    int
	CreatedAt   string
	ExpiresAt   string
	LastUsedAt  string // "" if never redeemed
	RevokedAt   string // "" unless revoked
	Status      string // LinkActive, LinkExpired, LinkUsedUp or LinkRevoked
	Reason      string // revocation reason ("" unless revoked)
}

// NewInviteLink is a CreateInviteLink request; the change's FolderID,
// ActorID, SessionID, Role and Authorize apply.
type NewInviteLink struct {
	Change    AlbumChange
	TokenHash string // hex SHA-256 of the token the caller generated
	TTL       time.Duration
	MaxUses   int
}

// LinkPreview is what anyone holding an invite link's token may learn:
// the album's name, who invites, the role and until when. Never the
// album's contents, members or remaining uses.
type LinkPreview struct {
	FolderName  string
	InviterName string
	Role        authz.AlbumRole
	ExpiresAt   string
}

// Invite link errors.
var (
	// ErrInviteLinkNotFound: no link has this token (or this ID in this
	// album).
	ErrInviteLinkNotFound = errors.New("invite link not found")
	// ErrInviteLinkGone: expired, used up or revoked (see GoneError).
	ErrInviteLinkGone = errors.New("invite link no longer valid")
	// ErrTooManyInviteLinks: the album has InviteLinkMaxActive usable links.
	ErrTooManyInviteLinks = errors.New("too many active invite links")
	ErrInvalidInviteLink  = errors.New("invalid invite link settings")
)

// InviteLinks manages album invite links. Writes run in one transaction
// that re-checks the caller and records an audit row (never the token).
type InviteLinks interface {
	// CreateInviteLink stores a link of c.FolderID (OWNER, decided by
	// Authorize) with role c.Role. ErrInvalidRole, ErrInvalidInviteLink,
	// ErrTooManyInviteLinks.
	CreateInviteLink(ctx context.Context, req NewInviteLink) (InviteLink, error)
	// ListInviteLinks returns the folder's usable links and the others of
	// the last MaxHistoryDays, newest first, at most InviteLinkHistoryLimit.
	ListInviteLinks(ctx context.Context, folderID string, now time.Time) ([]InviteLink, error)
	// RevokeInviteLink revokes link c.TargetID of c.FolderID.
	// ErrInviteLinkNotFound, ErrInviteLinkGone (revoked already).
	RevokeInviteLink(ctx context.Context, c AlbumChange) error
	// PreviewInviteLink looks a usable link up by its token hash.
	// ErrInviteLinkNotFound, ErrInviteLinkGone.
	PreviewInviteLink(ctx context.Context, tokenHash string, now time.Time) (LinkPreview, error)
	// RedeemInviteLink makes the caller (c.ActorID, c.SessionID; Authorize
	// decides on the principal) a member of the link's folder with the
	// link's role, uses one of its uses, records an accepted invitation
	// naming the link (a pending invitation of the same album and account
	// is revoked as replaced) and returns the folder with the caller's new
	// relation. ErrInviteLinkNotFound, ErrInviteLinkGone, ErrAlreadyMember
	// (the owner or a member: no use is spent; the folder is returned with
	// the error, so that the caller can name it).
	RedeemInviteLink(ctx context.Context, c AlbumChange, tokenHash string) (FolderAccess, error)
}
