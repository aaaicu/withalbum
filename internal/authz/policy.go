// Package authz defines authorization decisions for the future WithAlbum server.
// IMPORTANT: This package is not yet wired into the server HTTP handlers.
package authz

// ServerRole is the scope of the account on one self-hosted server instance.
type ServerRole string

const (
	ServerAdmin ServerRole = "SERVER_ADMIN"
	ServerUser  ServerRole = "USER"
)

// AlbumRole is only defined for invitees. The owner is stored separately in Folder.OwnerID.
type AlbumRole string

const (
	Contributor AlbumRole = "CONTRIBUTOR" // read and upload, cannot manage other users or delete assets
	Viewer      AlbumRole = "VIEWER"      // read only
	Uploader    AlbumRole = "UPLOADER"    // upload only; cannot list or download even its own uploads remotely
)

// Action is a server-side operation, never a UI visibility hint.
type Action string

const (
	ManageAccounts   Action = "manage_accounts"
	ManageServer     Action = "manage_server"
	GrantCreation    Action = "grant_album_creation"
	CreateAlbum      Action = "create_album"
	GetAlbumMetadata Action = "get_album_metadata"
	ListAlbumAssets  Action = "list_album_assets"
	DownloadAsset    Action = "download_asset"
	UploadAsset      Action = "upload_asset"
	DeleteAsset      Action = "delete_asset"
	InviteMember     Action = "invite_member"
	RevokeMember     Action = "revoke_member"
	ChangeMemberRole Action = "change_member_role"
	TransferOwner    Action = "transfer_owner"
	DeleteAlbum      Action = "delete_album"
)

// Principal is derived from the authenticated session and database, never request body fields.
type Principal struct {
	UserID          string
	Active          bool
	ServerRole      ServerRole
	CanCreateAlbums bool
}

// AlbumAccess comes from a database lookup for the requested album + user.
// MemberRole must be ignored when HasMembership=false.
type AlbumAccess struct {
	OwnerID       string
	HasMembership bool
	MemberRole    AlbumRole
}

// Allowed evaluates a single authorization decision. Call for EVERY protected HTTP operation.
// In particular, SERVER_ADMIN does NOT automatically get rights to private album content.
func Allowed(user Principal, action Action, album *AlbumAccess) bool {
	if user.UserID == "" || !user.Active {
		return false
	}
	if action == ManageAccounts || action == ManageServer || action == GrantCreation {
		return user.ServerRole == ServerAdmin
	}
	if action == CreateAlbum {
		return user.ServerRole == ServerAdmin || user.CanCreateAlbums
	}
	if album == nil || album.OwnerID == "" {
		return false
	}
	if album.OwnerID == user.UserID {
		switch action {
		case GetAlbumMetadata, ListAlbumAssets, DownloadAsset, UploadAsset,
			DeleteAsset, InviteMember, RevokeMember, ChangeMemberRole,
			TransferOwner, DeleteAlbum:
			return true
		default:
			return false
		}
	}
	if !album.HasMembership {
		return false
	}
	switch action {
	case GetAlbumMetadata:
		return album.MemberRole == Contributor || album.MemberRole == Viewer || album.MemberRole == Uploader
	case ListAlbumAssets, DownloadAsset:
		return album.MemberRole == Contributor || album.MemberRole == Viewer
	case UploadAsset:
		return album.MemberRole == Contributor || album.MemberRole == Uploader
	default:
		return false
	}
}
