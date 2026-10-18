package httpapi

import (
	"errors"
	"net/http"

	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Invitations, memberships, ownership transfer and archiving
// (ACCESS_CONTROL_v1.4.md §2-3). Only the owner manages a folder; the route
// policy decides on the request and every write decides again inside its
// transaction (recheck), so a caller who stopped being the owner meanwhile
// is refused. Every change is audited in the same transaction.
//
// Invitations name one registered, active account by user_id and can only
// be accepted by that account with its own session. Unknown, disabled and
// locked accounts are the same 404 invitee_not_found, so an invitation does
// not reveal an account's state.

func (s *Server) memberRoutes(rt *router) {
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}", s.archiveFolder)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/members", s.listMembers)
	rt.handleAlbum("PATCH /api/v1/folders/{folderID}/members/{userID}", s.setMemberRole)
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/members/{userID}", s.removeMember)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/owner", s.transferOwner)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/invitations", s.listFolderInvitations)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/invitations", s.createInvitation)
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/invitations/{invitationID}", s.revokeInvitation)
	rt.handle("GET /api/v1/invitations", s.listMyInvitations)
	rt.handle("POST /api/v1/invitations/{invitationID}/accept", s.acceptInvitation)
	rt.handle("POST /api/v1/invitations/{invitationID}/decline", s.declineInvitation)
}

type invitationJSON struct {
	ID          string `json:"id"`
	FolderID    string `json:"folder_id"`
	FolderName  string `json:"folder_name"`
	InviteeID   string `json:"invitee_user_id"`
	InviteeName string `json:"invitee_display_name"`
	InviterID   string `json:"inviter_user_id"`
	InviterName string `json:"inviter_display_name"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
}

func invitationView(inv meta.Invitation) invitationJSON {
	return invitationJSON{ID: inv.ID, FolderID: inv.FolderID, FolderName: inv.FolderName, InviteeID: inv.InviteeID,
		InviteeName: inv.InviteeName, InviterID: inv.InviterID, InviterName: inv.InviterName, Role: string(inv.Role),
		Status: inv.Status, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt}
}

func invitationsView(list []meta.Invitation) []invitationJSON {
	out := make([]invitationJSON, 0, len(list))
	for _, inv := range list {
		out = append(out, invitationView(inv))
	}
	return out
}

type memberJSON struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Active      bool   `json:"active"`
	JoinedAt    string `json:"joined_at"`
}

func memberView(m meta.Member) memberJSON {
	return memberJSON{UserID: m.UserID, DisplayName: m.DisplayName, Role: m.Role, Active: m.Active, JoinedAt: m.JoinedAt}
}

// change builds the AlbumChange of an album route: the caller, the folder,
// the action re-checked inside the transaction and the test fault point.
func (s *Server) change(a albumRequest, action authz.Action, target string) meta.AlbumChange {
	return meta.AlbumChange{FolderID: a.Folder.Folder.ID, ActorID: a.P.UserID, SessionID: a.ID.SessionID, TargetID: target,
		Authorize: recheck(action, nil), BeforeCommit: func() error { return s.fault(FaultMembershipBeforeCommit) }}
}

// membershipError replies to a failed membership read or write.
func membershipError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, errHidden), errors.Is(err, meta.ErrFolderNotFound):
		errorReply(w, 404, "folder_not_found")
	case errors.Is(err, errDenied), errors.Is(err, meta.ErrNotInvitee):
		errorReply(w, 403, "forbidden")
	case errors.Is(err, meta.ErrInvalidRole):
		errorReply(w, 400, "invalid_role")
	case errors.Is(err, meta.ErrInviteeNotFound):
		errorReply(w, 404, "invitee_not_found")
	case errors.Is(err, meta.ErrMemberNotFound):
		errorReply(w, 404, "member_not_found")
	case errors.Is(err, meta.ErrInvitationNotFound):
		errorReply(w, 404, "invitation_not_found")
	case errors.Is(err, meta.ErrInvitationGone):
		errorReply(w, 410, "invitation_gone")
	case errors.Is(err, meta.ErrAlreadyMember):
		errorReply(w, 409, "already_member")
	case errors.Is(err, meta.ErrInvitationPending):
		errorReply(w, 409, "invitation_pending")
	case errors.Is(err, meta.ErrTargetIsOwner):
		errorReply(w, 409, "target_is_owner")
	case errors.Is(err, meta.ErrNewOwnerInactive):
		errorReply(w, 409, "user_inactive")
	case errors.Is(err, meta.ErrIDGeneration):
		errorReply(w, 500, "random_failure")
	default:
		writeFailed(w, what, err, "metadata_write_failed")
	}
}

// archiveFolder: DELETE /folders/{id} keeps every file and row but hides the
// folder from everyone, owner included (no restore API yet).
func (s *Server) archiveFolder(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	if err := s.members.ArchiveFolder(r.Context(), s.change(a, authz.DeleteAlbum, "")); err != nil {
		membershipError(w, "archive folder", err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, a albumRequest) {
	list, err := s.members.ListMembers(r.Context(), a.Folder.Folder.ID)
	if errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	out := make([]memberJSON, 0, len(list))
	for _, m := range list {
		out = append(out, memberView(m))
	}
	jsonReply(w, 200, map[string]any{"folder_id": a.Folder.Folder.ID, "members": out})
}

// setMemberRole: {"role": "VIEWER"|"UPLOADER"|"CONTRIBUTOR"}. The member's
// next request is decided with the new role.
func (s *Server) setMemberRole(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.storageReady(w) {
		return
	}
	c := s.change(a, authz.ChangeMemberRole, r.PathValue("userID"))
	c.Role = authz.AlbumRole(req.Role)
	m, err := s.members.SetMemberRole(r.Context(), c)
	if err != nil {
		membershipError(w, "set member role", err)
		return
	}
	jsonReply(w, 200, memberView(m))
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	if err := s.members.RemoveMember(r.Context(), s.change(a, authz.RevokeMember, r.PathValue("userID"))); err != nil {
		membershipError(w, "remove member", err)
		return
	}
	w.WriteHeader(204)
}

// transferOwner: {"new_owner_user_id": ...}, a current member with an active
// account. The caller stays in the folder as CONTRIBUTOR; the reply is the
// folder as the caller now sees it.
func (s *Server) transferOwner(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		NewOwnerID string `json:"new_owner_user_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NewOwnerID == "" {
		errorReply(w, 400, "invalid_request")
		return
	}
	if !s.storageReady(w) {
		return
	}
	if err := s.members.TransferOwnership(r.Context(), s.change(a, authz.TransferOwner, req.NewOwnerID)); err != nil {
		membershipError(w, "transfer owner", err)
		return
	}
	fa := a.Folder
	fa.Access = authz.AlbumAccess{OwnerID: req.NewOwnerID, HasMembership: true, MemberRole: authz.Contributor}
	jsonReply(w, 200, folderView(a.P, fa))
}

