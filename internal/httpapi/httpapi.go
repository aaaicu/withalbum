// Package httpapi serves the PoC HTTP API and the local dashboard.
//
// This is a LAN/localhost demonstration API, NOT a production service. In
// particular, it intentionally has NO AUTHENTICATION. Never publish its port.
package httpapi

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
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

// Config wires the server to its storage. All fields except Version are required.
type Config struct {
	Volume  *storage.Volume
	Meta    meta.MetadataStore
	Blobs   *blobstore.Store
	Version string
}

// Server holds the HTTP handlers.
type Server struct {
	vol     *storage.Volume
	meta    meta.MetadataStore
	blobs   *blobstore.Store
	version string
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	return &Server{vol: cfg.Volume, meta: cfg.Meta, blobs: cfg.Blobs, version: cfg.Version}
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
		jsonReply(w, 200, map[string]string{"status": "ok", "version": s.version, "storage": storageState})
	})
	mux.HandleFunc("GET /api/v1/folders", s.getFolders)
	mux.HandleFunc("POST /api/v1/folders", s.createFolder)
	mux.HandleFunc("POST /api/v1/folders/{folderID}/assets", s.upload)
	mux.HandleFunc("GET /api/v1/folders/{folderID}/sync", s.syncEvents)
	mux.HandleFunc("GET /api/v1/assets/{assetID}/original", s.download(false))
	mux.HandleFunc("GET /api/v1/assets/{assetID}/preview", s.download(true))
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
		errorReply(w, 500, "metadata_write_failed")
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
	// Fail fast before spooling the body to a volume that is already gone.
	if !s.storageReady(w) {
		return
	}

	staged, err := s.blobs.Stage(r.Body, MaxUploadSize)
	switch {
	case err == nil:
	case errors.Is(err, blobstore.ErrTempCreate):
		errorReply(w, 500, "temp_failed")
		return
	case errors.Is(err, blobstore.ErrTooLarge):
		errorReply(w, 413, "file_too_large")
		return
	case errors.Is(err, blobstore.ErrEmpty):
		errorReply(w, 400, "empty_file")
		return
	case errors.Is(err, blobstore.ErrSync):
		errorReply(w, 500, "file_sync_failed")
		return
	case errors.Is(err, blobstore.ErrClose):
		errorReply(w, 500, "temp_close_failed")
		return
	default: // blobstore.ErrCopy
		errorReply(w, 400, "upload_failed")
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
	deviceID := strings.TrimSpace(r.Header.Get("X-Device-ID"))
	if len(deviceID) > 100 {
		errorReply(w, 400, "invalid_device_id")
		return
	}

	previewReady := false
	existing, err := s.meta.GetAssetBySHA(ctx, sha)
	switch {
	case err == nil:
		previewReady = existing.PreviewReady
	case errors.Is(err, meta.ErrAssetNotFound):
		// Blob before metadata: the pool file must exist before CommitUpload
		// makes the asset visible. A crash in between leaves an orphan blob.
		if !s.storageReady(w) {
			return
		}
		if err := s.blobs.Promote(staged, sha); err != nil {
			errorReply(w, 500, "storage_failed")
			return
		}
		previewReady = s.blobs.MakePreview(sha) == nil
	default:
		metaReadFailed(w, err)
		return
	}
	// Re-check: the pool rename and preview above may have taken a while.
	if !s.storageReady(w) {
		return
	}
	res, err := s.meta.CommitUpload(ctx, meta.CommitRequest{
		FolderID: folderID,
		DeviceID: deviceID,
		Asset:    meta.Asset{ID: sha, Filename: name, MimeType: staged.MimeType, Bytes: staged.Size, PreviewReady: previewReady},
	})
	if errors.Is(err, meta.ErrFolderNotFound) {
		errorReply(w, 404, "folder_not_found")
		return
	}
	if err != nil {
		errorReply(w, 500, "metadata_write_failed")
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
