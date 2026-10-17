// Package httpapi serves the HTTP API and the local dashboard.
//
// Every route needs a session except the dashboard page, health, login and
// refresh, and every folder and asset route is authorized per request
// against the owner and memberships stored in the database
// (middleware_authz.go). There is no TLS yet (M15), so the server still
// listens on loopback only. Never publish its port.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// MaxUploadSize is the largest accepted single-request upload.
const MaxUploadSize int64 = 512 << 20

const syncPageSize = 100

//go:embed web/index.html
var dashboardHTML []byte

// DefaultMinFreeBytes is the free space an upload must leave on the volume
// (WITHALBUM_MIN_FREE_BYTES), so that the database, previews and the
// operating system never run out of room.
const DefaultMinFreeBytes int64 = 1 << 30

// Fault injection points, in the order an upload passes them. See
// FaultInjector.
const (
	FaultAfterStage   = "after_stage"   // temp file written and fsynced
	FaultAfterPromote = "after_promote" // renamed into the pool, directory fsynced
	FaultPreview      = "preview"       // replaces preview generation
	FaultAfterPreview = "after_preview" // preview done, metadata not yet committed
	FaultBeforeCommit = "before_commit" // inside the metadata transaction, before COMMIT
	FaultAfterCommit  = "after_commit"  // committed, response not yet sent
)

// FaultInjector is for tests only and must be nil in production. Fault is
// called at each point an upload passes; a non-nil error is handled exactly
// like an error of the step just before that point (FaultPreview: like a
// failed preview), so tests can simulate a full disk (syscall.ENOSPC), a
// dying disk (syscall.EIO) or a busy database (meta.ErrBusy). A test may
// also block in Fault and kill the process to simulate a crash there.
type FaultInjector interface {
	Fault(point string) error
}

// Config wires the server to its storage. Volume, Meta and Blobs are required.
type Config struct {
	Volume  *storage.Volume
	Meta    meta.MetadataStore
	Blobs   *blobstore.Store
	Version string
	// MinFreeBytes is the free space an upload must leave on the volume.
	// Uploads that would go below it are refused with 507 before anything
	// is written. Zero only requires that the upload itself fits.
	MinFreeBytes int64
	// FreeBytes reports the free space of the volume (nil: Volume.FreeBytes).
	FreeBytes func() (uint64, error)
	// IntegrityWarning makes /api/v1/health report "integrity": "warning",
	// e.g. when startup reconciliation found missing or damaged originals.
	IntegrityWarning bool
	// Faults is for tests only (see FaultInjector); nil in production.
	Faults FaultInjector
	// Auth holds the login, session and account services (NewAuth).
	// Required: Handler panics without it.
	Auth *Auth
}

// Server holds the HTTP handlers.
type Server struct {
	vol              *storage.Volume
	meta             meta.MetadataStore
	blobs            *blobstore.Store
	version          string
	minFree          int64
	freeBytes        func() (uint64, error)
	integrityWarning bool
	faults           FaultInjector
	auth             *Auth
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	s := &Server{vol: cfg.Volume, meta: cfg.Meta, blobs: cfg.Blobs, version: cfg.Version,
		minFree: max(cfg.MinFreeBytes, 0), freeBytes: cfg.FreeBytes, integrityWarning: cfg.IntegrityWarning, faults: cfg.Faults, auth: cfg.Auth}
	if s.freeBytes == nil {
		s.freeBytes = cfg.Volume.FreeBytes
	}
	return s
}

func (s *Server) fault(point string) error {
	if s.faults == nil {
		return nil
	}
	return s.faults.Fault(point)
}

func jsonReply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func errorReply(w http.ResponseWriter, status int, code string) {
	jsonReply(w, status, map[string]string{"error": code})
}

