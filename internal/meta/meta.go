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
	// PreviewStatus is one of the PreviewStatus* constants; PreviewReady
	// is PreviewStatus == PreviewStatusReady. Not part of the PoC format.
	PreviewStatus string `json:"-"`
	UploadedAt    string `json:"uploaded_at"`
}

// Preview states of an asset (Asset.PreviewStatus).
const (
	PreviewStatusReady       = "ready"       // the JPEG preview exists
	PreviewStatusPending     = "pending"     // waiting for the preview worker
	PreviewStatusFailed      = "failed"      // the converter could not make one
	PreviewStatusUnsupported = "unsupported" // no converter for this type on this server
)

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

// Event kinds. EventAssetRemoved is a tombstone: the asset is no longer in
// the folder (its original stays on disk; other folders are not affected).
const (
	EventAssetAdded   = "asset_added"   // written by CommitUpload
	EventAssetRemoved = "asset_removed" // written by RemoveAsset
	// EventAssetUpdated: the asset's metadata changed (currently only its
	// preview status, written by SetPreviewStatus); read it again. Its
	// UploaderDeviceID is that of the asset's asset_added event in the same
	// folder (the server made the change, no device did).
	EventAssetUpdated = "asset_updated"
)

// FeedState locates a folder's change feed. Epoch identifies the feed of
// this folder in this database: it differs between folders and changes
// when the database is restored from a backup, so a sync cursor carrying
// another epoch does not belong to this feed. Events with Seq <= PrunedSeq
// may no longer be kept (retention; nothing prunes events yet).
type FeedState struct {
	Epoch     string
	LastSeq   int64
	PrunedSeq int64
}

// Idempotency names one upload request of a client: the session's device
// and the client's Idempotency-Key. Together with CommitRequest.UserID and
// FolderID it identifies the request; keys of other accounts, devices or
// folders never match.
type Idempotency struct {
	DeviceID string // devices.device_id of the uploading session
	Key      string
}

// Receipt is the stored outcome of a committed upload request.
type Receipt struct {
	SHA256    string
	Bytes     int64
	Result    string // ResultCreated or ResultAlreadyPresent
	AssetID   string
	CreatedAt string
}

// ReceiptTTL is how long a receipt is kept (PruneUploadReceipts). A retry
// after that is a new request; the content is still deduplicated
// (ResultAlreadyPresent), so nothing is stored twice.
const ReceiptTTL = 30 * 24 * time.Hour

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
	// ErrIdempotencyMismatch: the idempotency key was already used by this
	// account and device for this folder with other bytes.
	ErrIdempotencyMismatch = errors.New("idempotency key already used for other content")
	ErrReceiptNotFound     = errors.New("upload receipt not found")
	// ErrCursorInvalid: a sync position beyond the folder's last event.
	ErrCursorInvalid = errors.New("sync cursor is not a position of this feed")
	// ErrBusy means a write could not get the database write lock in time
	// (concurrent writers). Nothing was written; retrying later is safe.
	ErrBusy = errors.New("metadata store is busy")
	// ErrUploadNotFound: no upload session with this ID that belongs to this
	// account, device and folder and has not expired.
	ErrUploadNotFound = errors.New("upload session not found")
	// ErrUploadConflict: the session is not at the offset the write assumed
	// (another request moved it), or it is no longer open.
	ErrUploadConflict = errors.New("upload session changed")
	// ErrUploadCommitted: the idempotency key of a new session was already
	// committed with the same bytes (by a single request or a session that
	// has since been pruned). CreateUploadSession returns it as an
	// *UploadCommittedError carrying the stored outcome.
	ErrUploadCommitted = errors.New("upload already committed")
	// ErrTooManyUploads: the account has MaxOpenUploads open sessions.
	ErrTooManyUploads = errors.New("too many open upload sessions")
)