func (s *Server) listFolderInvitations(w http.ResponseWriter, r *http.Request, a albumRequest) {
	list, err := s.members.ListFolderInvitations(r.Context(), a.Folder.Folder.ID)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	jsonReply(w, 200, map[string]any{"invitations": invitationsView(list)})
}

// createInvitation: {"invited_user_id": ..., "role": ...}, valid 7 days.
func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		InviteeID string `json:"invited_user_id"`
		Role      string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InviteeID == "" {
		errorReply(w, 400, "invalid_request")
		return
	}
	if !s.storageReady(w) {
		return
	}
	c := s.change(a, authz.InviteMember, req.InviteeID)
	c.Role = authz.AlbumRole(req.Role)
	inv, err := s.members.CreateInvitation(r.Context(), c)
	if err != nil {
		membershipError(w, "create invitation", err)
		return
	}
	jsonReply(w, 201, invitationView(inv))
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	if err := s.members.RevokeInvitation(r.Context(), s.change(a, authz.InviteMember, r.PathValue("invitationID"))); err != nil {
		membershipError(w, "revoke invitation", err)
		return
	}
	w.WriteHeader(204)
}

// listMyInvitations: the pending invitations addressed to the caller.
func (s *Server) listMyInvitations(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	list, err := s.members.ListInvitationsFor(r.Context(), id.UserID)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	jsonReply(w, 200, map[string]any{"invitations": invitationsView(list)})
}

// inviteeChange is the AlbumChange of the invitee's answer to invitation
// invitationID: the store decides on the invitation and the folder inside
// the transaction; here only that the caller's account is active.
func (s *Server) inviteeChange(id session.Identity, invitationID string) meta.AlbumChange {
	return meta.AlbumChange{ActorID: id.UserID, SessionID: id.SessionID, TargetID: invitationID,
		Authorize: func(p authz.Principal, _ authz.AlbumAccess) error {
			if !p.Active {
				return errDenied
			}
			return nil
		},
		BeforeCommit: func() error { return s.fault(FaultMembershipBeforeCommit) }}
}

// acceptInvitation: only the invited account, while the invitation is
// pending, unexpired and was written by the folder's current owner. Unknown
// invitation 404, another account's 403, used/revoked/expired/declined 410.
func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	p := principal(id)
	if !s.storageReady(w) {
		return
	}
	fa, err := s.members.AcceptInvitation(r.Context(), s.inviteeChange(id, r.PathValue("invitationID")))
	if err != nil {
		membershipError(w, "accept invitation", err)
		return
	}
	jsonReply(w, 200, folderView(p, fa))
}

// declineInvitation: decided exactly like acceptInvitation (404, 403 for
// another account including administrators, 410). The invitation stops
// being pending, so it leaves the owner's list and the owner may invite the
// same account again. The reply says nothing about the album.
func (s *Server) declineInvitation(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	if !s.storageReady(w) {
		return
	}
	if err := s.members.DeclineInvitation(r.Context(), s.inviteeChange(id, r.PathValue("invitationID"))); err != nil {
		membershipError(w, "decline invitation", err)
		return
	}
	w.WriteHeader(204)
}