// Handler returns the routing table. Every route is registered through its
// declared access policy (middleware_authz.go). It panics if Config.Auth was
// nil: no folder or asset route exists without login and access control.
func (s *Server) Handler() http.Handler {
	if s.auth == nil {
		panic("httpapi: Config.Auth is required")
	}
	rt := newRouter(s)
	rt.handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// A static page without data; everything it shows comes from API
		// calls made after logging in.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		_, _ = w.Write(dashboardHTML)
	})
	rt.handle("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		// Report only ok/unavailable; never expose paths or volume ids here.
		storageState := "ok"
		if err := s.vol.Check(); err != nil {
			storageState = "unavailable"
		}
		integrity := "ok"
		if s.integrityWarning {
			integrity = "warning"
		}
		jsonReply(w, 200, map[string]string{"status": "ok", "version": s.version, "storage": storageState, "integrity": integrity})
	})
	rt.handle("/api/v1/", func(w http.ResponseWriter, r *http.Request) { errorReply(w, 404, "not_found") })
	s.authRoutes(rt)
	rt.handle("GET /api/v1/folders", s.getFolders)
	rt.handle("POST /api/v1/folders", s.createFolder)
	rt.handleAlbum("GET /api/v1/folders/{folderID}", s.getFolder)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/assets", s.upload)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/sync", s.syncEvents)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/assets/{assetID}/original", s.download(false))
	rt.handleAlbum("GET /api/v1/folders/{folderID}/assets/{assetID}/preview", s.download(true))
	return rt.finish()
}

// metaReadFailed replies to an unexpected metadata read error.
func metaReadFailed(w http.ResponseWriter, err error) {
	log.Printf("metadata read failed: %v", err)
	errorReply(w, 500, "metadata_read_failed")
}

// folderJSON is a folder as its owner or a member sees it: name, own role
// and what the role allows. It never names other members or files.
type folderJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Role      string `json:"role"`
	CanRead   bool   `json:"can_read"`
	CanUpload bool   `json:"can_upload"`
}

func folderView(p authz.Principal, fa meta.FolderAccess) folderJSON {
	return folderJSON{ID: fa.Folder.ID, Name: fa.Folder.Name, CreatedAt: fa.Folder.CreatedAt, Role: role(p.UserID, fa.Access),
		CanRead:   authz.Allowed(p, authz.ListAlbumAssets, &fa.Access),
		CanUpload: authz.Allowed(p, authz.UploadAsset, &fa.Access)}
}

// getFolders lists only the folders the caller owns or is a member of.
func (s *Server) getFolders(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	list, err := s.meta.ListFoldersFor(r.Context(), p.UserID)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	folders := []folderJSON{}
	for _, fa := range list {
		if authz.Allowed(p, authz.GetAlbumMetadata, &fa.Access) {
			folders = append(folders, folderView(p, fa))
		}
	}
	jsonReply(w, 200, map[string]any{"folders": folders})
}

func (s *Server) getFolder(w http.ResponseWriter, r *http.Request, a albumRequest) {
	jsonReply(w, 200, folderView(a.P, a.Folder))
}

// Denials found when an authorization is repeated inside a write
// transaction (meta.Authorizer).
var (
	errDenied = errors.New("permission denied")  // 403
	errHidden = errors.New("folder not visible") // 404
)

// recheck returns a meta.Authorizer that repeats the decision for action at
// commit time, and reports through canRead whether the caller could then
// read the folder.
func recheck(action authz.Action, canRead *bool) meta.Authorizer {
	return func(p authz.Principal, access authz.AlbumAccess) error {
		if canRead != nil {
			*canRead = authz.Allowed(p, authz.ListAlbumAssets, &access)
		}
		if authz.Allowed(p, action, &access) {
			return nil
		}
		if action != authz.CreateAlbum && !authz.Allowed(p, authz.GetAlbumMetadata, &access) {
			return errHidden
		}
		return errDenied
	}
}

