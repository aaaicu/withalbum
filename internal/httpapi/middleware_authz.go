package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Access is how a route is protected. Every route of the API is declared in
// routePolicies with one of these; registering a route that is not declared
// there (or declaring one that is never registered) panics when the handler
// is built, so no route can be added without a deliberate access decision.
type Access string

const (
	// AccessPublic: no session. Only the dashboard page (static, no data), health
	// and the login and refresh endpoints.
	AccessPublic Access = "public"
	// AccessSession: any valid session; the handler only touches the caller's
	// own data (own account, own devices, own folder list).
	AccessSession Access = "session"
	// AccessServer: a valid session and authz.Allowed(principal, Action, nil),
	// e.g. account administration or creating an album.
	AccessServer Access = "server"
	// AccessAlbum: a valid session, the folder named by {folderID}, and
	// authz.Allowed(principal, Action, access) on the owner and membership
	// read from the database for this request. A folder the caller cannot
	// see at all (unknown, archived, or neither owner nor member; a server
	// administrator is no exception) is 404 folder_not_found; a folder the
	// caller can see but where the role lacks Action is 403 forbidden.
	AccessAlbum Access = "album"
)

// RoutePolicy is the declared protection of one route.
type RoutePolicy struct {
	Pattern string // http.ServeMux pattern
	Access  Access
	Action  authz.Action // AccessServer and AccessAlbum only
}

// routePolicies lists every route of normal mode. The setup screen (M24)
// has its own table (setupRoutePolicies in setup.go), served only by the
// setup handler on the plain loopback listener before an administrator
// exists; none of its routes is ever registered here, so in normal mode and
// on every TLS or LAN listener they are 404.
//
// Patterns without a method match any
// method; "/api/v1/" catches every other API path, so an unknown API path is
// 401 without a session and 404 with one.
var routePolicies = []RoutePolicy{
	{"GET /{$}", AccessPublic, ""},
	{"GET /api/v1/health", AccessPublic, ""},
	{"POST /api/v1/auth/login", AccessPublic, ""},
	{"POST /api/v1/auth/refresh", AccessPublic, ""},
	{"/api/v1/", AccessSession, ""},
	{"POST /api/v1/auth/logout", AccessSession, ""},
	{"GET /api/v1/me", AccessSession, ""},
	{"POST /api/v1/me/password", AccessSession, ""},
	{"GET /api/v1/me/devices", AccessSession, ""},
	{"DELETE /api/v1/me/devices/{deviceID}", AccessSession, ""},
	{"GET /api/v1/admin/users", AccessServer, authz.ManageAccounts},
	{"POST /api/v1/admin/users", AccessServer, authz.ManageAccounts},
	{"GET /api/v1/admin/users/{userID}", AccessServer, authz.ManageAccounts},
	{"PATCH /api/v1/admin/users/{userID}", AccessServer, authz.ManageAccounts},
	{"PATCH /api/v1/admin/users/{userID}/permissions", AccessServer, authz.ManageAccounts},
	{"GET /api/v1/admin/users/{userID}/devices", AccessServer, authz.ManageAccounts},
	{"DELETE /api/v1/admin/users/{userID}/devices/{deviceID}", AccessServer, authz.ManageAccounts},
	{"GET /api/v1/admin/users/{userID}/folders", AccessServer, authz.ManageAccounts},
	// Archived folders only; the handler and the transaction refuse others
	// as unknown (admin.go). No access to the folder follows from it.
	{"POST /api/v1/admin/folders/{folderID}/owner", AccessServer, authz.ManageAccounts},
	{"GET /api/v1/admin/server/tls", AccessServer, authz.ManageServer},
	{"GET /api/v1/admin/server/storage", AccessServer, authz.ManageServer},
	{"GET /api/v1/folders", AccessSession, ""},
	{"POST /api/v1/folders", AccessServer, authz.CreateAlbum},
	{"GET /api/v1/folders/{folderID}", AccessAlbum, authz.GetAlbumMetadata},
	{"POST /api/v1/folders/{folderID}/assets", AccessAlbum, authz.UploadAsset},
	// Resumable upload sessions (uploads.go): only the account, device and
	// folder that created a session can see it; any other ID is 404.
	{"POST /api/v1/folders/{folderID}/uploads", AccessAlbum, authz.UploadAsset},
	{"GET /api/v1/folders/{folderID}/uploads/{uploadID}", AccessAlbum, authz.UploadAsset},
	{"PATCH /api/v1/folders/{folderID}/uploads/{uploadID}", AccessAlbum, authz.UploadAsset},
	{"DELETE /api/v1/folders/{folderID}/uploads/{uploadID}", AccessAlbum, authz.UploadAsset},
	{"POST /api/v1/folders/{folderID}/uploads/{uploadID}/complete", AccessAlbum, authz.UploadAsset},
	{"GET /api/v1/folders/{folderID}/assets", AccessAlbum, authz.ListAlbumAssets},
	{"POST /api/v1/folders/{folderID}/assets/check", AccessAlbum, authz.ListAlbumAssets},
	{"DELETE /api/v1/folders/{folderID}/assets/{assetID}", AccessAlbum, authz.DeleteAsset},
	{"GET /api/v1/folders/{folderID}/sync", AccessAlbum, authz.ListAlbumAssets},
	{"PUT /api/v1/folders/{folderID}/sync/cursor", AccessAlbum, authz.ListAlbumAssets},
	{"GET /api/v1/folders/{folderID}/assets/{assetID}/original", AccessAlbum, authz.DownloadAsset},
	{"GET /api/v1/folders/{folderID}/assets/{assetID}/preview", AccessAlbum, authz.DownloadAsset},
	{"DELETE /api/v1/folders/{folderID}", AccessAlbum, authz.DeleteAlbum},
	{"GET /api/v1/folders/{folderID}/members", AccessAlbum, authz.ListMembers},
	{"PATCH /api/v1/folders/{folderID}/members/{userID}", AccessAlbum, authz.ChangeMemberRole},
	{"DELETE /api/v1/folders/{folderID}/members/{userID}", AccessAlbum, authz.RevokeMember},
	{"POST /api/v1/folders/{folderID}/owner", AccessAlbum, authz.TransferOwner},
	{"GET /api/v1/folders/{folderID}/invitations", AccessAlbum, authz.ListMembers},
	{"POST /api/v1/folders/{folderID}/invitations", AccessAlbum, authz.InviteMember},
	{"DELETE /api/v1/folders/{folderID}/invitations/{invitationID}", AccessAlbum, authz.InviteMember},
	// Only the invited account's own invitations; acceptance is checked
	// against the invitation and the folder inside the transaction.
	{"GET /api/v1/invitations", AccessSession, ""},
	{"POST /api/v1/invitations/{invitationID}/accept", AccessSession, ""},
}

