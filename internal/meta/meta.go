// Package meta defines the metadata store used by the HTTP API: folders,
// assets, folder links and per-folder change events.
//
// Implementations only store metadata. Blob files live in internal/blobstore,
// and callers must verify the storage volume before calling a write method.
package meta

import (
	"context"
	"errors"
)

// Folder is an album.
type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// Asset is one stored original. In this PoC its ID is the SHA-256 of the bytes.
type Asset struct {
	ID           string `json:"id"`
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
	ResultCreated        = "created"         // new asset, linked to the folder
	ResultLinked         = "linked"          // existing asset, newly linked to the folder
	ResultAlreadyPresent = "already_present" // already linked; nothing changed
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

// CommitRequest links an uploaded blob to a folder.
type CommitRequest struct {
	FolderID string
	DeviceID string
	// Asset is registered only if no asset with Asset.ID exists yet. The store
	// sets UploadedAt. The blob file must already be in place.
	Asset Asset
	// BeforeCommit, if set, is called after all changes are prepared and
	// immediately before they are made durable, while the store holds its
	// write lock. If it returns an error nothing is written and CommitUpload
	// returns an error wrapping it. It is not called when the result is
	// ResultAlreadyPresent (nothing to write). The HTTP API re-verifies the
	// storage volume and the pool file here.
	BeforeCommit func() error
}

// CommitResult reports what CommitUpload did. Asset is the stored asset, which
// is the existing one for ResultLinked and ResultAlreadyPresent.
type CommitResult struct {
	Result string
	Asset  Asset
}

// EventPage is one page of a folder's change feed.
type EventPage struct {
	Events     []Event          // never nil
	Assets     map[string]Asset // assets referenced by Events; never nil
	NextCursor int64            // last Seq in Events, or the input cursor if empty
	HasMore    bool             // events after NextCursor exist
}

// MetadataStore is safe for concurrent use. Write methods either apply all of
// their changes durably or none of them.
type MetadataStore interface {
	// CreateFolder stores a new folder with a fresh random ID.
	CreateFolder(ctx context.Context, name string) (Folder, error)
	// ListFolders returns all folders in unspecified order (never nil).
	ListFolders(ctx context.Context) ([]Folder, error)
	// GetFolder returns ErrFolderNotFound for an unknown id.
	GetFolder(ctx context.Context, id string) (Folder, error)
	// CommitUpload atomically registers the asset (if new), links it to the
	// folder and appends one EventAssetAdded event. If the asset is already
	// linked to the folder it changes nothing and returns ResultAlreadyPresent.
	// It returns ErrFolderNotFound for an unknown folder.
	CommitUpload(ctx context.Context, req CommitRequest) (CommitResult, error)
	// ListEvents returns up to limit events with Seq > cursor, in Seq order.
	// It returns ErrFolderNotFound for an unknown folder and ErrInvalidLimit
	// for limit <= 0.
	ListEvents(ctx context.Context, folderID string, cursor int64, limit int) (EventPage, error)
	// GetAssetBySHA returns ErrAssetNotFound for an unknown hash.
	GetAssetBySHA(ctx context.Context, sha string) (Asset, error)
	// Close releases resources. The store must not be used afterwards.
	Close() error
}