// createFolder: SERVER_ADMIN or can_create_albums (checked by the route
// policy and again inside the transaction); the creator is the only owner.
func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	p := principal(id)
	var request struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&request); err != nil {
		errorReply(w, 400, "invalid_json")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if len(request.Name) == 0 || len([]rune(request.Name)) > 80 {
		errorReply(w, 400, "invalid_folder_name")
		return
	}
	if !s.storageReady(w) {
		return
	}
	folder, err := s.meta.CreateFolder(r.Context(), meta.NewFolder{Name: request.Name, OwnerID: p.UserID, SessionID: id.SessionID,
		Authorize: recheck(authz.CreateAlbum, nil)})
	switch {
	case errors.Is(err, errDenied):
		errorReply(w, 403, "forbidden")
		return
	case errors.Is(err, meta.ErrIDGeneration):
		errorReply(w, 500, "random_failure")
		return
	case err != nil:
		writeFailed(w, "create folder", err, "metadata_write_failed")
		return
	}
	jsonReply(w, 201, folderView(p, meta.FolderAccess{Folder: folder, Access: authz.AlbumAccess{OwnerID: p.UserID}}))
}

// storageReady re-verifies the volume right before a write. On failure it
// replies 503 storage_unavailable and the caller must not write anything.
func (s *Server) storageReady(w http.ResponseWriter) bool {
	if err := s.vol.Check(); err != nil {
		log.Printf("storage unavailable, refusing write: %v", err)
		errorReply(w, 503, "storage_unavailable")
		return false
	}
	return true
}

// writeFailed replies to a failed write. Errors that a client should handle
// differently from a plain server error get their own status:
//
//   - full volume (ENOSPC, EDQUOT, SQLITE_FULL): 507 insufficient_storage
//   - I/O error (EIO, SQLITE_IOERR): 503 storage_io_error
//   - database write lock not available in time: 503 busy, Retry-After: 1
//   - storage volume gone or replaced: 503 storage_unavailable
//
// Everything else is 500 with the given code.
func writeFailed(w http.ResponseWriter, what string, err error, code string) {
	log.Printf("%s failed: %v", what, err)
	switch {
	case storage.IsNoSpace(err):
		errorReply(w, 507, "insufficient_storage")
	case storage.IsIOError(err):
		errorReply(w, 503, "storage_io_error")
	case errors.Is(err, meta.ErrBusy):
		w.Header().Set("Retry-After", "1")
		errorReply(w, 503, "busy")
	case errors.Is(err, errVolumeLost):
		errorReply(w, 503, "storage_unavailable")
	default:
		errorReply(w, 500, code)
	}
}

// errVolumeLost marks a failed volume re-check inside the commit.
var errVolumeLost = errors.New("storage volume lost")

// uploadBudget returns how many bytes this upload may write: the free space
// minus the reserve, capped at MaxUploadSize. ok is false (and a reply was
// sent) when the volume cannot take any upload or cannot be queried.
func (s *Server) uploadBudget(w http.ResponseWriter) (int64, bool) {
	free, err := s.freeBytes()
	if errors.Is(err, storage.ErrFreeSpaceUnsupported) {
		return MaxUploadSize, true // no statfs on this platform; ENOSPC still maps to 507
	}
	if err != nil {
		log.Printf("cannot query free space, refusing write: %v", err)
		errorReply(w, 503, "storage_unavailable")
		return 0, false
	}
	if free <= uint64(s.minFree) {
		log.Printf("refusing upload: %d bytes free, reserve %d", free, s.minFree)
		errorReply(w, 507, "insufficient_storage")
		return 0, false
	}
	return int64(min(free-uint64(s.minFree), uint64(MaxUploadSize))), true
}

