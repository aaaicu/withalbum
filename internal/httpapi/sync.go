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
	// PreviewStatus: ready, pending (the preview worker will make it; an
	// asset_updated event follows), failed or unsupported.
	PreviewStatus string `json:"preview_status"`
	UploadedAt    string `json:"uploaded_at"`
	// CapturedAt: when it was taken, read from the original by the server
	// ("2026-10-19T14:03:22+09:00" with the local offset, "...Z" an
	// instant without it, no suffix a local time without zone), or null
	// when unknown or not read yet (an asset_updated event follows when it
	// is found). Never the upload time.
	CapturedAt *string `json:"captured_at"`
	// DurationMS: the playing time in whole milliseconds, read by the
	// server from the movie header of the original, or null when it is not
	// a movie, unknown or not read yet (an asset_updated event follows when
	// it is found). Always null for a live_photo: its own original is the
	// still; the video's length is on its component.
	DurationMS *int64 `json:"duration_ms"`
	// Location: where it was taken, read by the server from the original's
	// metadata (EXIF GPS, QuickTime ISO 6709), or null when unknown, not
	// read yet (an asset_updated event follows), or hidden: the folder's
	// owner set hide_location and the caller is a member, not the owner, or
	// the uploader set hide_my_location and the caller is someone else.
	// For a live_photo the still's, else its video's.
	Location *locationJSON `json:"location"`
	// Camera: the device it was taken with, read by the server from the
	// original's metadata (EXIF Make, Model, LensModel,
	// FocalLengthIn35mmFilm; QuickTime make/model), or null when none is
	// recorded or not read yet (an asset_updated event follows). For a
	// live_photo the still's.
	Camera *cameraJSON `json:"camera"`
	// Width, Height: the original's pixel size as displayed (after its
	// recorded orientation), both null when unknown or not read yet.
	Width  *int `json:"width"`
	Height *int `json:"height"`
	// UploadedByName: the uploader's display name, null when there is no
	// uploader or the account is disabled or cannot log in.
	UploadedByName *string `json:"uploaded_by_name"`
	// MediaKind: image, video, live_photo, raw or other. A live_photo's
	// own fields (sha256, mime_type, bytes, /original, /preview) are its
	// still; its video is in Components.
	MediaKind string `json:"media_kind"`
	// Pairing (live photos only): "verified" when the still and the video
	// carry the same Apple content identifier, "unverified" when the server
	// could not read one of them.
	Pairing *string `json:"pairing,omitempty"`
	// Components (live photos only): the extra originals, each downloaded
	// from .../components/{role}.
	Components []componentJSON `json:"components,omitempty"`
	// LikeCount: likes of people who can read the folder now; LikedByMe:
	// the caller's own; CommentCount: every comment on the asset in this
	// folder (social.go). Only readers ever get an asset.
	LikeCount    int  `json:"like_count"`
	LikedByMe    bool `json:"liked_by_me"`
	CommentCount int  `json:"comment_count"`
}