// UploadCommittedError is ErrUploadCommitted with the stored outcome of the
// committed request, as a replay of it: Result is what CommitUpload would
// answer for the same request again (Replayed set, Asset as it is now). It
// is read in the transaction that found the receipt, after Authorize.
type UploadCommittedError struct {
	Result CommitResult
}

func (e *UploadCommittedError) Error() string { return ErrUploadCommitted.Error() }
func (e *UploadCommittedError) Unwrap() error { return ErrUploadCommitted }

// Upload session states (UploadSession.Status).
const (
	UploadOpen      = "open"
	UploadCompleted = "completed"
)

// UploadSessionTTL is how long an upload session lives after its last
// change (creation, an appended chunk, completion).
const UploadSessionTTL = 24 * time.Hour

// MaxOpenUploads bounds the open upload sessions of one account.
const MaxOpenUploads = 16

// UploadSession is a resumable upload of one original in chunks. It is
// visible only to its account, its device and its folder.
type UploadSession struct {
	ID          string // random, 32 hex digits
	UserID      string
	DeviceID    string // devices.device_id of the creating session
	FolderID    string
	Key         string // Idempotency-Key
	SHA256      string // declared SHA-256 of the whole original
	TotalBytes  int64
	Received    int64 // bytes durably appended so far
	Filename    string
	DeviceLabel string // X-Device-ID label, recorded at commit
	Status      string // UploadOpen or UploadCompleted
	CreatedAt   string
	ExpiresAt   string
}

// NewUploadSession is a CreateUploadSession request.
type NewUploadSession struct {
	UserID      string
	SessionID   string // the caller's auth session, see NewFolder.SessionID
	DeviceID    string
	FolderID    string
	Key         string
	SHA256      string
	TotalBytes  int64
	Filename    string
	DeviceLabel string
	// Authorize re-checks the caller inside the write transaction (required).
	Authorize Authorizer
	// Reserve is called inside the write transaction with the bytes that
	// other open sessions still expect; an error refuses the new session
	// (the HTTP API answers 507 when the volume cannot hold them and this
	// one). Required.
	Reserve func(reserved int64) error
}

// UploadRef names an upload session as seen by its owner.
type UploadRef struct {
	ID       string
	UserID   string
	DeviceID string
	FolderID string
}

// PreviewJob is an original whose assets wait for a preview.
type PreviewJob struct {
	SHA256   string
	MimeType string
}

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
	// returns an error wrapping it. It is not called when nothing is
	// written: ResultAlreadyPresent without Idempotency, or a replay. The
	// HTTP API re-verifies the storage volume and the pool file here.
	BeforeCommit func() error
	// Idempotency, if set (UserID must be set too), records a receipt in the
	// same transaction, for every outcome including ResultAlreadyPresent.
	// If a receipt for the same request exists, nothing is written and its
	// result is returned with Replayed set (after Authorize); if that receipt
	// is for other bytes, CommitUpload returns ErrIdempotencyMismatch.
	Idempotency *Idempotency
	// UploadSessionID, if set, is the open upload session (of UserID and
	// Idempotency.DeviceID, for FolderID) these bytes came from: the same
	// transaction marks it completed, for every outcome including a replay.
	// If it is not open, CommitUpload returns ErrUploadNotFound and writes
	// nothing.
	UploadSessionID string
}

// CommitResult reports what CommitUpload did. Asset is the stored asset: the
// new one, or the folder's existing one for ResultAlreadyPresent.
type CommitResult struct {
	Result   string
	Asset    Asset
	Replayed bool // the stored result of an earlier request with the same idempotency key
}

// EventPage is one page of a folder's change feed.
type EventPage struct {
	Events []Event // never nil
	// Assets holds the assets of the asset_added and asset_updated events in
	// Events that are still linked to the folder, by Asset.ID, as they are
	// now; never nil. An asset that was removed since is left out (its
	// asset_removed event follows).
	Assets     map[string]Asset
	NextCursor int64     // last Seq in Events, or the input cursor if empty
	HasMore    bool      // events after NextCursor exist
	State      FeedState // read before the events
}