// upload stores one original. Commit order (each step durable before the
// next): temp file in incoming/ fsynced -> renamed into the pool and the pool
// directory fsynced -> preview (optional) -> one metadata transaction, inside
// which the caller's permission, the volume and the pool file are re-checked
// right before COMMIT -> response. A success status is therefore only sent
// once file and metadata are both on disk. A crash or error before COMMIT
// leaves at most a temp file or an unreferenced pool file, which startup
// reconciliation removes or quarantines (blobstore.Reconcile); retrying the
// upload is always safe.
//
// Whether the bytes are new to the server, already stored for another
// folder, or already in this folder does not change the steps taken before
// the commit: the pool file is promoted (or the existing one re-verified)
// and the preview generated in every case. The latency still differs by a
// few milliseconds (new directory entries and database rows must be synced,
// known ones not); that is a known, documented limit. Callers who can read the folder get
// "created" (201) or "already_present" (200) and the asset; a caller who may
// only upload (UPLOADER) always gets the same 202 receipt.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, a albumRequest) {
	ctx := r.Context()
	folderID := a.Folder.Folder.ID
	deviceID := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if len(deviceID) > 100 {
		errorReply(w, 400, "invalid_device_id")
		return
	}
	// Fail fast before spooling the body to a volume that is already gone.
	if !s.storageReady(w) {
		return
	}
	// Refuse before writing anything if the upload cannot fit. A body of
	// unknown length is limited to the budget while it is staged.
	budget, ok := s.uploadBudget(w)
	if !ok {
		return
	}
	if r.ContentLength > budget && r.ContentLength <= MaxUploadSize {
		errorReply(w, 507, "insufficient_storage")
		return
	}

	staged, err := s.blobs.Stage(r.Body, budget)
	if err == nil {
		if err = s.fault(FaultAfterStage); err != nil {
			staged.Remove()
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, blobstore.ErrTooLarge):
		if budget < MaxUploadSize && r.ContentLength <= MaxUploadSize {
			errorReply(w, 507, "insufficient_storage")
		} else {
			errorReply(w, 413, "file_too_large")
		}
		return
	case errors.Is(err, blobstore.ErrEmpty):
		errorReply(w, 400, "empty_file")
		return
	case errors.Is(err, blobstore.ErrCopy):
		errorReply(w, 400, "upload_failed")
		return
	case errors.Is(err, blobstore.ErrTempCreate):
		writeFailed(w, "stage upload", err, "temp_failed")
		return
	case errors.Is(err, blobstore.ErrClose):
		writeFailed(w, "stage upload", err, "temp_close_failed")
		return
	case errors.Is(err, blobstore.ErrWrite):
		writeFailed(w, "stage upload", err, "temp_write_failed")
		return
	default: // ErrSync, injected faults
		writeFailed(w, "stage upload", err, "file_sync_failed")
		return
	}
	defer staged.Remove()
	sha := staged.SHA256
	if declared := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Content-SHA256"))); declared != "" && declared != sha {
		errorReply(w, 422, "hash_mismatch")
		return
	}
	filename := r.Header.Get("X-Filename")
	// The local dashboard percent-encodes non-ASCII filenames for headers.
	if decoded, err := url.QueryUnescape(filename); err == nil {
		filename = decoded
	}
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "" || name == "." || name == "/" || len(name) > 240 {
		name = sha
	}

	// Blob before metadata: the pool file must be durable before
	// CommitUpload makes the asset visible. For bytes the pool already holds
	// Promote re-hashes the existing file (and puts it back if it is missing
	// or damaged), which also keeps the work the same as for new bytes.
	if !s.storageReady(w) {
		return
	}
	err = s.blobs.Promote(staged, sha)
	if err == nil {
		err = s.fault(FaultAfterPromote)
	}
	if err != nil {
		writeFailed(w, "promote upload", err, "storage_failed")
		return
	}
	// The preview is derived data: its failure never fails the upload. It is
	// (re)generated for known bytes too; the result is the same file.
	previewReady := false
	if err := s.fault(FaultPreview); err == nil {
		previewReady = s.blobs.MakePreview(sha) == nil
	}
	if err := s.fault(FaultAfterPreview); err != nil {
		writeFailed(w, "upload", err, "storage_failed")
		return
	}
	// Re-check: the pool rename and preview above may have taken a while.
	if !s.storageReady(w) {
		return
	}
	poolPath := s.blobs.PoolPath(sha)
	canRead := false
	res, err := s.meta.CommitUpload(ctx, meta.CommitRequest{
		FolderID:  folderID,
		DeviceID:  deviceID,
		UserID:    a.P.UserID,
		SessionID: a.ID.SessionID,
		Asset:     meta.Asset{SHA256: sha, Filename: name, MimeType: staged.MimeType, Bytes: staged.Size, PreviewReady: previewReady},
		// The permission checked when the request arrived may have been
		// revoked while the body was streaming: decide again on what is
		// stored now, under the database write lock.
		Authorize: recheck(authz.UploadAsset, &canRead),
		// Runs under the database write lock right before COMMIT, so the
		// window between this check and the commit is as small as possible.
		BeforeCommit: func() error {
			if err := s.fault(FaultBeforeCommit); err != nil {
				return err
			}
			if err := s.vol.Check(); err != nil {
				return fmt.Errorf("%w: %w", errVolumeLost, err)
			}
			fi, err := os.Stat(poolPath)
			if err != nil {
				return fmt.Errorf("pool file before commit: %w", err)
			}
			if fi.Size() != staged.Size {
				return fmt.Errorf("pool file has %d bytes before commit, want %d", fi.Size(), staged.Size)
			}
			return nil
		},
	})
	if err == nil {
		err = s.fault(FaultAfterCommit)
	}
	switch {
	case errors.Is(err, errHidden), errors.Is(err, meta.ErrFolderNotFound):
		log.Printf("upload refused at commit: no longer a member of the folder")
		errorReply(w, 404, "folder_not_found")
		return
	case errors.Is(err, errDenied):
		log.Printf("upload refused at commit: permission revoked during the upload")
		errorReply(w, 403, "forbidden")
		return
	case err != nil:
		writeFailed(w, "commit upload", err, "metadata_write_failed")
		return
	}
	if !canRead {
		// Upload-only: nothing about the folder's contents, the asset or
		// whether these bytes were already stored.
		jsonReply(w, 202, map[string]any{"result": "accepted", "folder_id": folderID, "sha256": sha, "bytes": staged.Size})
		return
	}
	status, result := 201, "created"
	if res.Result == meta.ResultAlreadyPresent {
		status, result = 200, "already_present"
	}
	jsonReply(w, status, map[string]any{"result": result, "asset_id": res.Asset.ID, "folder_id": folderID, "sha256": sha, "preview_ready": res.Asset.PreviewReady})
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

