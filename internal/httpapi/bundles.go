package httpapi

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/livephoto"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Live Photo bundles. A Live Photo is a still (HEIC or JPEG) and its paired
// video (QuickTime), stored as one asset whose original is the still and
// whose paired_video component is the video. The client creates a bundle
// that declares both originals, sends each through its own upload session
// (the ordinary PATCH .../uploads/{uid}), and completes the bundle:
//
//	POST   /folders/{fid}/bundles                       create (Idempotency-Key)
//	GET    /folders/{fid}/bundles/{bid}                 state and offsets
//	DELETE /folders/{fid}/bundles/{bid}                 cancel (both sessions)
//	POST   /folders/{fid}/bundles/{bid}/complete        verify and commit both
//	GET    /folders/{fid}/assets/{aid}/components/{role}  download a component
//
// Like an upload session, a bundle is visible only to the account, device
// and folder that created it (any other ID is 404 bundle_not_found), and
// its Idempotency-Key names one request in the same scope as every other
// upload. Completion verifies the size and SHA-256 of both stored files,
// their types and that both carry the same Apple content identifier (when
// both carry one), moves both into the pool, and only then commits them in
// one metadata transaction that re-checks the permission and the volume:
// before that, no asset, event or receipt exists. An UPLOADER gets the
// same 202 receipt whatever the folder already held.

const bundleCompleteRoute = "POST /api/v1/folders/{folderID}/bundles/{bundleID}/complete"

// ServerFeatures are the optional abilities this server announces in GET
// /me, so that a client can tell an older server from this one:
// live_photo_bundles (POST .../bundles), folder_create_idempotency
// (Idempotency-Key on POST /folders), asset_duration (duration_ms on
// assets and components), asset_location (location on assets,
// hide_location on folders and PATCH /folders/{id}, and the account's own
// hide_my_location in GET /me "settings" and PATCH /me/settings),
// reactions_comments (likes, comments, mentions and /me/activity,
// social.go), asset_camera (camera, width and height on assets) and
// video_playback (playback on assets and GET .../assets/{aid}/playback,
// playback.go), album_management (album_management.go) and sync_state
// (GET /sync/state: every folder's cursor and digest in one request, with
// a long poll, sync_state.go).
var ServerFeatures = []string{"live_photo_bundles", "folder_create_idempotency", "asset_duration", "asset_location", "reactions_comments", "asset_camera", "video_playback", "album_management", "sync_state"}

func (s *Server) bundleRoutes(rt *router) {
	rt.handleAlbum("POST /api/v1/folders/{folderID}/bundles", s.createBundle)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/bundles/{bundleID}", s.withBundle(false, s.getBundle))
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/bundles/{bundleID}", s.withBundle(true, s.cancelBundle))
	rt.handleAlbum(bundleCompleteRoute, s.withBundle(true, s.completeBundle))
	rt.handleAlbum("GET /api/v1/folders/{folderID}/assets/{assetID}/components/{role}", s.downloadComponent)
}

// bundleView is a bundle as its owner sees it.
func bundleView(b meta.Bundle) map[string]any {
	comps := []map[string]any{}
	for _, c := range b.Components {
		comps = append(comps, map[string]any{"role": c.Role, "upload_id": c.ID, "bytes": c.TotalBytes, "sha256": c.SHA256,
			"offset": c.Received, "status": c.Status, "max_chunk_bytes": MaxChunkSize})
	}
	return map[string]any{"bundle_id": b.ID, "folder_id": b.FolderID, "kind": b.Kind, "status": b.Status, "expires_at": b.ExpiresAt,
		"components": comps}
}

// componentOffsets is the body of 409 upload_incomplete for a bundle.
func componentOffsets(b meta.Bundle) map[string]any {
	comps := []map[string]any{}
	for _, c := range b.Components {
		comps = append(comps, map[string]any{"role": c.Role, "upload_id": c.ID, "offset": c.Received, "bytes": c.TotalBytes})
	}
	return map[string]any{"error": "upload_incomplete", "components": comps}
}