// AssetPage is one page of the assets currently linked to a folder, in
// Asset.ID order.
type AssetPage struct {
	Assets  []Asset   // never nil
	HasMore bool      // assets after the last one exist
	State   FeedState // read before the assets
}

// Invitation statuses (folder_invitations.status). A pending invitation
// whose ExpiresAt has passed is reported as InvitationExpired; the stored
// row may still say pending.
const (
	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationRevoked  = "revoked"
	InvitationExpired  = "expired"
	InvitationDeclined = "declined" // by the invitee
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
	FolderID  string // "" for Accept/DeclineInvitation (taken from the invitation)
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
	// ErrInvitationGone: accepted, revoked, expired or declined; it cannot
	// be used.
	ErrInvitationGone = errors.New("invitation no longer valid")
	// ErrNotInvitee: the invitation names another account.
	ErrNotInvitee     = errors.New("invitation is addressed to another account")
	ErrMemberNotFound = errors.New("member not found")
	ErrTargetIsOwner  = errors.New("target is the owner")
	// ErrNewOwnerInactive: the member chosen as new owner cannot log in.
	ErrNewOwnerInactive = errors.New("new owner account is not active")
	// ErrNewOwnerNotFound: no account has the ID chosen as new owner.
	ErrNewOwnerNotFound = errors.New("new owner account not found")
	// ErrFolderNotArchived: the folder exists but is not archived, so an
	// administrator may not reassign it (only its owner can transfer it).
	ErrFolderNotArchived = errors.New("folder is not archived")
)

// OwnedFolders is what an administrator may know about the folders an
// account owns: how many are in use, and the IDs of the archived ones.
// Never names, members or contents.
type OwnedFolders struct {
	Active   int
	Archived []ArchivedFolder
}

// ArchivedFolder is an archived folder as an administrator sees it.
type ArchivedFolder struct {
	ID         string
	CreatedAt  string
	ArchivedAt string
}

