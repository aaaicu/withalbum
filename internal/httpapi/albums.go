package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Album management (docs/API_v2.yaml, tag "albums"): deleting and
// restoring an album, archiving it, the album trash and hidden assets.
//
// Deleting an album (OWNER) hides it from everyone at once, like an
// unknown folder; its owner can restore it for 30 days (meta.TrashRetention),
// then the server purges it (PurgeExpired) together with the originals no
// other album holds. Removing an asset (OWNER, CONTRIBUTOR) moves it to the
// album's trash: it leaves the album for everyone, keeps its likes and
// comments, and can be restored by the OWNER or a CONTRIBUTOR for 30 days;
// then the removal becomes final (the original stays on disk, as it always
// did). Hiding an asset (OWNER, CONTRIBUTOR) makes it invisible to everyone
// else who reads the album (VIEWER) on every read path; it is an app-level
// visibility, never a deletion or an encryption of the original.

func (s *Server) albumRoutes(rt *router) {
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}", s.deleteFolder)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/archive", s.archiveFolder)
	rt.handle("GET /api/v1/folders/deleted", s.listDeletedFolders)
	rt.handleDeletedAlbum("POST /api/v1/folders/{folderID}/restore", s.restoreFolder)
	rt.handleAlbum("PATCH /api/v1/folders/{folderID}/assets/{assetID}", s.updateAsset)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/trash", s.listTrash)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/trash/{assetID}/preview", s.trashPreview)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/trash/{assetID}/restore", s.restoreAsset)
}

// view is what the caller may see of the folder's assets.
func (a albumRequest) view() meta.View { return meta.ViewOf(a.P, a.Folder.Access) }

// albumError replies to a failed album management read or write.
func albumError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, meta.ErrAssetNotFound):
		errorReply(w, 404, "asset_not_found")
	case errors.Is(err, meta.ErrAssetConflict):
		errorReply(w, 409, "asset_conflict")
	case errors.Is(err, meta.ErrFolderNotDeleted):
		errorReply(w, 409, "folder_not_deleted")
	default:
		membershipError(w, what, err)
	}
}

// deleteFolder: DELETE /folders/{id} (OWNER). The folder disappears for
// everyone (GET /folders leaves it out, every folder route answers 404
// folder_not_found) and pending invitations are revoked; nothing is
// deleted until the purge, 30 days later.
func (s *Server) deleteFolder(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	if err := s.albums.DeleteFolder(r.Context(), s.change(a, authz.DeleteAlbum, "")); err != nil {
		albumError(w, "delete folder", err)
		return
	}
	w.WriteHeader(204)
}

// archiveFolder: POST /folders/{id}/archive (OWNER) keeps every file and
// row but hides the folder from everyone, owner included, without a
// restore or a purge (an administrator may then reassign its owner).
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

// listDeletedFolders: GET /folders/deleted, the caller's own deleted
// folders that can still be restored, most recently deleted first.
func (s *Server) listDeletedFolders(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	list, err := s.albums.ListDeletedFolders(r.Context(), p.UserID, time.Now())
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	out := make([]map[string]string, 0, len(list))
	for _, f := range list {
		out = append(out, map[string]string{"id": f.ID, "name": f.Name, "created_at": f.CreatedAt, "deleted_at": f.DeletedAt, "purge_after": f.PurgeAfter})
	}
	jsonReply(w, 200, map[string]any{"folders": out})
}

// restoreFolder: POST /folders/{id}/restore. Only the owner of a deleted
// folder that is not yet due for purging (handleDeletedAlbum: anyone
// else, and any folder in use, archived, purged or unknown, is 404
// folder_not_found). Decided again inside the transaction on the account,
// session and owner as stored then; a folder restored meanwhile is 409
// folder_not_deleted.
func (s *Server) restoreFolder(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	c := s.change(a, authz.DeleteAlbum, "")
	recheck := c.Authorize
	c.Authorize = func(p authz.Principal, access authz.AlbumAccess) error {
		if err := recheck(p, access); err != nil {
			return errHidden // a deleted folder is hidden from everyone else
		}
		return nil
	}
	fa, err := s.albums.RestoreFolder(r.Context(), c)
	if err != nil {
		albumError(w, "restore folder", err)
		return
	}
	jsonReply(w, 200, folderView(a.P, fa))
}

