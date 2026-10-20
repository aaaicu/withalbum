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
	// CapturedAt is when the photo or video was taken, as read from the
	// original by the server (capturetime.Time.String), or "" when it is
	// unknown or not read yet. Never the upload time.
	CapturedAt string `json:"-"`
	// DurationMS is the playing time of the original in milliseconds, as
	// read from its movie header (capturetime.Duration), or 0 when it is
	// not a movie, unknown or not read yet.
	DurationMS int64 `json:"-"`
	// UploaderName is the display name of the account that uploaded the
	// asset, or "" when there is none or the account is disabled or cannot
	// log in. Read only; never the login name, user_id or e-mail.
	UploaderName string `json:"-"`
	// MediaKind is assets.media_kind (MediaImage, MediaVideo,
	// MediaLivePhoto, ...); "" for legacy data that has none.
	MediaKind string `json:"-"`
	// Pairing is how a Live Photo's still and video were matched
	// (PairingVerified, PairingUnverified); "" for other assets.
	Pairing string `json:"-"`
	// PairedVideo is the paired video of a Live Photo (Role
	// RolePairedVideo); the zero Component when the asset has none. The
	// asset's own SHA256, MimeType and Bytes are always the still.
	PairedVideo Component `json:"-"`
}

// Component is an extra original of an asset (asset_components), stored
// as its own blob. The asset's primary original (the still of a Live
// Photo) is Asset.SHA256, not a Component.
type Component struct {
	Role     string // RolePairedVideo
	SHA256   string
	Filename string
	MimeType string
	Bytes    int64
	// DurationMS: as Asset.DurationMS, for this original.
	DurationMS int64
}

// Media kinds (assets.media_kind).
const (
	MediaImage     = "image"
	MediaVideo     = "video"
	MediaLivePhoto = "live_photo" // a still (the asset's original) and its paired video
	MediaOther     = "other"
)

// Component roles. RoleStill names the primary original of a bundle (the
// asset's own original); RolePairedVideo is stored as a Component.
const (
	RoleStill       = "still"
	RolePairedVideo = "paired_video"
)

// Pairing states of a Live Photo (assets.pairing_state).
const (
	// PairingVerified: both originals carry the same Apple content
	// identifier.
	PairingVerified = "verified"
	// PairingUnverified: at least one carries none the server could read.
	PairingUnverified = "unverified"
)

// CaptureResult is what the capture-time worker read from one original
// (SetCaptureTimes). CapturedAt "" records that the original has no
// readable capture time, DurationMS 0 that it has no readable duration.
type CaptureResult struct {
	SHA256      string
	CapturedAt  string // capturetime.Time.String
	CapturedUTC string // the UTC instant (RFC 3339, "Z"), or "" without one
	DurationMS  int64  // capturetime.Duration
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
	// EventAssetUpdated: the asset's metadata changed (its preview status,
	// written by SetPreviewStatus, or its capture time or duration,
	// written by SetCaptureTimes); read it again. Its
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
	Result    string // ResultCreated, ResultAlreadyPresent or (bundles) ResultUpgraded
	AssetID   string
	CreatedAt string
	// PairedSHA256 is the paired video of a bundle's receipt, "" for a
	// single original.
	PairedSHA256 string
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
	// ResultUpgraded (bundles): the folder held the still as a still-only
	// asset; the paired video was attached to that asset.
	ResultUpgraded = "upgraded"
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
	// ErrBundleNotFound: no upload bundle with this ID that belongs to this
	// account, device and folder and has not expired (or, for a commit, it
	// is no longer open).
	ErrBundleNotFound = errors.New("upload bundle not found")
	// ErrComponentConflict: the folder already holds the bundle's still as
	// an asset with another paired video (or one that cannot take this
	// one). Existing originals are never replaced; nothing was written.
	ErrComponentConflict = errors.New("the folder holds this still with another paired video")
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
	// BundleID and Role are set for a component session of an upload
	// bundle (Role RoleStill or RolePairedVideo). Such a session only takes
	// chunks; it is completed and cancelled through its bundle.
	BundleID string
	Role     string
}

// Upload bundle kinds (Bundle.Kind).
const BundleLivePhoto = "live_photo"

// MaxPairedVideoSize bounds the paired video of a Live Photo.
const MaxPairedVideoSize int64 = 512 << 20