// Reassignment is the result of ReassignArchivedOwner.
type Reassignment struct {
	FolderID        string
	PreviousOwnerID string
	NewOwnerID      string
	ArchivedAt      string
}

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
	// DeclineInvitation marks invitation TargetID declined, as the caller.
	// It is decided like AcceptInvitation: ErrInvitationNotFound,
	// ErrNotInvitee, ErrInvitationGone (also when the folder was archived
	// or its owner changed). Nothing is written unless it is declined; a
	// declined invitation is no longer pending, so the owner may invite the
	// same account again.
	DeclineInvitation(ctx context.Context, c AlbumChange) error
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
	// OwnedFolders reports the folders userID owns (for account
	// administration: an account that owns folders cannot be disabled).
	OwnedFolders(ctx context.Context, userID string) (OwnedFolders, error)
	// ReassignArchivedOwner makes TargetID the owner of the archived folder
	// FolderID, so that its previous owner's account can be disabled. It is
	// an administrator's operation: Authorize decides on the caller alone
	// (the AlbumAccess passed is the zero value). The folder stays archived,
	// so the new owner gains no access to it; the previous owner does not
	// become a member, a membership of the new owner is dropped and pending
	// invitations are revoked. ErrFolderNotFound (unknown),
	// ErrFolderNotArchived, ErrTargetIsOwner, ErrNewOwnerNotFound,
	// ErrNewOwnerInactive.
	ReassignArchivedOwner(ctx context.Context, c AlbumChange) (Reassignment, error)
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
	// FeedState returns the folder's feed position. ErrFolderNotFound.
	FeedState(ctx context.Context, folderID string) (FeedState, error)
	// ListFolderAssets returns up to limit assets currently linked to the
	// folder with Asset.ID > after, in Asset.ID order (a snapshot page).
	// ErrFolderNotFound, ErrInvalidLimit.
	ListFolderAssets(ctx context.Context, folderID, after string, limit int) (AssetPage, error)
	// FindFolderAssets returns, for each given SHA-256 that the folder
	// currently holds, the ID of its asset there. It never looks at other
	// folders.
	FindFolderAssets(ctx context.Context, folderID string, shas []string) (map[string]string, error)
	// RemoveAsset unlinks asset c.TargetID from c.FolderID and appends an
	// EventAssetRemoved event, re-checking the caller (c.Authorize) and
	// writing an audit row in the same transaction. The asset row and the
	// original file are kept. ErrAssetNotFound (unknown, removed already,
	// or of another folder).
	RemoveAsset(ctx context.Context, c AlbumChange) error
	// AckSyncCursor records that deviceID has applied c.FolderID's feed up
	// to seq (device_sync_cursors), after re-checking the caller.
	// ErrCursorInvalid when seq is beyond the last event.
	AckSyncCursor(ctx context.Context, c AlbumChange, deviceID string, seq int64) error
	// LookupReceipt returns the receipt of an upload request, or
	// ErrReceiptNotFound. It is a read; CommitUpload decides again.
	LookupReceipt(ctx context.Context, userID, folderID string, id Idempotency) (Receipt, error)
	// PruneUploadReceipts deletes receipts created before t.
	PruneUploadReceipts(ctx context.Context, before time.Time) (int64, error)
	// CreateUploadSession starts an upload session, or returns the existing
	// one of the same (account, device, folder, key) with created false when
	// it has the same SHA-256 and size (a retried creation; a completed
	// session included). An expired session of the key is replaced; its ID
	// is returned in replaced so the caller can delete its file.
	// ErrIdempotencyMismatch (the key names other bytes, in a session or a
	// receipt), *UploadCommittedError (ErrUploadCommitted: the key's receipt
	// says these bytes were committed), ErrTooManyUploads, ErrFolderNotFound,
	// and the errors of Authorize and Reserve, wrapped.
	CreateUploadSession(ctx context.Context, req NewUploadSession) (us UploadSession, created bool, replaced string, err error)
	// GetUploadSession returns the session if it belongs to ref's account,
	// device and folder and has not expired, ErrUploadNotFound otherwise.
	GetUploadSession(ctx context.Context, ref UploadRef) (UploadSession, error)
	// AdvanceUploadSession records that the open session ref has received
	// bytes up to to, if it is at from now, and extends its expiry.
	// ErrUploadNotFound, ErrUploadConflict.
	AdvanceUploadSession(ctx context.Context, ref UploadRef, from, to int64) error
	// DeleteUploadSession removes the session ref (open or completed).
	// ErrUploadNotFound.
	DeleteUploadSession(ctx context.Context, ref UploadRef) error
	// ReservedUploadBytes is what the open, unexpired sessions other than
	// exceptID still expect to receive.
	ReservedUploadBytes(ctx context.Context, exceptID string) (int64, error)
	// ListUploadSessions returns every session row, expired ones included
	// (startup reconciliation of the .part files).
	ListUploadSessions(ctx context.Context) ([]UploadSession, error)
	// PruneUploadSessions deletes the sessions that expired before t and
	// returns their IDs (the caller deletes their files afterwards).
	PruneUploadSessions(ctx context.Context, t time.Time) ([]string, error)
	// PendingPreviews returns up to limit originals with assets whose
	// preview is pending, oldest first.
	PendingPreviews(ctx context.Context, limit int) ([]PreviewJob, error)
	// SetPreviewStatus sets the preview status of every asset of the
	// original sha whose preview is pending to status (PreviewStatusReady,
	// PreviewStatusFailed or PreviewStatusUnsupported) and appends one
	// EventAssetUpdated to each folder that holds one of them, in one
	// transaction. It returns the number of assets changed.
	SetPreviewStatus(ctx context.Context, sha, status string) (int, error)
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