// updateAsset: PATCH /folders/{id}/assets/{aid} {"hidden": bool} (OWNER,
// CONTRIBUTOR). Idempotent; answers the asset as the caller sees it.
func (s *Server) updateAsset(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		Hidden *bool `json:"hidden"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Hidden == nil {
		errorReply(w, 400, "invalid_request")
		return
	}
	if !s.storageReady(w) {
		return
	}
	as, _, err := s.albums.SetAssetHidden(r.Context(), s.change(a, authz.HideAsset, r.PathValue("assetID")), *req.Hidden)
	if err != nil {
		albumError(w, "hide asset", err)
		return
	}
	sums, ok := s.summaries(w, r, a, []string{as.ID})
	if !ok {
		return
	}
	jsonReply(w, 200, assetView(as, a.locationViewer(), sums[as.ID]))
}

// trashJSON is an asset in the album's trash.
type trashJSON struct {
	assetJSON
	RemovedAt string `json:"removed_at"`
	// RemovedByName: who removed it, null when that account cannot log in.
	RemovedByName *string `json:"removed_by_name"`
	PurgeAfter    string  `json:"purge_after"`
}

// listTrash: GET /folders/{id}/trash?cursor=&limit= (OWNER, CONTRIBUTOR),
// most recently removed first; hidden assets included (marked).
func (s *Server) listTrash(w http.ResponseWriter, r *http.Request, a albumRequest) {
	cur, limit, ok := socialPage(w, r)
	if !ok {
		return
	}
	folderID := a.Folder.Folder.ID
	page, err := s.albums.ListTrash(r.Context(), folderID, time.Now(), cur, limit)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	ids := make([]string, 0, len(page.Assets))
	for _, t := range page.Assets {
		ids = append(ids, t.ID)
	}
	sums, ok := s.summaries(w, r, a, ids)
	if !ok {
		return
	}
	list := make([]trashJSON, 0, len(page.Assets))
	for _, t := range page.Assets {
		list = append(list, trashJSON{assetJSON: assetView(t.Asset, a.locationViewer(), sums[t.ID]), RemovedAt: t.RemovedAt,
			RemovedByName: nonEmpty(t.RemovedByName), PurgeAfter: t.PurgeAfter})
	}
	next := ""
	if page.HasMore {
		next = encodeSocialCursor(page.Next)
	}
	jsonReply(w, 200, map[string]any{"folder_id": folderID, "assets": list, "next_cursor": next, "has_more": page.HasMore})
}

// trashPreview: GET /folders/{id}/trash/{aid}/preview (OWNER,
// CONTRIBUTOR), the preview of an asset in the trash.
func (s *Server) trashPreview(w http.ResponseWriter, r *http.Request, a albumRequest) {
	t, err := s.albums.GetTrashedAsset(r.Context(), a.Folder.Folder.ID, r.PathValue("assetID"), time.Now())
	if errors.Is(err, meta.ErrAssetNotFound) {
		errorReply(w, 404, "asset_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	if !t.PreviewReady {
		errorReply(w, 404, "preview_not_ready")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, s.blobs.PreviewPath(t.SHA256))
}

// restoreAsset: POST /folders/{id}/trash/{aid}/restore (OWNER,
// CONTRIBUTOR). The asset comes back with its likes, comments and hidden
// state (asset_added in the feed). 409 asset_conflict when the album holds
// the same original again (uploaded since).
func (s *Server) restoreAsset(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	as, err := s.albums.RestoreAsset(r.Context(), s.change(a, authz.DeleteAsset, r.PathValue("assetID")))
	if err != nil {
		albumError(w, "restore asset", err)
		return
	}
	s.kickPlayback()
	sums, ok := s.summaries(w, r, a, []string{as.ID})
	if !ok {
		return
	}
	jsonReply(w, 200, assetView(as, a.locationViewer(), sums[as.ID]))
}

// PurgeReport is what PurgeExpired did.
type PurgeReport struct {
	TrashPurged     int // removals made final
	FoldersPurged   int
	OriginalsPurged int // originals no asset holds any more, deleted
}

// purgeFolderBatch bounds the deleted folders one PurgeExpired call reads
// at a time.
const purgeFolderBatch = 20

// PurgeExpired purges what was deleted or removed more than
// meta.TrashRetention before now: the album trash (likes and comments of
// removed assets) and deleted folders with the originals no other asset
// holds. It does nothing when the storage volume does not verify. A
// folder whose purge fails is left for the next run.
func (s *Server) PurgeExpired(ctx context.Context, now time.Time) (PurgeReport, error) {
	var rep PurgeReport
	if err := s.vol.Check(); err != nil {
		return rep, err
	}
	before := now.Add(-meta.TrashRetention)
	n, err := s.albums.PurgeTrash(ctx, before)
	rep.TrashPurged = n
	if err != nil {
		return rep, fmt.Errorf("purge trash: %w", err)
	}
	failed := map[string]bool{}
	for {
		ids, err := s.albums.DeletedFoldersDue(ctx, before, purgeFolderBatch+len(failed))
		if err != nil {
			return rep, fmt.Errorf("deleted folders: %w", err)
		}
		progressed := false
		for _, id := range ids {
			if failed[id] {
				continue
			}
			progressed = true
			if err := s.vol.Check(); err != nil {
				return rep, err
			}
			p, err := s.albums.PurgeDeletedFolder(ctx, id, before, s.blobs.DetachOriginals)
			if err != nil {
				log.Printf("purge: folder %s left for the next run: %v", id, err)
				failed[id] = true
				continue
			}
			rep.FoldersPurged++
			for _, sha := range p.Blobs {
				if err := s.blobs.RemoveDetached(sha); err != nil {
					log.Printf("purge: original %s: deletion left for the next start: %v", sha, err)
				}
			}
			rep.OriginalsPurged += len(p.Blobs)
			for _, up := range p.Uploads {
				if err := s.blobs.RemovePart(up); err != nil {
					log.Printf("purge: upload session %s: partial file left for the next start: %v", up, err)
				}
			}
		}
		if !progressed {
			break
		}
	}
	if rep.FoldersPurged > 0 {
		s.kickPlayback()
	}
	return rep, nil
}