// Routes returns the declared protection of every route (for tests and
// documentation).
func Routes() []RoutePolicy { return slices.Clone(routePolicies) }

// albumRequest is what an album route handler gets: the caller and the
// folder with the caller's relation to it, both read for this request.
type albumRequest struct {
	ID     session.Identity
	P      authz.Principal
	Folder meta.FolderAccess
}

// allowed evaluates action on this folder for this caller.
func (a albumRequest) allowed(action authz.Action) bool {
	return authz.Allowed(a.P, action, &a.Folder.Access)
}

type albumHandler func(w http.ResponseWriter, r *http.Request, a albumRequest)

// router registers routes only through their declared policy.
type router struct {
	s        *Server
	mux      *http.ServeMux
	policies map[string]RoutePolicy
	done     map[string]bool
}

func newRouter(s *Server) *router {
	rt := &router{s: s, mux: http.NewServeMux(), policies: map[string]RoutePolicy{}, done: map[string]bool{}}
	for _, p := range routePolicies {
		if _, dup := rt.policies[p.Pattern]; dup {
			panic("httpapi: route declared twice: " + p.Pattern)
		}
		if (p.Access == AccessServer || p.Access == AccessAlbum) != (p.Action != "") {
			panic("httpapi: route " + p.Pattern + ": an action is required for server and album routes only")
		}
		rt.policies[p.Pattern] = p
	}
	return rt
}

func (rt *router) policy(pattern string, album bool) RoutePolicy {
	p, ok := rt.policies[pattern]
	if !ok {
		panic("httpapi: route without a declared access policy: " + pattern)
	}
	if rt.done[pattern] {
		panic("httpapi: route registered twice: " + pattern)
	}
	if (p.Access == AccessAlbum) != album {
		panic(fmt.Sprintf("httpapi: route %s is declared %s", pattern, p.Access))
	}
	rt.done[pattern] = true
	return p
}

// handle registers a public, session or server route, wrapped as declared.
func (rt *router) handle(pattern string, h http.HandlerFunc) {
	p := rt.policy(pattern, false)
	switch p.Access {
	case AccessPublic:
		rt.mux.Handle(pattern, h)
	case AccessSession:
		rt.mux.Handle(pattern, rt.s.RequireSession(h))
	case AccessServer:
		rt.mux.Handle(pattern, rt.s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pr, _ := PrincipalFrom(r.Context())
			if !authz.Allowed(pr, p.Action, nil) {
				rt.s.denied(r, pr.UserID, pattern, 403, "")
				errorReply(w, 403, "forbidden")
				return
			}
			h(w, r)
		})))
	default:
		panic("httpapi: unknown access " + string(p.Access))
	}
}

// handleAlbum registers an album route.
func (rt *router) handleAlbum(pattern string, h albumHandler) {
	p := rt.policy(pattern, true)
	rt.mux.Handle(pattern, rt.s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := IdentityFrom(r.Context())
		a := albumRequest{ID: id, P: principal(id)}
		fa, err := rt.s.meta.GetFolderAccess(r.Context(), r.PathValue("folderID"), a.P.UserID)
		if errors.Is(err, meta.ErrFolderNotFound) {
			rt.s.denied(r, a.P.UserID, pattern, 404, r.PathValue("folderID"))
			errorReply(w, 404, "folder_not_found")
			return
		}
		if err != nil {
			metaReadFailed(w, err)
			return
		}
		a.Folder = fa
		// Owners and members (any role) may know the folder exists.
		if !a.allowed(authz.GetAlbumMetadata) {
			rt.s.denied(r, a.P.UserID, pattern, 404, r.PathValue("folderID"))
			errorReply(w, 404, "folder_not_found")
			return
		}
		if !a.allowed(p.Action) {
			rt.s.denied(r, a.P.UserID, pattern, 403, fa.Folder.ID)
			errorReply(w, 403, "forbidden")
			return
		}
		h(w, r, a)
	})))
}

// finish returns the mux after checking that every declared route exists.
func (rt *router) finish() *http.ServeMux {
	for _, p := range routePolicies {
		if !rt.done[p.Pattern] {
			panic("httpapi: declared route has no handler: " + p.Pattern)
		}
	}
	return rt.mux
}

// role names the caller's relation to a folder.
func role(userID string, a authz.AlbumAccess) string {
	if a.OwnerID == userID {
		return "OWNER"
	}
	if a.HasMembership {
		return string(a.MemberRole)
	}
	return ""
}