// createBundle: POST /folders/{fid}/bundles {"kind": "live_photo",
// "components": [{"role", "bytes", "sha256", "filename"}, ...]}.
func (s *Server) createBundle(w http.ResponseWriter, r *http.Request, a albumRequest) {
	deviceLabel := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if len(deviceLabel) > 100 || !plainLabel(deviceLabel) {
		errorReply(w, 400, "invalid_device_id")
		return
	}
	key := r.Header.Get(headerIdempotencyKey)
	if key == "" {
		errorReply(w, 400, "idempotency_key_required")
		return
	}
	if !validIdempotencyKey(key) {
		errorReply(w, 400, "invalid_idempotency_key")
		return
	}
	var req struct {
		Kind       string `json:"kind"`
		Components []struct {
			Role     string `json:"role"`
			Bytes    int64  `json:"bytes"`
			SHA256   string `json:"sha256"`
			Filename string `json:"filename"`
		} `json:"components"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind != meta.BundleLivePhoto {
		errorReply(w, 400, "invalid_kind")
		return
	}
	var comps []meta.NewBundleComponent
	roles := map[string]int{}
	var total int64
	for _, c := range req.Components {
		roles[c.Role]++
		sha := strings.ToLower(strings.TrimSpace(c.SHA256))
		limit := MaxResumableUploadSize
		if c.Role == meta.RolePairedVideo {
			limit = meta.MaxPairedVideoSize
		}
		switch {
		case c.Role != meta.RoleStill && c.Role != meta.RolePairedVideo:
			errorReply(w, 400, "invalid_components")
			return
		case c.Bytes <= 0:
			errorReply(w, 400, "invalid_bytes")
			return
		case c.Bytes > limit:
			errorReply(w, 413, "file_too_large")
			return
		case !sha256RE.MatchString(sha):
			errorReply(w, 400, "invalid_sha256")
			return
		}
		total += c.Bytes
		comps = append(comps, meta.NewBundleComponent{Role: c.Role, SHA256: sha, Bytes: c.Bytes, Filename: storedFilename(c.Filename, sha)})
	}
	if len(comps) != 2 || roles[meta.RoleStill] != 1 || roles[meta.RolePairedVideo] != 1 {
		errorReply(w, 400, "invalid_components")
		return
	}
	if !s.storageReady(w) {
		return
	}
	canRead := false
	b, created, replaced, err := s.meta.CreateBundle(r.Context(), meta.NewBundle{
		UserID: a.P.UserID, SessionID: a.ID.SessionID, DeviceID: a.ID.DeviceID, FolderID: a.Folder.Folder.ID,
		Key: key, Kind: req.Kind, DeviceLabel: deviceLabel, Components: comps,
		Authorize: recheck(authz.UploadAsset, &canRead),
		Reserve: func(reserved int64) error {
			free, err := s.freeBytes()
			if errors.Is(err, storage.ErrFreeSpaceUnsupported) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%w: %w", errVolumeLost, err)
			}
			if free <= uint64(s.minFree) || free-uint64(s.minFree) < uint64(max(reserved, 0))+uint64(total) {
				log.Printf("refusing upload bundle of %d bytes: %d bytes free, reserve %d, open sessions expect %d", total, free, s.minFree, reserved)
				return errNoRoom
			}
			return nil
		},
	})
	var committed *meta.UploadCommittedError
	switch {
	case err == nil:
	case errors.Is(err, errNoRoom):
		errorReply(w, 507, "insufficient_storage")
		return
	case errors.Is(err, meta.ErrIdempotencyMismatch):
		errorReply(w, 422, "idempotency_key_reused")
		return
	case errors.As(err, &committed):
		// Committed already and the bundle is gone: the stored result,
		// as a replayed completion, under the caller's current role.
		bundleReply(w, a.Folder.Folder.ID, comps, committed.Result, canRead)
		return
	case errors.Is(err, meta.ErrTooManyUploads):
		errorReply(w, 429, "too_many_uploads")
		return
	case errors.Is(err, errHidden), errors.Is(err, meta.ErrFolderNotFound):
		errorReply(w, 404, "folder_not_found")
		return
	case errors.Is(err, errDenied):
		errorReply(w, 403, "forbidden")
		return
	default:
		writeFailed(w, "create upload bundle", err, "metadata_write_failed")
		return
	}
	for _, id := range replaced {
		if err := s.blobs.RemovePart(id); err != nil {
			log.Printf("upload session %s: cannot remove the expired partial file: %v", id, err)
		}
	}
	status := 200
	if created {
		status = 201
	}
	jsonReply(w, status, bundleView(b))
}

type bundleHandler func(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.BundleRef, b meta.Bundle)

// withBundle loads the caller's bundle named in the path (404
// bundle_not_found for any other) and, for writes, holds the locks of the
// bundle and of its component sessions while h runs (409 upload_busy while
// another request has one of them, a chunk upload included). A write
// reads the bundle again once it holds the locks.
func (s *Server) withBundle(write bool, h bundleHandler) albumHandler {
	return func(w http.ResponseWriter, r *http.Request, a albumRequest) {
		ref := meta.BundleRef{ID: r.PathValue("bundleID"), UserID: a.P.UserID, DeviceID: a.ID.DeviceID, FolderID: a.Folder.Folder.ID}
		if !assetIDRE.MatchString(ref.ID) {
			errorReply(w, 404, "bundle_not_found")
			return
		}
		busy := func() {
			w.Header().Set("Retry-After", "1")
			errorReply(w, 409, "upload_busy")
		}
		if write {
			if !s.uploads.acquire("bundle/" + ref.ID) {
				busy()
				return
			}
			defer s.uploads.release("bundle/" + ref.ID)
		}
		b, err := s.meta.GetBundle(r.Context(), ref)
		if errors.Is(err, meta.ErrBundleNotFound) {
			errorReply(w, 404, "bundle_not_found")
			return
		}
		if err != nil {
			metaReadFailed(w, err)
			return
		}
		if write {
			for _, c := range b.Components {
				if !s.uploads.acquire(c.ID) {
					busy()
					return
				}
				defer s.uploads.release(c.ID)
			}
			if b, err = s.meta.GetBundle(r.Context(), ref); errors.Is(err, meta.ErrBundleNotFound) {
				errorReply(w, 404, "bundle_not_found")
				return
			} else if err != nil {
				metaReadFailed(w, err)
				return
			}
		}
		h(w, r, a, ref, b)
	}
}

func (s *Server) getBundle(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.BundleRef, b meta.Bundle) {
	jsonReply(w, 200, bundleView(b))
}

// cancelBundle deletes the bundle, its sessions and their partial files.
// A completed bundle is only forgotten; its asset stays committed.
func (s *Server) cancelBundle(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.BundleRef, b meta.Bundle) {
	if !s.storageReady(w) {
		return
	}
	ids, err := s.meta.DeleteBundle(r.Context(), ref)
	if errors.Is(err, meta.ErrBundleNotFound) {
		errorReply(w, 404, "bundle_not_found")
		return
	} else if err != nil {
		writeFailed(w, "cancel upload bundle", err, "metadata_write_failed")
		return
	}
	for _, id := range ids {
		if err := s.blobs.RemovePart(id); err != nil {
			log.Printf("upload session %s: cannot remove the partial file (removed at the next start): %v", id, err)
		}
	}
	w.WriteHeader(204)
}

// stagedPath is where a verified component's bytes are: its partial file,
// or the pool when an earlier completion already moved it there.
func (s *Server) stagedPath(f *blobstore.StagedFile) string {
	if _, err := os.Lstat(f.Path); err == nil {
		return f.Path
	}
	return s.blobs.PoolPath(f.SHA256)
}

// completeBundle verifies both components and commits them as one asset.
// A completed bundle answers with the stored result again.
func (s *Server) completeBundle(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.BundleRef, b meta.Bundle) {
	ctx := r.Context()
	folderID := a.Folder.Folder.ID
	still, ok1 := b.Component(meta.RoleStill)
	video, ok2 := b.Component(meta.RolePairedVideo)
	if !ok1 || !ok2 {
		log.Printf("upload bundle %s: components missing", b.ID)
		errorReply(w, 404, "bundle_not_found")
		return
	}
	declared := []meta.NewBundleComponent{{Role: meta.RoleStill, SHA256: still.SHA256, Bytes: still.TotalBytes},
		{Role: meta.RolePairedVideo, SHA256: video.SHA256, Bytes: video.TotalBytes}}
	canRead := false
	commit := meta.BundleCommit{Ref: ref, SessionID: a.ID.SessionID, DeviceLabel: b.DeviceLabel,
		Still:     meta.Asset{SHA256: still.SHA256, Bytes: still.TotalBytes, Filename: still.Filename},
		Video:     meta.Component{Role: meta.RolePairedVideo, SHA256: video.SHA256, Bytes: video.TotalBytes, Filename: video.Filename},
		Pairing:   meta.PairingUnverified,
		Authorize: recheck(authz.UploadAsset, &canRead),
		// An upload-only caller must not learn that the folder holds this
		// still with another video: its Live Photo becomes an asset of its
		// own, and it gets the same 202 receipt.
		AddOnConflict: func() bool { return !canRead },
	}
	if b.Status == meta.UploadCompleted {
		// The receipt outlives the bundle; the commit only replays it.
		res, err := s.meta.CommitBundle(ctx, commit)
		if !s.bundleCommitError(w, r, a, err) {
			return
		}
		bundleReply(w, folderID, declared, res, canRead)
		return
	}
	if still.Received != still.TotalBytes || video.Received != video.TotalBytes {
		jsonReply(w, 409, componentOffsets(b))
		return
	}
	if !s.storageReady(w) {
		return
	}
	staged := map[string]*blobstore.StagedFile{}
	for _, c := range b.Components {
		f, err := s.blobs.FinishPart(c.ID, c.SHA256, c.TotalBytes)
		uref := meta.UploadRef{ID: c.ID, UserID: ref.UserID, DeviceID: ref.DeviceID, FolderID: ref.FolderID}
		switch {
		case err == nil:
			staged[c.Role] = f
			continue
		case errors.Is(err, blobstore.ErrPartMismatch):
			// That component was damaged on the way; the other one keeps
			// its bytes.
			if err := s.restartUpload(ctx, uref, c.Received); err != nil {
				writeFailed(w, "restart upload session", err, "storage_failed")
				return
			}
			jsonReply(w, 422, map[string]any{"error": "hash_mismatch", "role": c.Role, "upload_id": c.ID})
			return
		case errors.Is(err, blobstore.ErrPartMissing):
			if err := s.restartUpload(ctx, uref, c.Received); err != nil {
				writeFailed(w, "restart upload session", err, "storage_failed")
				return
			}
			log.Printf("upload session %s: partial file missing at completion; restarting at 0", c.ID)
			if b, err = s.meta.GetBundle(ctx, ref); err != nil {
				metaReadFailed(w, err)
				return
			}
			jsonReply(w, 409, componentOffsets(b))
			return
		default:
			writeFailed(w, "verify upload session", err, "storage_failed")
			return
		}
	}
	sf, vf := staged[meta.RoleStill], staged[meta.RolePairedVideo]
	for _, t := range []struct {
		role string
		ok   bool
	}{{meta.RoleStill, livephoto.StillType(sf.MimeType)}, {meta.RolePairedVideo, livephoto.VideoType(vf.MimeType)}} {
		if !t.ok {
			jsonReply(w, 415, map[string]any{"error": "unsupported_component_type", "role": t.role})
			return
		}
	}
	// The pairing is read from the bytes just verified; it never depends
	// on what the folder holds.
	sid, err := livephoto.StillIdentifierFile(s.stagedPath(sf))
	if err != nil {
		writeFailed(w, "read live photo still", err, "storage_failed")
		return
	}
	vid, err := livephoto.VideoIdentifierFile(s.stagedPath(vf))
	if err != nil {
		writeFailed(w, "read live photo video", err, "storage_failed")
		return
	}
	pairing, err := livephoto.Pair(sid, vid)
	if err != nil {
		errorReply(w, 422, "pairing_mismatch")
		return
	}
	commit.Pairing, commit.PairingID = pairing, sid
	if pairing == livephoto.Unverified {
		commit.PairingID = ""
	}
	// Both originals durable in the pool before the metadata commit.
	if !s.storageReady(w) || !s.promote(w, sf) || !s.promote(w, vf) {
		return
	}
	previewStatus := s.makePreview(sf)
	if err := s.fault(FaultAfterPreview); err != nil {
		writeFailed(w, "upload", err, "storage_failed")
		return
	}
	if !s.storageReady(w) {
		return
	}
	commit.Still = meta.Asset{SHA256: sf.SHA256, Filename: still.Filename, MimeType: sf.MimeType, Bytes: sf.Size,
		PreviewReady: previewStatus == meta.PreviewStatusReady, PreviewStatus: previewStatus}
	commit.Video.MimeType = vf.MimeType
	commit.BeforeCommit = func() error {
		if err := s.fault(FaultBeforeCommit); err != nil {
			return err
		}
		if err := s.vol.Check(); err != nil {
			return fmt.Errorf("%w: %w", errVolumeLost, err)
		}
		for _, f := range []*blobstore.StagedFile{sf, vf} {
			fi, err := os.Stat(s.blobs.PoolPath(f.SHA256))
			if err != nil {
				return fmt.Errorf("pool file before commit: %w", err)
			}
			if fi.Size() != f.Size {
				return fmt.Errorf("pool file has %d bytes before commit, want %d", fi.Size(), f.Size)
			}
		}
		return nil
	}
	res, err := s.meta.CommitBundle(ctx, commit)
	if err == nil {
		err = s.fault(FaultAfterCommit)
	}
	if !s.bundleCommitError(w, r, a, err) {
		return
	}
	if previewStatus == meta.PreviewStatusPending {
		s.kickPreviews()
	}
	s.kickCaptures()
	for _, c := range b.Components {
		if err := s.blobs.RemovePart(c.ID); err != nil {
			log.Printf("upload session %s: cannot remove the partial file (removed at the next start): %v", c.ID, err)
		}
	}
	bundleReply(w, folderID, declared, res, canRead)
}

// bundleCommitError replies to a failed CommitBundle and returns false, or
// returns true when err is nil.
func (s *Server) bundleCommitError(w http.ResponseWriter, r *http.Request, a albumRequest, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, meta.ErrComponentConflict):
		errorReply(w, 409, "component_conflict")
	case errors.Is(err, meta.ErrBundleNotFound):
		errorReply(w, 404, "bundle_not_found")
	default:
		return s.uploadCommitError(w, r, a, bundleCompleteRoute, err)
	}
	return false
}

// bundleReply answers a committed (or replayed) bundle. Readers of the
// folder get the asset; an upload-only caller (UPLOADER) gets the same 202
// receipt whatever happened, which only repeats what it sent.
func bundleReply(w http.ResponseWriter, folderID string, declared []meta.NewBundleComponent, res meta.CommitResult, canRead bool) {
	if res.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	var still meta.NewBundleComponent
	extra := []map[string]any{}
	for _, c := range declared {
		if c.Role == meta.RoleStill {
			still = c
		} else {
			extra = append(extra, map[string]any{"role": c.Role, "sha256": c.SHA256, "bytes": c.Bytes})
		}
	}
	if !canRead {
		jsonReply(w, 202, map[string]any{"result": "accepted", "folder_id": folderID, "sha256": still.SHA256, "bytes": still.Bytes,
			"components": extra})
		return
	}
	status := 200
	if res.Result == meta.ResultCreated || res.Result == meta.ResultLinked {
		status, res.Result = 201, meta.ResultCreated
	}
	as := res.Asset
	jsonReply(w, status, map[string]any{"result": res.Result, "asset_id": as.ID, "folder_id": folderID, "sha256": as.SHA256,
		"media_kind": as.MediaKind, "pairing": nonEmpty(as.Pairing), "components": componentsView(as),
		"preview_ready": as.PreviewReady, "preview_status": as.PreviewStatus})
}

// downloadComponent serves an extra original of an asset linked to the
// folder in the path (readers only, like /original): role paired_video
// of a live photo. Range requests work as for /original.
func (s *Server) downloadComponent(w http.ResponseWriter, r *http.Request, a albumRequest) {
	asset, err := s.meta.GetFolderAsset(r.Context(), a.Folder.Folder.ID, r.PathValue("assetID"), a.view())
	if errors.Is(err, meta.ErrAssetNotFound) {
		errorReply(w, 404, "asset_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	c := asset.PairedVideo
	if c.SHA256 == "" || r.PathValue("role") != c.Role {
		errorReply(w, 404, "component_not_found")
		return
	}
	serveOriginal(w, r, s.blobs.PoolPath(c.SHA256), c.MimeType, c.Filename)
}
