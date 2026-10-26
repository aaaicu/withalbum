package authz

import "testing"

func TestGlobalPermissions(t *testing.T) {
	admin := Principal{UserID: "a", Active: true, ServerRole: ServerAdmin}
	user := Principal{UserID: "b", Active: true, ServerRole: ServerUser}
	creator := Principal{UserID: "c", Active: true, ServerRole: ServerUser, CanCreateAlbums: true}
	inactive := Principal{UserID: "d", Active: false, ServerRole: ServerAdmin, CanCreateAlbums: true}
	for _, tc := range []struct {
		name string
		p    Principal
		a    Action
		want bool
	}{
		{"admin-create", admin, CreateAlbum, true},
		{"user-cannot-create", user, CreateAlbum, false},
		{"creator-create", creator, CreateAlbum, true},
		{"user-cannot-manage", user, ManageAccounts, false},
		{"creator-cannot-manage", creator, ManageAccounts, false},
		{"admin-manage", admin, ManageAccounts, true},
		{"admin-grant", admin, GrantCreation, true},
		{"inactive-admin", inactive, CreateAlbum, false},
		{"inactive-grant", inactive, GrantCreation, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Allowed(tc.p, tc.a, nil); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFolderRoles(t *testing.T) {
	acts := []Action{GetAlbumMetadata, ListAlbumAssets, DownloadAsset, UploadAsset, DeleteAsset, InviteMember, RevokeMember, ChangeMemberRole, ListMembers, TransferOwner, DeleteAlbum, UpdateAlbumSettings, ReactToAsset, ModerateComments, ListInvitations, HideAsset, SeeHiddenAssets}
	roleCases := []struct {
		name   string
		user   Principal
		access AlbumAccess
		want   map[Action]bool
	}{
		{"owner", Principal{UserID: "owner", Active: true, ServerRole: ServerUser}, AlbumAccess{OwnerID: "owner"}, map[Action]bool{
			GetAlbumMetadata: true, ListAlbumAssets: true, DownloadAsset: true, UploadAsset: true, DeleteAsset: true, InviteMember: true, RevokeMember: true, ChangeMemberRole: true, ListMembers: true, TransferOwner: true, DeleteAlbum: true, UpdateAlbumSettings: true,
			ReactToAsset: true, ModerateComments: true, ListInvitations: true, HideAsset: true, SeeHiddenAssets: true,
		}},
		{"viewer", Principal{UserID: "v", Active: true, ServerRole: ServerUser}, AlbumAccess{OwnerID: "owner", HasMembership: true, MemberRole: Viewer}, map[Action]bool{GetAlbumMetadata: true, ListAlbumAssets: true, DownloadAsset: true, ReactToAsset: true, ListMembers: true}},
		{"uploader", Principal{UserID: "u", Active: true, ServerRole: ServerUser}, AlbumAccess{OwnerID: "owner", HasMembership: true, MemberRole: Uploader}, map[Action]bool{GetAlbumMetadata: true, UploadAsset: true}},
		{"contributor", Principal{UserID: "c", Active: true, ServerRole: ServerUser}, AlbumAccess{OwnerID: "owner", HasMembership: true, MemberRole: Contributor}, map[Action]bool{GetAlbumMetadata: true, ListAlbumAssets: true, DownloadAsset: true, UploadAsset: true, ReactToAsset: true,
			ListMembers: true, DeleteAsset: true, HideAsset: true, SeeHiddenAssets: true}},
		{"outsider", Principal{UserID: "x", Active: true, ServerRole: ServerUser}, AlbumAccess{OwnerID: "owner"}, map[Action]bool{}},
		{"server-admin-not-owner", Principal{UserID: "admin", Active: true, ServerRole: ServerAdmin}, AlbumAccess{OwnerID: "owner"}, map[Action]bool{}},
		{"unregistered-membership", Principal{UserID: "x", Active: true, ServerRole: ServerUser}, AlbumAccess{OwnerID: "owner", HasMembership: false, MemberRole: Viewer}, map[Action]bool{}},
	}
	for _, tc := range roleCases {
		for _, a := range acts {
			t.Run(tc.name+"/"+string(a), func(t *testing.T) {
				if got := Allowed(tc.user, a, &tc.access); got != tc.want[a] {
					t.Fatalf("got %v, want %v", got, tc.want[a])
				}
			})
		}
	}
}

func TestNoUnknownOrInactivePermissions(t *testing.T) {
	folder := &AlbumAccess{OwnerID: "x"}
	for _, p := range []Principal{{}, {UserID: "x", Active: false}, {UserID: "x", Active: true}} {
		if Allowed(p, Action("surprise_operation"), folder) {
			t.Fatal("unexpected allow")
		}
	}
	if Allowed(Principal{UserID: "x", Active: true}, DownloadAsset, nil) {
		t.Fatal("nil album allowed")
	}
}
