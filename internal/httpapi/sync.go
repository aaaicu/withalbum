package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Sync API v2 (docs/API_v2.yaml): the change feed with tombstones, the
// asset snapshot, the folder-scoped duplicate check, asset removal and the
// optional cursor acknowledgement. Only readers of the folder (OWNER,
// CONTRIBUTOR, VIEWER) reach these routes, except removal (OWNER); an
// UPLOADER is refused with 403 by the route policy before anything is read.
//
// A sync cursor is "<epoch>.<seq>": the folder's feed epoch (16 hex digits,
// see meta.FeedState) and the last applied event sequence. It is valid
// only for the folder and the database that issued it. A cursor of another
// folder or from before a server restore is 409 cursor_invalid; a cursor
// older than the retained events is 409 cursor_expired. Both mean: drop the
// local state of this folder and reload it from the snapshot.

const (
	uploadRoute = "POST /api/v1/folders/{folderID}/assets"

	defaultPageLimit = 100
	maxPageLimit     = 500
	// maxCheckHashes bounds one duplicate check.
	maxCheckHashes = 100

	headerIdempotencyKey = "Idempotency-Key"
)

var (
	idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	epochRE          = regexp.MustCompile(`^[0-9a-f]{16}$`)
	assetIDRE        = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sha256RE         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validIdempotencyKey(k string) bool { return idempotencyKeyRE.MatchString(k) }

func (s *Server) syncRoutes(rt *router) {
	rt.handleAlbum("GET /api/v1/folders/{folderID}/assets", s.listAssets)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/assets/check", s.checkAssets)
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/assets/{assetID}", s.removeAsset)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/sync", s.syncEvents)
	rt.handleAlbum("PUT /api/v1/folders/{folderID}/sync/cursor", s.ackCursor)
}

type syncCursor struct {
	epoch string // "" for "from the beginning"
	seq   int64
}

func encodeCursor(epoch string, seq int64) string { return epoch + "." + strconv.FormatInt(seq, 10) }

// parseCursor parses a cursor parameter; "" is the beginning of the feed.
func parseCursor(v string) (syncCursor, bool) {
	if v == "" {
		return syncCursor{}, true
	}
	epoch, num, ok := strings.Cut(v, ".")
	if !ok || !epochRE.MatchString(epoch) || num == "" || len(num) > 18 || strings.TrimLeft(num, "0123456789") != "" {
		return syncCursor{}, false
	}
	seq, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return syncCursor{}, false
	}
	return syncCursor{epoch: epoch, seq: seq}, true
}

// cursorProblem returns the 409 error code for a cursor that does not fit
// the feed state, or "".
func cursorProblem(c syncCursor, st meta.FeedState) string {
	switch {
	case c.epoch != "" && c.epoch != st.Epoch, c.seq > st.LastSeq:
		return "cursor_invalid"
	case c.seq < st.PrunedSeq:
		return "cursor_expired"
	}
	return ""
}

// pageLimit parses the limit parameter (default and range above). On error
// it replies 400 invalid_limit and returns false.
func pageLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return defaultPageLimit, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPageLimit || strings.TrimLeft(v, "0123456789") != "" {
		errorReply(w, 400, "invalid_limit")
		return 0, false
	}
	return n, true
}

// assetJSON is an asset as a reader of its folder sees it. sha256 lets the
// client verify a downloaded original.
type assetJSON struct {
	ID           string `json:"id"`
	SHA256       string `json:"sha256"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mime_type"`
	Bytes        int64  `json:"bytes"`
	PreviewReady bool   `json:"preview_ready"`
	UploadedAt   string `json:"uploaded_at"`
}

func assetView(as meta.Asset) assetJSON {
	return assetJSON{ID: as.ID, SHA256: as.SHA256, Filename: as.Filename, MimeType: as.MimeType, Bytes: as.Bytes,
		PreviewReady: as.PreviewReady, UploadedAt: as.UploadedAt}
}