// Bundle is an upload of several originals that become one asset: for
// BundleLivePhoto a still and its paired video. Each component is an
// upload session (chunks, resume) of the bundle; completing the bundle
// commits all of them in one transaction. Like an upload session it is
// visible only to its account, device and folder, and it expires with its
// components (UploadSessionTTL after the last change of any of them).
type Bundle struct {
	ID          string // random, 32 hex digits
	UserID      string
	DeviceID    string
	FolderID    string
	Key         string // the client's Idempotency-Key
	Kind        string // BundleLivePhoto
	Status      string // UploadOpen or UploadCompleted
	DeviceLabel string
	CreatedAt   string
	ExpiresAt   string
	// Components: the still first, then the paired video.
	Components []UploadSession
}

// Component returns the bundle's session of role.
func (b Bundle) Component(role string) (UploadSession, bool) {
	for _, c := range b.Components {
		if c.Role == role {
			return c, true
		}
	}
	return UploadSession{}, false
}

// NewBundleComponent declares one original of a new bundle.
type NewBundleComponent struct {
	Role     string // RoleStill or RolePairedVideo
	SHA256   string
	Bytes    int64
	Filename string
}

// NewBundle is a CreateBundle request. Its fields are those of
// NewUploadSession; Components must hold exactly one still and one paired
// video.
type NewBundle struct {
	UserID      string
	SessionID   string
	DeviceID    string
	FolderID    string
	Key         string
	Kind        string
	DeviceLabel string
	Components  []NewBundleComponent
	// Authorize re-checks the caller inside the write transaction (required).
	Authorize Authorizer
	// Reserve: as NewUploadSession.Reserve, for the components together.
	Reserve func(reserved int64) error
}

// BundleRef names an upload bundle as seen by its owner.
type BundleRef struct {
	ID       string
	UserID   string
	DeviceID string
	FolderID string
}

