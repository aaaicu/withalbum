// Package authz defines authorization decisions for the WithAlbum server.
// Every protected HTTP route decides with Allowed (internal/httpapi).
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
	Contributor AlbumRole = "CONTRIBUTOR" // read, upload, and manage the album's assets (delete, restore, hide); cannot manage members
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
	// DeleteAsset: move any asset of the album to its trash, list the
	// trash and restore from it (OWNER, CONTRIBUTOR).
	DeleteAsset      Action = "delete_asset"
	InviteMember     Action = "invite_member"
	RevokeMember     Action = "revoke_member"
	ChangeMemberRole Action = "change_member_role"
	// ListMembers: the album's owner and members (every role that can
	// read the album: OWNER, CONTRIBUTOR, VIEWER).
	ListMembers Action = "list_members"
	// ListInvitations: the album's pending invitations (OWNER).
	ListInvitations Action = "list_invitations"
	TransferOwner   Action = "transfer_owner"
	// DeleteAlbum: delete (restorable for 30 days), restore or archive the
	// album (OWNER).
	DeleteAlbum Action = "delete_album"
	// UpdateAlbumSettings: the owner's album settings (hide_location).
	UpdateAlbumSettings Action = "update_album_settings"
	// ReactToAsset: like an asset and write, edit or delete one's own
	// comments. Every role that can read the album (OWNER, CONTRIBUTOR,
	// VIEWER); never an UPLOADER, which cannot see the album's contents.
	ReactToAsset Action = "react_to_asset"
	// ModerateComments: delete anyone's comment in the album (OWNER).
	ModerateComments Action = "moderate_comments"
	// HideAsset: mark an asset of the album hidden or visible again
	// (OWNER, CONTRIBUTOR).
	HideAsset Action = "hide_asset"
	// SeeHiddenAssets: see the album's hidden assets (OWNER, CONTRIBUTOR).
	// Everyone else who can read the album (VIEWER) is shown the album
	// without them.
	SeeHiddenAssets Action = "see_hidden_assets"
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
			ListMembers, ListInvitations, TransferOwner, DeleteAlbum, UpdateAlbumSettings,
			ReactToAsset, ModerateComments, HideAsset, SeeHiddenAssets:
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
	case ListAlbumAssets, DownloadAsset, ReactToAsset, ListMembers:
		return album.MemberRole == Contributor || album.MemberRole == Viewer
	case UploadAsset:
		return album.MemberRole == Contributor || album.MemberRole == Uploader
	case DeleteAsset, HideAsset, SeeHiddenAssets:
		return album.MemberRole == Contributor
	default:
		return false
	}
}
