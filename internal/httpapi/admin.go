package httpapi

import (
	"errors"
	"log"
	"net/http"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Server administration beyond accounts: the storage state, and the
// ownership of archived folders (so that the account of a family member who
// left can be disabled). None of these routes reveals a folder's name,
// members or contents, and none gives the administrator access to a folder.

func (s *Server) adminRoutes(rt *router) {
	rt.handle("GET /api/v1/admin/server/tls", s.serverTLS)
	rt.handle("GET /api/v1/admin/server/storage", s.serverStorage)
	rt.handle("GET /api/v1/admin/users/{userID}/folders", s.adminOwnedFolders)
	rt.handle("POST /api/v1/admin/folders/{folderID}/owner", s.adminReassignArchivedOwner)
}

// StartupCheck is the result of the startup reconciliation of the pool
// against the database (counts only, never hashes or paths).
type StartupCheck struct {
	Blobs        int // originals recorded in the database
	Missing      int // recorded but missing from the pool
	SizeMismatch int // pool file of the wrong size (damaged)
	Orphans      int // unreferenced pool files moved to quarantine
	Unexpected   int // entries WithAlbum did not create
}

// serverStorage answers GET /api/v1/admin/server/storage (SERVER_ADMIN):
// whether the storage volume is present, its free space and upload
// reserve, the stored and album totals (usage: how much storing every
// SHA-256 once saves, totals only), the playback copies of videos (tool,
// counts by state, bytes of the ready copies), and the startup check. No path or
// volume identifier. The full check (withalbum-server verify) is a console
// command; its result is not kept by the server.
func (s *Server) serverStorage(w http.ResponseWriter, r *http.Request) {
	state := "ok"
	if err := s.vol.Check(); err != nil {
		state = "unavailable"
	}
	integrity := "ok"
	if s.integrityWarning {
		integrity = "warning"
	}
	body := map[string]any{"storage": state, "integrity": integrity, "min_free_bytes": s.minFree, "free_bytes": nil}
	if free, err := s.freeBytes(); err == nil {
		body["free_bytes"] = free
	} else if !errors.Is(err, storage.ErrFreeSpaceUnsupported) {
		body["storage"] = "unavailable"
	}
	if ur, ok := s.meta.(meta.StorageUsageReporter); ok {
		if u, err := ur.StorageUsage(r.Context()); err == nil {
			body["usage"] = map[string]int64{"stored_originals": u.StoredOriginals, "stored_bytes": u.StoredBytes,
				"album_copies": u.AlbumCopies, "album_bytes": u.AlbumBytes, "dedup_saved_bytes": u.SavedBytes}
		} else {
			log.Printf("admin storage: cannot read storage usage: %v", err)
		}
	}
	if s.playback != nil {
		if st, err := s.playback.PlaybackStats(r.Context()); err == nil {
			tool := ""
			if t := s.blobs.Transcoder(); t != nil {
				tool = t.Name()
			}
			body["playback"] = map[string]any{"tool": nonEmpty(tool), "pending": st.Pending, "ready": st.Ready, "failed": st.Failed,
				"unsupported": st.Unsupported, "ready_bytes": st.ReadyBytes}
		} else {
			log.Printf("admin storage: cannot read playback copies: %v", err)
		}
	}
	if c := s.startup; c != nil {
		body["startup_check"] = map[string]int{"blobs": c.Blobs, "missing": c.Missing, "size_mismatch": c.SizeMismatch,
			"orphans": c.Orphans, "unexpected": c.Unexpected}
	}
	jsonReply(w, 200, body)
}

// adminOwnedFolders answers GET /api/v1/admin/users/{userID}/folders: how
// many folders in use the account owns, and the IDs and dates of its
// archived ones, which an administrator may reassign before disabling it.
func (s *Server) adminOwnedFolders(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	if _, err := s.auth.Users.Get(r.Context(), userID); err != nil {
		s.usersError(w, "get user", err)
		return
	}
	of, err := s.members.OwnedFolders(r.Context(), userID)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	archived := make([]map[string]string, 0, len(of.Archived))
	for _, f := range of.Archived {
		archived = append(archived, map[string]string{"folder_id": f.ID, "created_at": f.CreatedAt, "archived_at": f.ArchivedAt})
	}
	jsonReply(w, 200, map[string]any{"user_id": userID, "active_owned": of.Active, "archived": archived})
}

// adminReassignArchivedOwner answers POST /api/v1/admin/folders/{folderID}/owner
// ({"new_owner_user_id": ...}). Only archived folders: a folder in use is
// answered exactly like an unknown one (404 folder_not_found), so the route
// tells an administrator nothing about folders it cannot see. The folder
// stays archived, so the new owner sees nothing of it either.
func (s *Server) adminReassignArchivedOwner(w http.ResponseWriter, r *http.Request) {
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
	id, _ := IdentityFrom(r.Context())
	res, err := s.members.ReassignArchivedOwner(r.Context(), meta.AlbumChange{FolderID: r.PathValue("folderID"),
		ActorID: id.UserID, SessionID: id.SessionID, TargetID: req.NewOwnerID,
		// Decided again on the account and session as stored inside the
		// transaction: a demoted or signed-out administrator is refused.
		Authorize: func(p authz.Principal, _ authz.AlbumAccess) error {
			if !authz.Allowed(p, authz.ManageAccounts, nil) {
				return errDenied
			}
			return nil
		},
		BeforeCommit: func() error { return s.fault(FaultMembershipBeforeCommit) }})
	switch {
	case err == nil:
	case errors.Is(err, errDenied):
		errorReply(w, 403, "forbidden")
		return
	case errors.Is(err, meta.ErrFolderNotFound), errors.Is(err, meta.ErrFolderNotArchived):
		errorReply(w, 404, "folder_not_found")
		return
	case errors.Is(err, meta.ErrNewOwnerNotFound):
		errorReply(w, 404, "user_not_found")
		return
	case errors.Is(err, meta.ErrTargetIsOwner):
		errorReply(w, 409, "target_is_owner")
		return
	case errors.Is(err, meta.ErrNewOwnerInactive):
		errorReply(w, 409, "user_inactive")
		return
	default:
		writeFailed(w, "reassign archived folder", err, "metadata_write_failed")
		return
	}
	jsonReply(w, 200, map[string]any{"folder_id": res.FolderID, "previous_owner_user_id": res.PreviousOwnerID,
		"owner_user_id": res.NewOwnerID, "archived_at": res.ArchivedAt})
}
