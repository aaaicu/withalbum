// Package httpapi serves the PoC HTTP API and the local dashboard.
//
// This is a LAN/localhost demonstration API, NOT a production service. Login,
// sessions and account administration exist (auth.go, middleware_auth.go),
// but the folder, upload, sync and download routes are NOT protected yet
// (M10). Never publish its port.
package httpapi

import (
	_ "embed"
	"encoding/hex"
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
	// Auth enables the login, session and account routes (NewAuth). nil:
	// they are not registered (stores without an account database).
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

// Handler returns the routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'")
		_, _ = w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /api/v1/folders", s.getFolders)
	mux.HandleFunc("POST /api/v1/folders", s.createFolder)
	mux.HandleFunc("POST /api/v1/folders/{folderID}/assets", s.upload)
	mux.HandleFunc("GET /api/v1/folders/{folderID}/sync", s.syncEvents)
	mux.HandleFunc("GET /api/v1/assets/{assetID}/original", s.download(false))
	mux.HandleFunc("GET /api/v1/assets/{assetID}/preview", s.download(true))
	if s.auth != nil {
		s.authRoutes(mux)
	}
	return mux
}

// metaReadFailed replies to an unexpected metadata read error.
func metaReadFailed(w http.ResponseWriter, err error) {
	log.Printf("metadata read failed: %v", err)
	errorReply(w, 500, "metadata_read_failed")
}
func (s *Server) getFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.meta.ListFolders(r.Context())
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	if folders == nil {
		folders = []meta.Folder{}
	}
	jsonReply(w, 200, map[string]any{"folders": folders})
}
func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
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
	folder, err := s.meta.CreateFolder(r.Context(), request.Name)
	if errors.Is(err, meta.ErrIDGeneration) {
		errorReply(w, 500, "random_failure")
		return
	}
	if err != nil {
		writeFailed(w, "create folder", err, "metadata_write_failed")
		return
	}
	jsonReply(w, 201, folder)
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
// which the volume and the pool file are re-checked right before COMMIT ->
// response. A success status is therefore only sent once file and metadata
// are both on disk. A crash or error before COMMIT leaves at most a temp file
// or an unreferenced pool file, which startup reconciliation removes or
// quarantines (blobstore.Reconcile); retrying the upload is always safe.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	folderID := r.PathValue("folderID")
	if _, err := s.meta.GetFolder(ctx, folderID); errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	} else if err != nil {
		metaReadFailed(w, err)
		return
	}
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
	// The experimental local dashboard percent-encodes non-ASCII filenames for headers.
	if decoded, err := url.QueryUnescape(filename); err == nil {
		filename = decoded
	}
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "" || name == "." || name == "/" || len(name) > 240 {
		name = sha
	}

	previewReady := false
	existing, err := s.meta.GetAssetBySHA(ctx, sha)
	switch {
	case err == nil:
		previewReady = existing.PreviewReady
		// The metadata knows this original but its pool file is gone (see
		// the startup report): these are the same bytes, restore it.
		if _, statErr := os.Lstat(s.blobs.PoolPath(sha)); errors.Is(statErr, os.ErrNotExist) {
			if !s.storageReady(w) {
				return
			}
			if err := s.blobs.Promote(staged, sha); err != nil {
				writeFailed(w, "restore missing original", err, "storage_failed")
				return
			}
			log.Printf("restored missing original %s from a new upload", sha)
		}
	case errors.Is(err, meta.ErrAssetNotFound):
		// Blob before metadata: the pool file must be durable before
		// CommitUpload makes the asset visible.
		if !s.storageReady(w) {
			return
		}
		err := s.blobs.Promote(staged, sha)
		if err == nil {
			err = s.fault(FaultAfterPromote)
		}
		if err != nil {
			writeFailed(w, "promote upload", err, "storage_failed")
			return
		}
		// The preview is derived data: its failure never fails the upload.
		if err := s.fault(FaultPreview); err == nil {
			previewReady = s.blobs.MakePreview(sha) == nil
		}
		if err := s.fault(FaultAfterPreview); err != nil {
			writeFailed(w, "upload", err, "storage_failed")
			return
		}
	default:
		metaReadFailed(w, err)
		return
	}
	// Re-check: the pool rename and preview above may have taken a while.
	if !s.storageReady(w) {
		return
	}
	poolPath := s.blobs.PoolPath(sha)
	res, err := s.meta.CommitUpload(ctx, meta.CommitRequest{
		FolderID: folderID,
		DeviceID: deviceID,
		Asset:    meta.Asset{ID: sha, Filename: name, MimeType: staged.MimeType, Bytes: staged.Size, PreviewReady: previewReady},
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
	if errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if err != nil {
		writeFailed(w, "commit upload", err, "metadata_write_failed")
		return
	}
	status := 201
	if res.Result == meta.ResultAlreadyPresent {
		status = 200
	}
	jsonReply(w, status, map[string]any{"result": res.Result, "asset_id": sha, "folder_id": folderID, "sha256": sha, "preview_ready": res.Asset.PreviewReady})
}
func (s *Server) syncEvents(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("folderID")
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
	if page.Events == nil {
		page.Events = []meta.Event{}
	}
	if page.Assets == nil {
		page.Assets = map[string]meta.Asset{}
	}
	jsonReply(w, 200, map[string]any{"folder_id": folderID, "events": page.Events, "assets": page.Assets,
		"next_cursor": page.NextCursor, "has_more": page.HasMore})
}
func (s *Server) download(preview bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("assetID")
		if len(sha) != 64 {
			errorReply(w, 400, "invalid_asset_id")
			return
		}
		if _, err := hex.DecodeString(sha); err != nil {
			errorReply(w, 400, "invalid_asset_id")
			return
		}
		asset, err := s.meta.GetAssetBySHA(r.Context(), sha)
		if errors.Is(err, meta.ErrAssetNotFound) {
			errorReply(w, 404, "asset_not_found")
			return
		}
		if err != nil {
			metaReadFailed(w, err)
			return
		}
		path := s.blobs.PoolPath(sha)
		if preview {
			if !asset.PreviewReady {
				errorReply(w, 404, "preview_not_ready")
				return
			}
			path = s.blobs.PreviewPath(sha)
			w.Header().Set("Content-Type", "image/jpeg")
		} else {
			w.Header().Set("Content-Type", asset.MimeType)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, path)
	}
}
