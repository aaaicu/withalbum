package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	rt.handleAlbum("GET /api/v1/folders/{folderID}/members", s.listMembers)
	rt.handleAlbum("PATCH /api/v1/folders/{folderID}/members/{userID}", s.setMemberRole)
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/members/{userID}", s.removeMember)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/owner", s.transferOwner)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/invitations", s.listFolderInvitations)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/invitations", s.createInvitation)
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/invitations/{invitationID}", s.revokeInvitation)
	rt.handle("GET /api/v1/invitations", s.listMyInvitations)
	s.inviteLinkRoutes(rt)
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
	// AnsweredAt: when it stopped being pending (accepted, declined or
	// revoked at, or its expiry); null while pending and when unknown
	// (answered before the server recorded it).
	AnsweredAt *string `json:"answered_at"`
	// Reason: why a revoked invitation was revoked (meta.Reason*); null
	// otherwise and when unknown.
	Reason *string `json:"reason"`
	// MemberNow: the invitee is the owner or a member of the album now.
	MemberNow bool `json:"member_now"`
	// Source: "direct" (addressed to the account) or "link" (an invite
	// link the account redeemed).
	Source string `json:"source"`
}

func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func invitationView(inv meta.Invitation) invitationJSON {
	v := invitationJSON{ID: inv.ID, FolderID: inv.FolderID, FolderName: inv.FolderName, InviteeID: inv.InviteeID,
		InviteeName: inv.InviteeName, InviterID: inv.InviterID, InviterName: inv.InviterName, Role: string(inv.Role),
		Status: inv.Status, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt, AnsweredAt: optString(inv.AnsweredAt),
		MemberNow: inv.MemberNow, Source: "direct"}
	if inv.Status == meta.InvitationRevoked {
		v.Reason = optString(inv.Reason)
	}
	if inv.LinkID != "" {
		v.Source = "link"
	}
	return v
}

// ownerInvitationView is an invitation as the album's owner sees it: a
// declined one is "not_joined", the neutral word the owner also reads for
// one that expired (the owner learns the account did not join, not how
// it answered).
func ownerInvitationView(inv meta.Invitation) invitationJSON {
	v := invitationView(inv)
	if v.Status == meta.InvitationDeclined {
		v.Status = meta.InvitationNotJoined
	}
	return v
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
	IsMe        bool   `json:"is_me"`
	IsOwner     bool   `json:"is_owner"`
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
		goneReply(w, "invitation_gone", err)
	case errors.Is(err, meta.ErrInviteLinkGone):
		goneReply(w, "invite_link_gone", err)
	case errors.Is(err, meta.ErrInviteLinkNotFound):
		errorReply(w, 404, "invite_link_not_found")
	case errors.Is(err, meta.ErrTooManyInviteLinks):
		errorReply(w, 409, "too_many_invite_links")
	case errors.Is(err, meta.ErrInvalidInviteLink):
		errorReply(w, 400, "invalid_invite_link")
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

// listMembers: GET /folders/{id}/members, every reader of the folder
// (OWNER, CONTRIBUTOR, VIEWER; never an UPLOADER): the owner first, then
// the members by join time, each with is_me and is_owner. The owner also
// gets members whose accounts cannot log in (active false) and the pending
// invitations; the others only see members who can log in.
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
	owner := a.allowed(authz.ListInvitations)
	out := make([]memberJSON, 0, len(list))
	for _, m := range list {
		if !m.Active && !owner {
			continue
		}
		v := memberView(m)
		v.IsMe, v.IsOwner = m.UserID == a.P.UserID, m.Role == "OWNER"
		out = append(out, v)
	}
	body := map[string]any{"folder_id": a.Folder.Folder.ID, "members": out}
	if owner {
		inv, err := s.members.ListFolderInvitations(r.Context(), a.Folder.Folder.ID)
		if err != nil {
			metaReadFailed(w, err)
			return
		}
		body["invitations"] = invitationsView(inv)
	}
	jsonReply(w, 200, body)
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

// goneReply answers 410 code with the reason of a meta.GoneError.
func goneReply(w http.ResponseWriter, code string, err error) {
	body := map[string]string{"error": code}
	if reason := meta.GoneReason(err); reason != "" {
		body["reason"] = reason
	}
	jsonReply(w, 410, body)
}

// Invitation history pages (GET /invitations, GET /folders/{id}/invitations).
const (
	defaultInvitationLimit = 100
	maxInvitationLimit     = 100
	defaultHistoryDays     = meta.MaxHistoryDays
)

// invitationQuery parses include=recent, days, limit and cursor into q. On
// error it replies 400 and returns false.
func invitationQuery(w http.ResponseWriter, r *http.Request, q *meta.InvitationQuery) bool {
	v := r.URL.Query()
	number := func(name string, def, hi int) (int, bool) {
		s := v.Get(name)
		if s == "" {
			return def, true
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > hi || strings.TrimLeft(s, "0123456789") != "" {
			return 0, false
		}
		return n, true
	}
	switch v.Get("include") {
	case "":
	case "recent":
		q.Recent = true
	default:
		errorReply(w, 400, "invalid_filter")
		return false
	}
	days, ok := number("days", defaultHistoryDays, meta.MaxHistoryDays)
	if !ok || (v.Has("days") && !q.Recent) {
		errorReply(w, 400, "invalid_filter")
		return false
	}
	if q.Limit, ok = number("limit", defaultInvitationLimit, maxInvitationLimit); !ok {
		errorReply(w, 400, "invalid_limit")
		return false
	}
	q.Now = time.Now()
	q.Since = q.Now.Add(-time.Duration(days) * 24 * time.Hour)
	q.After = v.Get("cursor")
	return true
}

// invitationPage replies with one history page, each invitation shown by
// view.
func (s *Server) invitationPage(w http.ResponseWriter, r *http.Request, q meta.InvitationQuery, view func(meta.Invitation) invitationJSON) {
	page, err := s.members.InvitationHistory(r.Context(), q)
	if errors.Is(err, meta.ErrInvalidCursor) {
		errorReply(w, 400, "invalid_cursor")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	out := make([]invitationJSON, 0, len(page.Invitations))
	for _, inv := range page.Invitations {
		out = append(out, view(inv))
	}
	body := map[string]any{"invitations": out, "next_cursor": optString(page.NextCursor)}
	jsonReply(w, 200, body)
}

// listFolderInvitations: the album's pending invitations, newest first,
// and with include=recent also those answered in the last days (OWNER).
func (s *Server) listFolderInvitations(w http.ResponseWriter, r *http.Request, a albumRequest) {
	q := meta.InvitationQuery{FolderID: a.Folder.Folder.ID}
	if !invitationQuery(w, r, &q) {
		return
	}
	s.invitationPage(w, r, q, ownerInvitationView)
}

// createInvitation: {"invited_user_id": ..., "role": ..., "replace_pending":
// bool}, valid 7 days. With replace_pending a pending invitation of the
// same account is revoked (reason replaced) instead of 409.
func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		InviteeID      string `json:"invited_user_id"`
		Role           string `json:"role"`
		ReplacePending bool   `json:"replace_pending"`
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
	c.Role, c.ReplacePending = authz.AlbumRole(req.Role), req.ReplacePending
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

// listMyInvitations: the pending invitations addressed to the caller,
// newest first, and with include=recent also those answered in the last
// days (most recently answered first).
func (s *Server) listMyInvitations(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	q := meta.InvitationQuery{InviteeID: id.UserID}
	if !invitationQuery(w, r, &q) {
		return
	}
	s.invitationPage(w, r, q, invitationView)
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