// componentJSON is an extra original of an asset.
type componentJSON struct {
	Role     string `json:"role"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	MimeType string `json:"mime_type"`
	Filename string `json:"filename"`
	// DurationMS: as assetJSON.DurationMS, for this original.
	DurationMS *int64 `json:"duration_ms"`
}

// cameraJSON: each field null when the original does not record it.
type cameraJSON struct {
	Make      *string `json:"make"`
	Model     *string `json:"model"`
	Lens      *string `json:"lens"`
	Focal35mm *int    `json:"focal_35mm"`
}

// locationJSON is a position in WGS 84 degrees with the recorded
// horizontal accuracy in metres (null when the original gave none).
type locationJSON struct {
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	AccuracyM *float64 `json:"accuracy_m"`
}

// showLocation reports whether the caller of an album request may see the
// folder's asset locations at all: a reader (the route policy already
// requires ListAlbumAssets) and, when the owner hides them, only the owner.
// The uploader's own choice is applied per asset (assetView).
func (a albumRequest) showLocation() bool {
	if !a.allowed(authz.ListAlbumAssets) {
		return false
	}
	return !a.Folder.Folder.HideLocation || a.Folder.Access.OwnerID == a.P.UserID
}

// locationViewer is who looks at an asset: whether the album lets them see
// locations (albumRequest.showLocation) and their user_id, against the
// uploader's own hide_my_location.
type locationViewer struct {
	album  bool
	userID string
}

func (a albumRequest) locationViewer() locationViewer {
	return locationViewer{album: a.showLocation(), userID: a.P.UserID}
}

// sees reports whether v may see as.Location.
func (v locationViewer) sees(as meta.Asset) bool {
	return v.album && (as.LocationHiddenBy == "" || as.LocationHiddenBy == v.userID)
}

// assetView is an asset as a reader sees it, with its social summary
// (social.go) for the caller.
func assetView(as meta.Asset, viewer locationViewer, sum meta.SocialSummary) assetJSON {
	showLocation := viewer.sees(as)
	v := assetJSON{ID: as.ID, SHA256: as.SHA256, Filename: as.Filename, MimeType: as.MimeType, Bytes: as.Bytes,
		PreviewReady: as.PreviewReady, PreviewStatus: as.PreviewStatus, UploadedAt: as.UploadedAt,
		CapturedAt: nonEmpty(as.CapturedAt), UploadedByName: nonEmpty(as.UploaderName), MediaKind: as.MediaKind,
		Pairing: nonEmpty(as.Pairing), Components: componentsView(as),
		LikeCount: sum.LikeCount, LikedByMe: sum.LikedByMe, CommentCount: sum.CommentCount}
	if as.MediaKind != meta.MediaLivePhoto {
		v.DurationMS = positive(as.DurationMS)
	}
	if c := as.Camera; c != nil {
		v.Camera = &cameraJSON{Make: nonEmpty(c.Make), Model: nonEmpty(c.Model), Lens: nonEmpty(c.Lens)}
		if c.Focal35mm > 0 {
			f := c.Focal35mm
			v.Camera.Focal35mm = &f
		}
	}
	if as.Width > 0 && as.Height > 0 {
		w, h := as.Width, as.Height
		v.Width, v.Height = &w, &h
	}
	if l := as.Location; showLocation && l != nil {
		v.Location = &locationJSON{Lat: l.Lat, Lon: l.Lon}
		if l.AccuracyM > 0 {
			acc := l.AccuracyM
			v.Location.AccuracyM = &acc
		}
	}
	return v
}

// componentsView lists the extra originals of an asset (nil: none).
func componentsView(as meta.Asset) []componentJSON {
	if c := as.PairedVideo; c.SHA256 != "" {
		return []componentJSON{{Role: c.Role, SHA256: c.SHA256, Bytes: c.Bytes, MimeType: c.MimeType, Filename: c.Filename, DurationMS: positive(c.DurationMS)}}
	}
	return nil
}

// positive is nil for n <= 0 (JSON null).
func positive(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

// nonEmpty is nil for "" (JSON null).
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// syncEvents: GET /folders/{id}/sync?cursor=&limit=. Events after the
// cursor in seq order: asset_added, asset_updated and the social events
// likes_updated, comment_added, comment_updated and comment_removed (with
// the asset as it is now in "assets" while it is still in the folder) and
// asset_removed (a tombstone, no asset entry).
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
	ids := make([]string, 0, len(page.Assets))
	for id := range page.Assets {
		ids = append(ids, id)
	}
	sums, ok := s.summaries(w, r, a, ids)
	if !ok {
		return
	}
	assets := make(map[string]assetJSON, len(page.Assets))
	for id, as := range page.Assets {
		assets[id] = assetView(as, a.locationViewer(), sums[id])
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
	ids := make([]string, 0, len(page.Assets))
	for _, as := range page.Assets {
		ids = append(ids, as.ID)
	}
	sums, ok := s.summaries(w, r, a, ids)
	if !ok {
		return
	}
	list := make([]assetJSON, 0, len(page.Assets))
	next := ""
	for _, as := range page.Assets {
		list = append(list, assetView(as, a.locationViewer(), sums[as.ID]))
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
// folders, never the server as a whole). Advisory: the upload decides. A
// hash matches an asset's own original (a live photo's still), never a
// paired video.
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
		if m, ok := found[h]; ok {
			p := map[string]string{"sha256": h, "asset_id": m.AssetID}
			if m.PairedSHA256 != "" {
				// A live photo: the client can tell a still-only asset
				// (no paired_sha256), which a bundle upload completes.
				p["paired_sha256"] = m.PairedSHA256
			}
			present = append(present, p)
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
