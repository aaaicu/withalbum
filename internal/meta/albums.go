package meta

import (
	"context"
	"errors"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
)

// Album management (migration 0017): deleted albums, the album trash and
// hidden assets.
//
// A deleted album is hidden from everyone (every route answers it like an
// unknown folder) and can be restored by its owner for TrashRetention;
// then PurgeDeletedFolder removes its rows and the originals no other
// asset holds. An asset removed from an album goes to the album's trash:
// it leaves the album for everyone (asset_removed) but keeps its likes
// and comments and can be restored by the album's OWNER or a CONTRIBUTOR
// for TrashRetention; then PurgeTrash deletes its likes and comments and
// the removal is final (the original stays on disk, as before).
//
// A hidden asset is seen only by those who may see hidden assets
// (authz.SeeHiddenAssets: OWNER, CONTRIBUTOR). Everyone else who reads the
// album (VIEWER) gets every read as if it were not in the album: snapshot,
// feed, downloads, likes, comments and activity.

// TrashRetention is how long a deleted album or a removed asset can be
// restored before it is purged.
const TrashRetention = 30 * 24 * time.Hour

// View is what one reader of a folder may see of its assets. The zero
// value is the narrower one.
type View uint8

const (
	// ViewVisible: hidden assets are left out (VIEWER).
	ViewVisible View = iota
	// ViewAll: hidden assets are included, marked Asset.Hidden (OWNER,
	// CONTRIBUTOR).
	ViewAll
)

// ViewOf is the view of principal p of a folder with access, decided with
// authz.Allowed (SeeHiddenAssets).
func ViewOf(p authz.Principal, access authz.AlbumAccess) View {
	if authz.Allowed(p, authz.SeeHiddenAssets, &access) {
		return ViewAll
	}
	return ViewVisible
}

// AssetQuery selects the assets of a snapshot page (ListFolderAssets).
type AssetQuery struct {
	View View
	// LikedBy, if set, keeps only the assets this account likes in the
	// folder.
	LikedBy string
}

// Album management errors.
var (
	// ErrAssetConflict: the folder holds the same original again (another
	// asset added since the removal), so the removed one is not restored.
	ErrAssetConflict = errors.New("the folder holds this original again")
	// ErrFolderNotDeleted: RestoreFolder on a folder that is not deleted.
	ErrFolderNotDeleted = errors.New("folder is not deleted")
)

// TrashedAsset is an asset in a folder's trash.
type TrashedAsset struct {
	Asset
	RemovedAt string
	// RemovedByName is the display name of who removed it while that
	// account can log in, "" otherwise.
	RemovedByName string
	// PurgeAfter is when it is purged (RemovedAt + TrashRetention).
	PurgeAfter string
}

// TrashPage is one page of a folder's trash, most recently removed first.
type TrashPage struct {
	Assets  []TrashedAsset // never nil
	HasMore bool
	Next    SocialCursor // position of the last asset (At: RemovedAt, ID: asset ID)
}

// DeletedFolder is a deleted folder as its owner sees it.
type DeletedFolder struct {
	ID         string
	Name       string
	CreatedAt  string
	DeletedAt  string
	PurgeAfter string // DeletedAt + TrashRetention
}

// PurgedFolder is what PurgeDeletedFolder removed. Blobs are the originals
// whose rows it deleted: their pool files were handed to Detach before the
// commit; the caller deletes them afterwards with their derived files.
// Uploads are the upload sessions of the folder, whose partial files the
// caller deletes.
type PurgedFolder struct {
	FolderID string
	Assets   int
	Blobs    []string
	Uploads  []string
}

// DetachFunc moves the pool files of originals whose rows a purge is about
// to delete out of the pool, inside the purge's transaction, right before
// COMMIT. If it fails nothing is committed. If the commit then fails, the
// purge calls undo, which must put them back.
type DetachFunc func(shas []string) (undo func(), err error)

// AlbumManagement is a MetadataStore that keeps deleted albums, the album
// trash and hidden assets. Every write is one transaction that re-checks
// the caller (AlbumChange.Authorize) and writes an audit row, like
// Memberships.
type AlbumManagement interface {
	// DeleteFolder marks the folder deleted (hidden from everyone,
	// restorable by its owner) and revokes its pending invitations.
	DeleteFolder(ctx context.Context, c AlbumChange) error
	// RestoreFolder brings back c.FolderID if it is deleted, was deleted
	// less than TrashRetention ago and c.ActorID owns it; Authorize gets
	// the folder's access as if it were not deleted. It returns the folder
	// with the caller's relation. ErrFolderNotFound (unknown, not the
	// caller's, purged or too old), ErrFolderNotDeleted.
	RestoreFolder(ctx context.Context, c AlbumChange) (FolderAccess, error)
	// GetDeletedFolderAccess is GetFolderAccess for a folder that is
	// deleted and still restorable at now (deleted after now -
	// TrashRetention, not archived); any other folder is ErrFolderNotFound.
	GetDeletedFolderAccess(ctx context.Context, folderID, userID string, now time.Time) (FolderAccess, error)
	// ListDeletedFolders returns the folders ownerID owns that are deleted
	// and not yet due for purging, most recently deleted first.
	ListDeletedFolders(ctx context.Context, ownerID string, now time.Time) ([]DeletedFolder, error)
	// DeletedFoldersDue returns up to limit folders deleted before t.
	DeletedFoldersDue(ctx context.Context, before time.Time, limit int) ([]string, error)
	// PurgeDeletedFolder deletes folderID if it was deleted before t: its
	// rows (members, invitations, feed, links, likes, comments, receipts,
	// upload sessions), the assets no other folder links or references,
	// and the rows of the originals no remaining asset holds, whose pool
	// files detach moves away before COMMIT. Not deleted, or deleted
	// later: ErrFolderNotFound, nothing written.
	PurgeDeletedFolder(ctx context.Context, folderID string, before time.Time, detach DetachFunc) (PurgedFolder, error)
	// DeletedFolderCount is how many folders are deleted and not purged
	// (for administrators: a number only).
	DeletedFolderCount(ctx context.Context) (int, error)

	// SetAssetHidden hides or shows asset c.TargetID of c.FolderID and
	// returns it (ViewAll). When the state changes it appends the feed
	// event in the same transaction; the same state changes nothing.
	// ErrAssetNotFound.
	SetAssetHidden(ctx context.Context, c AlbumChange, hidden bool) (a Asset, changed bool, err error)
	// ListTrash returns up to limit assets of the folder's trash (removed,
	// not purged, removed after now - TrashRetention) before the cursor,
	// most recently removed first, hidden ones included.
	ListTrash(ctx context.Context, folderID string, now time.Time, before SocialCursor, limit int) (TrashPage, error)
	// GetTrashedAsset returns an asset of the folder's trash, or
	// ErrAssetNotFound.
	GetTrashedAsset(ctx context.Context, folderID, assetID string, now time.Time) (TrashedAsset, error)
	// RestoreAsset brings asset c.TargetID back from c.FolderID's trash
	// (with its likes, comments and hidden state) and appends
	// EventAssetAdded. ErrAssetNotFound (not in the trash), ErrAssetConflict.
	RestoreAsset(ctx context.Context, c AlbumChange) (Asset, error)
	// PurgeTrash makes the removals made before t final: it deletes the
	// likes and comments of those assets in their folders and marks them
	// purged, in batches. It returns how many it purged.
	PurgeTrash(ctx context.Context, before time.Time) (int, error)
}
