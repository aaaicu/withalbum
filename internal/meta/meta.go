// Package meta defines the metadata store used by the HTTP API: folders and
// their owner and members, assets, folder links and per-folder change events.
//
// Implementations only store metadata. Blob files live in internal/blobstore,
// and callers must verify the storage volume before calling a write method.
// The store reports how a user is related to a folder (FolderAccess) and
// re-reads it inside write transactions (Authorizer); the decision itself is
// always made by the caller with internal/authz.Allowed.
package meta

import (
	"context"
	"errors"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
)

// Folder is an album.
type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// Asset is one original in a folder. ID is a random, opaque identifier;
// SHA256 identifies the bytes (the blob) and is internal to the server. Two
// folders holding the same bytes share the blob but not the asset (assets
// imported from the PoC state.json may be linked to several folders).
//
// The JSON tags are the PoC state.json format (internal/meta/jsonstore, which
// only produces legacy data for tests and uses the SHA-256 as ID). The HTTP
// API builds its own response bodies.
type Asset struct {
	ID           string `json:"id"`
	SHA256       string `json:"-"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Bytes        int64  `json:"bytes"`
	PreviewReady bool   `json:"preview_ready"`
	UploadedAt   string `json:"uploaded_at"`
}

// Link records that an asset belongs to a folder.
type Link struct {
	AssetID          string `json:"asset_id"`
	UploaderDeviceID string `json:"uploader_device_id"`
	AddedAt          string `json:"added_at"`
}

// Event is one entry of a folder's change feed. Seq starts at 1 per folder
// and increases by one with every event.
type Event struct {
	Seq              int64  `json:"seq"`
	Kind             string `json:"kind"`
	AssetID          string `json:"asset_id"`
	FolderID         string `json:"folder_id"`
	UploaderDeviceID string `json:"uploader_device_id"`
	OccurredAt       string `json:"occurred_at"`
}

// EventAssetAdded is the Event.Kind written by CommitUpload.
const EventAssetAdded = "asset_added"

// CommitResult values.
const (
	ResultCreated        = "created"         // new blob; a new asset linked to the folder
	ResultLinked         = "linked"          // known blob; a new asset linked to the folder
	ResultAlreadyPresent = "already_present" // the folder already holds these bytes; nothing changed
)

var (
	ErrFolderNotFound = errors.New("folder not found")
	ErrAssetNotFound  = errors.New("asset not found")
	// ErrIDGeneration means no random identifier could be generated.
	ErrIDGeneration = errors.New("id generation failed")
	ErrInvalidLimit = errors.New("limit must be positive")
	// ErrBusy means a write could not get the database write lock in time
	// (concurrent writers). Nothing was written; retrying later is safe.
	ErrBusy = errors.New("metadata store is busy")
)

// Authorizer re-checks a write inside the store's write transaction, with
// the principal and the folder access exactly as stored at that moment. The
// principal is inactive (Active false) when the account is disabled or
// cannot log in, or when the request's session or its device is revoked; an
// unknown account gives the zero Principal. A non-nil error aborts the write.
type Authorizer func(p authz.Principal, access authz.AlbumAccess) error

// NewFolder is a CreateFolder request.
type NewFolder struct {
	Name    string
	OwnerID string // users.user_id of the creator, who becomes the only owner
	// SessionID is the creator's session; empty for console tools and tests.
	SessionID string
	// Authorize, if set, runs inside the write transaction before anything
	// is written (access is the zero AlbumAccess). If it returns an error
	// nothing is written and CreateFolder returns an error wrapping it.
	Authorize Authorizer
}

// FolderAccess is a folder together with one user's relation to it.
// Access.OwnerID is always set; HasMembership and MemberRole describe the user.
type FolderAccess struct {
	Folder Folder
	Access authz.AlbumAccess
}

// CommitRequest links an uploaded blob to a folder.
type CommitRequest struct {
	FolderID string
	DeviceID string // free-form device label of the uploader (X-Device-ID)
	// UserID is the uploading account, recorded as uploader (empty: none).
	UserID string
	// SessionID is the uploader's session, see NewFolder.SessionID.
	SessionID string
	// Asset describes the original by SHA256, Filename, MimeType, Bytes and
	// PreviewReady. The store assigns ID and UploadedAt. The blob file must
	// already be in place.
	Asset Asset
	// Authorize, if set, re-checks the uploader against account, session,
	// owner and membership as stored at that moment, under the write lock, so
	// a permission revoked during the upload is honored. It is called for
	// every outcome, ResultAlreadyPresent included. If it returns an error
	// nothing is written and CommitUpload returns an error wrapping it.
	Authorize Authorizer
	// BeforeCommit, if set, is called after all changes are prepared and
	// immediately before they are made durable, while the store holds its
	// write lock. If it returns an error nothing is written and CommitUpload
	// returns an error wrapping it. It is not called when the result is
	// ResultAlreadyPresent (nothing to write). The HTTP API re-verifies the
	// storage volume and the pool file here.
	BeforeCommit func() error
}

// CommitResult reports what CommitUpload did. Asset is the stored asset: the
// new one, or the folder's existing one for ResultAlreadyPresent.
type CommitResult struct {
	Result string
	Asset  Asset
}

// EventPage is one page of a folder's change feed.
type EventPage struct {
	Events     []Event          // never nil
	Assets     map[string]Asset // assets referenced by Events, by Asset.ID; never nil
	NextCursor int64            // last Seq in Events, or the input cursor if empty
	HasMore    bool             // events after NextCursor exist
}

// Invitation statuses (folder_invitations.status). A pending invitation
// whose ExpiresAt has passed is reported as InvitationExpired; the stored
// row may still say pending.
const (
	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationRevoked  = "revoked"
	InvitationExpired  = "expired"
)

// InvitationTTL is how long an invitation can be accepted.
const InvitationTTL = 7 * 24 * time.Hour

// Invitation names one registered account (InviteeID) that the folder's
// owner invited with Role. Only that account can accept it, with its own
// session; the ID is random but is not a secret.
type Invitation struct {
	ID          string
	FolderID    string
	FolderName  string
	InviteeID   string
	InviteeName string // display name
	InviterID   string
	InviterName string // display name
	Role        authz.AlbumRole
	Status      string // effective status, see InvitationPending
	CreatedAt   string
	ExpiresAt   string
	AcceptedAt  string // "" unless accepted
}

// Member is the owner (Role "OWNER") or a member of a folder.
type Member struct {
	UserID      string
	DisplayName string
	Role        string // "OWNER" or an authz.AlbumRole
	Active      bool   // the account can log in
	JoinedAt    string // created_at of the folder for the owner
}

// AlbumChange is a membership write made by an authenticated user.
type AlbumChange struct {
	FolderID  string // "" for AcceptInvitation (taken from the invitation)
	ActorID   string // users.user_id of the caller
	SessionID string // the caller's session, see NewFolder.SessionID
	// TargetID is the user (invitee, member, new owner) or the invitation
	// the change is about.
	TargetID string
	Role     authz.AlbumRole // CreateInvitation and SetMemberRole
	// Authorize re-checks the caller inside the write transaction, with the
	// account, session, owner and membership as stored at that moment. It is
	// required; its error aborts the change and is returned wrapped.
	Authorize Authorizer
	// BeforeCommit, if set, runs after every change is prepared, right
	// before COMMIT. Its error rolls everything back (tests inject faults).
	BeforeCommit func() error
}

// Membership errors.
var (
	ErrInvalidRole = errors.New("invalid album role")
	// ErrInviteeNotFound: no account with this ID can be invited (unknown,
	// disabled or unable to log in). The three are not told apart.
	ErrInviteeNotFound    = errors.New("invitee not found")
	ErrAlreadyMember      = errors.New("already owner or member")
	ErrInvitationPending  = errors.New("a pending invitation exists")
	ErrInvitationNotFound = errors.New("invitation not found")
	// ErrInvitationGone: accepted, revoked or expired; it cannot be used.
	ErrInvitationGone = errors.New("invitation no longer valid")
	// ErrNotInvitee: the invitation names another account.
	ErrNotInvitee     = errors.New("invitation is addressed to another account")
	ErrMemberNotFound = errors.New("member not found")
	ErrTargetIsOwner  = errors.New("target is the owner")
	// ErrNewOwnerInactive: the member chosen as new owner cannot log in.
	ErrNewOwnerInactive = errors.New("new owner account is not active")
)

// Memberships manages invitations, memberships, ownership transfer and
// archiving. Every write runs in one transaction that re-checks the caller
// (AlbumChange.Authorize) and records an audit_log row; either all of it is
// committed or none of it. Folders keep exactly one owner at all times.
type Memberships interface {
	// CreateInvitation invites TargetID with Role to FolderID, valid for
	// InvitationTTL. ErrInviteeNotFound, ErrAlreadyMember (the owner or a
	// member), ErrInvitationPending (one per folder and account).
	CreateInvitation(ctx context.Context, c AlbumChange) (Invitation, error)
	// ListFolderInvitations returns the folder's pending invitations.
	ListFolderInvitations(ctx context.Context, folderID string) ([]Invitation, error)
	// RevokeInvitation cancels invitation TargetID of FolderID.
	// ErrInvitationNotFound (unknown or of another folder), ErrInvitationGone.
	RevokeInvitation(ctx context.Context, c AlbumChange) error
	// ListInvitationsFor returns the pending invitations addressed to userID
	// for folders that are not archived.
	ListInvitationsFor(ctx context.Context, userID string) ([]Invitation, error)
	// AcceptInvitation makes the caller a member with the invited role and
	// returns the folder with the caller's new relation.
	// ErrInvitationNotFound, ErrNotInvitee, ErrInvitationGone (also when the
	// folder was archived or its owner changed), ErrAlreadyMember.
	AcceptInvitation(ctx context.Context, c AlbumChange) (FolderAccess, error)
	// ListMembers returns the owner first, then the members by join time.
	ListMembers(ctx context.Context, folderID string) ([]Member, error)
	// SetMemberRole changes member TargetID's role. ErrMemberNotFound,
	// ErrTargetIsOwner.
	SetMemberRole(ctx context.Context, c AlbumChange) (Member, error)
	// RemoveMember deletes member TargetID. ErrMemberNotFound, ErrTargetIsOwner.
	RemoveMember(ctx context.Context, c AlbumChange) error
	// TransferOwnership makes member TargetID the owner; the previous owner
	// becomes a CONTRIBUTOR and pending invitations are revoked.
	// ErrMemberNotFound, ErrTargetIsOwner (the caller), ErrNewOwnerInactive.
	TransferOwnership(ctx context.Context, c AlbumChange) error
	// ArchiveFolder hides the folder from everyone (data is kept) and revokes
	// its pending invitations.
	ArchiveFolder(ctx context.Context, c AlbumChange) error
}

// MetadataStore is safe for concurrent use. Write methods either apply all of
// their changes durably or none of them.
//
// Nothing here decides permissions. A caller serving a user decides with
// authz.Allowed on GetFolderAccess and passes an Authorizer to every write,
// so that the decision is repeated right before the commit.
type MetadataStore interface {
	// CreateFolder stores a new folder with a fresh random ID, owned by
	// req.OwnerID, which must be an active account.
	CreateFolder(ctx context.Context, req NewFolder) (Folder, error)
	// ListFolders returns all folders in unspecified order (never nil). It
	// is for maintenance and tests, never for answering a user.
	ListFolders(ctx context.Context) ([]Folder, error)
	// ListFoldersFor returns the folders userID owns or is a member of, with
	// the user's relation, oldest first (never nil).
	ListFoldersFor(ctx context.Context, userID string) ([]FolderAccess, error)
	// GetFolder returns ErrFolderNotFound for an unknown id.
	GetFolder(ctx context.Context, id string) (Folder, error)
	// GetFolderAccess returns the folder and userID's relation to it, which
	// may be none at all. It returns ErrFolderNotFound for an unknown or
	// archived folder.
	GetFolderAccess(ctx context.Context, folderID, userID string) (FolderAccess, error)
	// CommitUpload atomically registers the blob (if new) and a new asset,
	// links the asset to the folder and appends one EventAssetAdded event.
	// If the folder already holds an asset with the same bytes it changes
	// nothing and returns ResultAlreadyPresent with that asset. It returns
	// ErrFolderNotFound for an unknown folder.
	CommitUpload(ctx context.Context, req CommitRequest) (CommitResult, error)
	// ListEvents returns up to limit events with Seq > cursor, in Seq order.
	// It returns ErrFolderNotFound for an unknown folder and ErrInvalidLimit
	// for limit <= 0.
	ListEvents(ctx context.Context, folderID string, cursor int64, limit int) (EventPage, error)
	// GetFolderAsset returns the asset with this ID if it is currently linked
	// to the folder, and ErrAssetNotFound otherwise (unknown ID, removed, or
	// an asset of another folder).
	GetFolderAsset(ctx context.Context, folderID, assetID string) (Asset, error)
	// GetAssetBySHA returns the oldest asset whose original has this hash,
	// in any folder, or ErrAssetNotFound. It serves deduplication inside the
	// server; its result must never be shown to a user.
	GetAssetBySHA(ctx context.Context, sha string) (Asset, error)
	// Close releases resources. The store must not be used afterwards.
	Close() error
}