// BundleCommit commits a bundle whose components are complete, verified
// and in the pool.
type BundleCommit struct {
	Ref         BundleRef
	SessionID   string // the caller's auth session, see NewFolder.SessionID
	DeviceLabel string // X-Device-ID label, recorded as uploader device
	// Still is the asset's original (SHA256, Filename, MimeType, Bytes,
	// PreviewReady, PreviewStatus); Video the paired video (SHA256,
	// Filename, MimeType, Bytes).
	Still Asset
	Video Component
	// Pairing (PairingVerified or PairingUnverified) and PairingID (the
	// content identifier, "" when unknown) are recorded on a new or
	// upgraded asset.
	Pairing   string
	PairingID string
	// Authorize re-checks the caller inside the write transaction
	// (required); it is called for every outcome, a replay included.
	Authorize Authorizer
	// AddOnConflict, if set, is called after Authorize. When it returns
	// true, a still the folder already holds with another paired video (or
	// in an asset that other folders share) is stored as a new asset
	// instead of failing with ErrComponentConflict: an upload-only caller
	// must not learn what the folder holds, and its bytes are kept.
	AddOnConflict func() bool
	// BeforeCommit: as CommitRequest.BeforeCommit (not called for a
	// replay).
	BeforeCommit func() error
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
	// MemberCount is the owner and the members (every role) whose accounts
	// are active and can log in. Pending invitations are not counted. The
	// HTTP API shows it only to callers who may read the folder.
	MemberCount int
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

// FolderMatch is an asset FindFolderAssets found.
type FolderMatch struct {
	AssetID string
	// PairedSHA256 is the asset's paired video ("" when it has none), so
	// that a client can tell a still-only asset it may complete with a
	// Live Photo bundle.
	PairedSHA256 string
}

// StorageUsage compares the originals the albums hold with what the pool
// stores. Every SHA-256 is stored once, however many albums (or accounts)
// hold it, so AlbumBytes can be larger than StoredBytes. Totals only:
// nothing about any one album.
type StorageUsage struct {
	StoredOriginals int64 // originals in the pool (blobs rows)
	StoredBytes     int64 // their bytes
	// AlbumCopies counts originals as the albums hold them: one per asset
	// currently in a folder, plus one for its paired video, archived
	// folders included (their data is kept).
	AlbumCopies int64
	AlbumBytes  int64 // the bytes of AlbumCopies, as if each were its own file
	// SavedBytes is AlbumBytes minus the bytes of the distinct originals
	// those copies use: what storing each SHA-256 once saves.
	SavedBytes int64
}

// StorageUsageReporter is a MetadataStore that can report StorageUsage.
type StorageUsageReporter interface {
	StorageUsage(ctx context.Context) (StorageUsage, error)
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
	// CreateFolderOnce is CreateFolder for a request named by an
	// idempotency key of req.OwnerID's device id.DeviceID. In the same
	// transaction it records the key (folder_create_receipts), or, if the
	// key is recorded already, creates nothing and returns that folder
	// with replayed true (after req.Authorize, like a new request). A key
	// recorded with another name is ErrIdempotencyMismatch. Concurrent
	// requests with one key create one folder.
	CreateFolderOnce(ctx context.Context, req NewFolder, id Idempotency) (f Folder, replayed bool, err error)
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
	// currently holds as an asset's original, that asset there (by its
	// smallest ID). It never looks at other folders.
	FindFolderAssets(ctx context.Context, folderID string, shas []string) (map[string]FolderMatch, error)
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
	// PruneUploadReceipts deletes receipts created before t: upload
	// receipts and folder creation receipts (CreateFolderOnce).
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
	// PruneUploadSessions deletes the sessions and the upload bundles
	// (with their component sessions) that expired before t, and returns
	// the IDs of the deleted sessions (the caller deletes their files
	// afterwards).
	PruneUploadSessions(ctx context.Context, t time.Time) ([]string, error)
	// CreateBundle starts an upload bundle with one upload session per
	// component, or returns the existing one of the same (account, device,
	// folder, key) with created false when it declares the same originals
	// (a retried creation; a completed bundle included). An expired bundle
	// of the key is replaced; the IDs of its sessions are returned in
	// replaced so the caller can delete their files. ErrIdempotencyMismatch
	// (the key names other originals, a single upload or a session),
	// *UploadCommittedError (the key's receipt says these originals were
	// committed), ErrTooManyUploads (counting every component),
	// ErrFolderNotFound, and the errors of Authorize and Reserve, wrapped.
	CreateBundle(ctx context.Context, req NewBundle) (b Bundle, created bool, replaced []string, err error)
	// GetBundle returns the bundle and its component sessions if it
	// belongs to ref's account, device and folder and has not expired,
	// ErrBundleNotFound otherwise.
	GetBundle(ctx context.Context, ref BundleRef) (Bundle, error)
	// DeleteBundle removes the bundle ref (open or completed) and its
	// component sessions, and returns their IDs. ErrBundleNotFound.
	DeleteBundle(ctx context.Context, ref BundleRef) ([]string, error)
	// CommitBundle commits an open bundle in one transaction: it re-checks
	// the caller, then, by the folder's assets that are not removed:
	//   - no asset has the still: a new asset (media kind live photo, the
	//     video as its paired_video component), EventAssetAdded,
	//     ResultCreated;
	//   - an asset has the still and no paired video, and no other folder
	//     shares it: the video is attached, its media kind becomes live
	//     photo, EventAssetUpdated and an audit row, ResultUpgraded;
	//   - an asset has the still and the same video: ResultAlreadyPresent;
	//   - otherwise ErrComponentConflict (or a new asset, see
	//     BundleCommit.AddOnConflict).
	// Every outcome records the receipt (with the paired video) and marks
	// the bundle and its sessions completed. A receipt of the bundle's key
	// with the same originals is replayed (Replayed set, nothing written
	// but the bundle's completion); with other originals, or one of a
	// single upload, ErrIdempotencyMismatch. ErrBundleNotFound when the
	// bundle is not open.
	CommitBundle(ctx context.Context, req BundleCommit) (CommitResult, error)
	// PendingPreviews returns up to limit originals with assets whose
	// preview is pending, oldest first.
	PendingPreviews(ctx context.Context, limit int) ([]PreviewJob, error)
	// SetPreviewStatus sets the preview status of every asset of the
	// original sha whose preview is pending to status (PreviewStatusReady,
	// PreviewStatusFailed or PreviewStatusUnsupported) and appends one
	// EventAssetUpdated to each folder that holds one of them, in one
	// transaction. It returns the number of assets changed.
	SetPreviewStatus(ctx context.Context, sha, status string) (int, error)
	// PendingCaptures returns up to limit originals whose capture time or
	// duration has not been read yet, by SHA-256.
	PendingCaptures(ctx context.Context, limit int) ([]string, error)
	// SetCaptureTimes records what the capture-time worker read, in one
	// transaction: the capture time of the originals whose capture time is
	// still pending and the duration of those whose duration is still
	// pending (other values are left as they are). For each original whose
	// newly recorded capture time or duration is known it appends one
	// EventAssetUpdated to every folder holding an asset of it as its
	// original, and for a newly known duration also to every folder holding
	// an asset with it as a component (a Live Photo's paired video); an
	// original with nothing known changes nothing a client sees, so it gets
	// no event. It returns the number of events appended.
	SetCaptureTimes(ctx context.Context, results []CaptureResult) (int, error)
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