// syncEvents: GET /folders/{id}/sync?cursor=&limit=. Events after the
// cursor in seq order: asset_added (with the asset in "assets" while it is
// still in the folder) and asset_removed (a tombstone, no asset entry).
func (s *Server) syncEvents(w http.ResponseWriter, r *http.Request, a albumRequest) {
	folderID := a.Folder.Folder.ID
	cur, ok := parseCursor(r.URL.Query().Get("cursor"))
	if !ok {
		errorReply(w, 400, "invalid_cursor")
		return
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	page, err := s.meta.ListEvents(r.Context(), folderID, cur.seq, limit)
	if errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	if code := cursorProblem(cur, page.State); code != "" {
		errorReply(w, 409, code)
		return
	}
	assets := make(map[string]assetJSON, len(page.Assets))
	for id, as := range page.Assets {
		assets[id] = assetView(as)
	}
	jsonReply(w, 200, map[string]any{"folder_id": folderID, "events": page.Events, "assets": assets,
		"next_cursor": encodeCursor(page.State.Epoch, page.NextCursor), "has_more": page.HasMore})
}

// listAssets: GET /folders/{id}/assets?after=&limit=, the assets currently
// in the folder by id (a snapshot). The first page (without "after") also
// returns "cursor", the feed position read before the snapshot: after the
// last page the client continues with GET /sync from that cursor, which
// replays every change made while it was paging (applying them is
// idempotent by asset id).
func (s *Server) listAssets(w http.ResponseWriter, r *http.Request, a albumRequest) {
	folderID := a.Folder.Folder.ID
	after := r.URL.Query().Get("after")
	if after != "" && !assetIDRE.MatchString(after) {
		errorReply(w, 400, "invalid_after")
		return
	}
	limit, ok := pageLimit(w, r)
	if !ok {
		return
	}
	page, err := s.meta.ListFolderAssets(r.Context(), folderID, after, limit)
	if errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	list := make([]assetJSON, 0, len(page.Assets))
	next := ""
	for _, as := range page.Assets {
		list = append(list, assetView(as))
		next = as.ID
	}
	if !page.HasMore {
		next = ""
	}
	body := map[string]any{"folder_id": folderID, "assets": list, "has_more": page.HasMore, "next_after": next}
	if after == "" {
		body["cursor"] = encodeCursor(page.State.Epoch, page.State.LastSeq)
	}
	jsonReply(w, 200, body)
}

// checkAssets: POST /folders/{id}/assets/check {"sha256": [...]}, which of
// these originals this folder already holds (readers only; never other
// folders, never the server as a whole). Advisory: the upload decides.
func (s *Server) checkAssets(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		SHA256 []string `json:"sha256"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.SHA256) == 0 || len(req.SHA256) > maxCheckHashes {
		errorReply(w, 400, "invalid_request")
		return
	}
	seen := map[string]bool{}
	var hashes []string
	for _, h := range req.SHA256 {
		h = strings.ToLower(strings.TrimSpace(h))
		if !sha256RE.MatchString(h) {
			errorReply(w, 400, "invalid_sha256")
			return
		}
		if !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	found, err := s.meta.FindFolderAssets(r.Context(), a.Folder.Folder.ID, hashes)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	present := []map[string]string{}
	for _, h := range hashes {
		if id, ok := found[h]; ok {
			present = append(present, map[string]string{"sha256": h, "asset_id": id})
		}
	}
	jsonReply(w, 200, map[string]any{"folder_id": a.Folder.Folder.ID, "present": present})
}

// removeAsset: DELETE /folders/{id}/assets/{aid} (OWNER). The asset leaves
// the folder and an asset_removed event is appended, in one audited
// transaction that re-checks the caller. The original stays on disk.
func (s *Server) removeAsset(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	err := s.meta.RemoveAsset(r.Context(), s.change(a, authz.DeleteAsset, r.PathValue("assetID")))
	switch {
	case err == nil:
		w.WriteHeader(204)
	case errors.Is(err, meta.ErrAssetNotFound):
		errorReply(w, 404, "asset_not_found")
	default:
		membershipError(w, "remove asset", err)
	}
}

// ackCursor: PUT /folders/{id}/sync/cursor {"cursor": ...}. Optional:
// records how far this device has applied the feed (device_sync_cursors).
// The server does not depend on it yet; it is meant for a future event
// retention that keeps what devices still need.
func (s *Server) ackCursor(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		Cursor string `json:"cursor"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	cur, ok := parseCursor(req.Cursor)
	if !ok || cur.epoch == "" {
		errorReply(w, 400, "invalid_cursor")
		return
	}
	st, err := s.meta.FeedState(r.Context(), a.Folder.Folder.ID)
	if err != nil {
		membershipError(w, "sync cursor", err)
		return
	}
	if cur.epoch != st.Epoch {
		errorReply(w, 409, "cursor_invalid")
		return
	}
	if !s.storageReady(w) {
		return
	}
	err = s.meta.AckSyncCursor(r.Context(), s.change(a, authz.ListAlbumAssets, ""), a.ID.DeviceID, cur.seq)
	switch {
	case err == nil:
		w.WriteHeader(204)
	case errors.Is(err, meta.ErrCursorInvalid):
		errorReply(w, 409, "cursor_invalid")
	default:
		membershipError(w, "ack sync cursor", err)
	}
}