func (s *Server) syncEvents(w http.ResponseWriter, r *http.Request, a albumRequest) {
	folderID := a.Folder.Folder.ID
	cursor, err := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	if r.URL.Query().Get("cursor") == "" {
		cursor = 0
		err = nil
	}
	if err != nil || cursor < 0 {
		errorReply(w, 400, "invalid_cursor")
		return
	}
	page, err := s.meta.ListEvents(r.Context(), folderID, cursor, syncPageSize)
	if errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	events := page.Events
	if events == nil {
		events = []meta.Event{}
	}
	assets := make(map[string]assetJSON, len(page.Assets))
	for id, as := range page.Assets {
		assets[id] = assetJSON{ID: as.ID, SHA256: as.SHA256, Filename: as.Filename, MimeType: as.MimeType, Bytes: as.Bytes,
			PreviewReady: as.PreviewReady, UploadedAt: as.UploadedAt}
	}
	jsonReply(w, 200, map[string]any{"folder_id": folderID, "events": events, "assets": assets,
		"next_cursor": page.NextCursor, "has_more": page.HasMore})
}

// download serves an original or preview of an asset that is linked to the
// folder in the path (the route policy has checked the caller may read it).
// An asset ID of another folder, even one the caller can read, is 404.
func (s *Server) download(preview bool) albumHandler {
	return func(w http.ResponseWriter, r *http.Request, a albumRequest) {
		asset, err := s.meta.GetFolderAsset(r.Context(), a.Folder.Folder.ID, r.PathValue("assetID"))
		if errors.Is(err, meta.ErrAssetNotFound) {
			errorReply(w, 404, "asset_not_found")
			return
		}
		if err != nil {
			metaReadFailed(w, err)
			return
		}
		path := s.blobs.PoolPath(asset.SHA256)
		if preview {
			if !asset.PreviewReady {
				errorReply(w, 404, "preview_not_ready")
				return
			}
			path = s.blobs.PreviewPath(asset.SHA256)
			w.Header().Set("Content-Type", "image/jpeg")
		} else {
			w.Header().Set("Content-Type", asset.MimeType)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, path)
	}
}
