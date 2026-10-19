// Package httpapi serves the HTTP API and the local dashboard.
//
// Every route needs a session except the dashboard page, health, login and
// refresh, and every folder and asset route is authorized per request
// against the owner and memberships stored in the database
// (middleware_authz.go). The server speaks plain HTTP on loopback only;
// on a LAN address it requires TLS (server/main.go, internal/tlsid). Never
// publish its port to the internet.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// MaxUploadSize is the largest accepted single-request upload.
const MaxUploadSize int64 = 512 << 20

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
	// FaultMembershipBeforeCommit: inside the transaction of an invitation,
	// membership, ownership or archive change, everything prepared, before
	// COMMIT; an error rolls the change back.
	FaultMembershipBeforeCommit = "membership_before_commit"
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
	// Members serves invitations, memberships and ownership changes. nil:
	// Meta, which must then implement meta.Memberships (Handler panics
	// otherwise).
	Members meta.Memberships
	// TLS is the server's TLS identity shown to administrators (nil: no
	// TLS). It can also be set later with SetTLSInfo.
	TLS *TLSInfo
	// StartupCheck is the startup reconciliation result shown to
	// administrators (nil: not reported).
	StartupCheck *StartupCheck
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
	members          meta.Memberships
	tls              *TLSInfo
	startup          *StartupCheck
	denials          *denialLog
	uploads          uploadLocks
	previewKick      chan struct{} // wakes the preview worker (capacity 1)
	captureKick      chan struct{} // wakes the capture-time worker (capacity 1)
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	s := &Server{vol: cfg.Volume, meta: cfg.Meta, blobs: cfg.Blobs, version: cfg.Version,
		minFree: max(cfg.MinFreeBytes, 0), freeBytes: cfg.FreeBytes, integrityWarning: cfg.IntegrityWarning, faults: cfg.Faults, auth: cfg.Auth,
		members: cfg.Members, tls: cfg.TLS, startup: cfg.StartupCheck, previewKick: make(chan struct{}, 1),
		captureKick: make(chan struct{}, 1)}
	if s.members == nil {
		s.members, _ = cfg.Meta.(meta.Memberships)
	}
	if s.freeBytes == nil {
		s.freeBytes = cfg.Volume.FreeBytes
	}
	if cfg.Auth != nil {
		s.denials = newDenialLog(cfg.Auth.db)
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
	if s.members == nil {
		panic("httpapi: Config.Members is required")
	}
	rt := newRouter(s)
	rt.handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// A static page without data; everything it shows comes from API
		// calls made after logging in.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", dashboardCSP)
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
	rt.handleAlbum(uploadRoute, s.upload)
	s.uploadSessionRoutes(rt)
	s.bundleRoutes(rt)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/assets/{assetID}/original", s.download(false))
	rt.handleAlbum("GET /api/v1/folders/{folderID}/assets/{assetID}/preview", s.download(true))
	s.syncRoutes(rt)
	s.memberRoutes(rt)
	s.adminRoutes(rt)
	return securityHeaders(rt.finish())
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
	// MemberCount (owner and members whose accounts are active; no
	// pending invitations) only for callers who may read the folder: an
	// upload-only member learns nothing about the album beyond its name.
	MemberCount *int `json:"member_count,omitempty"`
}

func folderView(p authz.Principal, fa meta.FolderAccess) folderJSON {
	v := folderJSON{ID: fa.Folder.ID, Name: fa.Folder.Name, CreatedAt: fa.Folder.CreatedAt, Role: role(p.UserID, fa.Access),
		CanRead:   authz.Allowed(p, authz.ListAlbumAssets, &fa.Access),
		CanUpload: authz.Allowed(p, authz.UploadAsset, &fa.Access)}
	if v.CanRead {
		n := fa.MemberCount
		v.MemberCount = &n
	}
	return v
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
	// A new folder has its creator (active, just re-checked) and nobody else.
	jsonReply(w, 201, folderView(p, meta.FolderAccess{Folder: folder, Access: authz.AlbumAccess{OwnerID: p.UserID}, MemberCount: 1}))
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
// minus the reserve and minus what open upload sessions other than
// exceptUpload still expect, capped at limit. ok is false (and a reply was
// sent) when the volume cannot take any upload or cannot be queried.
func (s *Server) uploadBudget(w http.ResponseWriter, r *http.Request, exceptUpload string, limit int64) (int64, bool) {
	free, err := s.freeBytes()
	if errors.Is(err, storage.ErrFreeSpaceUnsupported) {
		return limit, true // no statfs on this platform; ENOSPC still maps to 507
	}
	if err != nil {
		log.Printf("cannot query free space, refusing write: %v", err)
		errorReply(w, 503, "storage_unavailable")
		return 0, false
	}
	reserved, err := s.meta.ReservedUploadBytes(r.Context(), exceptUpload)
	if err != nil {
		metaReadFailed(w, err)
		return 0, false
	}
	need := uint64(s.minFree) + uint64(max(reserved, 0))
	if free <= need {
		log.Printf("refusing upload: %d bytes free, reserve %d, open upload sessions expect %d", free, s.minFree, reserved)
		errorReply(w, 507, "insufficient_storage")
		return 0, false
	}
	return int64(min(free-need, uint64(limit))), true
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
// and the preview generated in every case, and every outcome commits a
// receipt row (Idempotency-Key), so the database write and its sync happen
// for known bytes too. The latency can still differ slightly (a new pool
// file needs new directory entries synced); that is a known, documented
// limit. Callers who can read the folder get
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
	key := r.Header.Get(headerIdempotencyKey)
	if key == "" {
		errorReply(w, 400, "idempotency_key_required")
		return
	}
	if !validIdempotencyKey(key) {
		errorReply(w, 400, "invalid_idempotency_key")
		return
	}
	// Scoped to this account, its session's device and this folder.
	idem := meta.Idempotency{DeviceID: a.ID.DeviceID, Key: key}
	// Fail fast before spooling the body to a volume that is already gone.
	if !s.storageReady(w) {
		return
	}
	// Refuse before writing anything if the upload cannot fit. A body of
	// unknown length is limited to the budget while it is staged.
	budget, ok := s.uploadBudget(w, r, "", MaxUploadSize)
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
	name := uploadFilename(r.Header.Get("X-Filename"), sha)
	// The same key with other bytes is a client error; refuse it before the
	// bytes reach the pool. A retry with the same bytes goes through every
	// step again (same work as a first attempt) and the commit then answers
	// what was stored the first time.
	if rc, err := s.meta.LookupReceipt(ctx, a.P.UserID, folderID, idem); err == nil && rc.SHA256 != sha {
		errorReply(w, 422, "idempotency_key_reused")
		return
	} else if err != nil && !errors.Is(err, meta.ErrReceiptNotFound) {
		metaReadFailed(w, err)
		return
	}
	s.commitOriginal(w, r, a, original{staged: staged, name: name, deviceLabel: deviceID, idem: idem, route: uploadRoute})
}

// uploadFilename is the stored name of an upload from the X-Filename
// header (percent-encoding allowed), see storedFilename.
func uploadFilename(filename, sha string) string {
	// The local dashboard percent-encodes non-ASCII filenames for headers.
	if decoded, err := url.QueryUnescape(filename); err == nil {
		filename = decoded
	}
	return storedFilename(filename, sha)
}

// storedFilename is the last path element of the client's name, or the
// hash when there is no usable name.
func storedFilename(filename, sha string) string {
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if name == "" || name == "." || name == "/" || len(name) > 240 {
		name = sha
	}
	return name
}

// original is a complete, verified original on its way into a folder.
type original struct {
	staged      *blobstore.StagedFile
	name        string
	deviceLabel string
	idem        meta.Idempotency
	uploadID    string // the upload session it came from, if any
	route       string // the route pattern, for the denial log
}

// commitOriginal stores a staged original (single request or completed
// upload session) and replies. It never removes the staged file; the
// caller does (a single upload's temp file always, an upload session's
// partial file only after a successful commit). It reports whether the
// original was committed.
func (s *Server) commitOriginal(w http.ResponseWriter, r *http.Request, a albumRequest, o original) bool {
	ctx := r.Context()
	folderID := a.Folder.Folder.ID
	staged, sha := o.staged, o.staged.SHA256
	// Blob before metadata: the pool file must be durable before
	// CommitUpload makes the asset visible. For bytes the pool already holds
	// Promote re-hashes the existing file (and puts it back if it is missing
	// or damaged), which also keeps the work the same as for new bytes.
	if !s.storageReady(w) || !s.promote(w, staged) {
		return false
	}
	previewStatus := s.makePreview(staged)
	if err := s.fault(FaultAfterPreview); err != nil {
		writeFailed(w, "upload", err, "storage_failed")
		return false
	}
	// Re-check: the pool rename and preview above may have taken a while.
	if !s.storageReady(w) {
		return false
	}
	poolPath := s.blobs.PoolPath(sha)
	canRead := false
	idem := o.idem
	res, err := s.meta.CommitUpload(ctx, meta.CommitRequest{
		FolderID:  folderID,
		DeviceID:  o.deviceLabel,
		UserID:    a.P.UserID,
		SessionID: a.ID.SessionID,
		Asset: meta.Asset{SHA256: sha, Filename: o.name, MimeType: staged.MimeType, Bytes: staged.Size,
			PreviewReady: previewStatus == meta.PreviewStatusReady, PreviewStatus: previewStatus},
		// Every outcome writes its receipt, so a new asset and bytes the
		// folder already holds take the same write path.
		Idempotency:     &idem,
		UploadSessionID: o.uploadID,
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
	if !s.uploadCommitError(w, r, a, o.route, err) {
		return false
	}
	if previewStatus == meta.PreviewStatusPending {
		s.kickPreviews()
	}
	// Every outcome wakes the capture-time worker (it finds nothing to do
	// for bytes it has already read).
	s.kickCaptures()
	uploadReply(w, folderID, sha, staged.Size, res, canRead)
	return true
}

// promote moves a verified original into the pool (or re-verifies the
// pool's copy of the same bytes). On failure it replies and returns false.
func (s *Server) promote(w http.ResponseWriter, staged *blobstore.StagedFile) bool {
	err := s.blobs.Promote(staged, staged.SHA256)
	if err == nil {
		err = s.fault(FaultAfterPromote)
	}
	if err != nil {
		writeFailed(w, "promote upload", err, "storage_failed")
		return false
	}
	return true
}

// makePreview makes the preview of a promoted original whose type has a
// built-in converter and returns the preview status to commit. The preview
// is derived data: its failure never fails the upload. It is (re)generated
// for known bytes too; the result is the same file. Types an external tool
// converts are left to the preview worker after the commit (pending), for
// new and known bytes alike.
func (s *Server) makePreview(staged *blobstore.StagedFile) string {
	switch s.blobs.PreviewMode(staged.MimeType) {
	case blobstore.PreviewBuiltin:
		if err := s.fault(FaultPreview); err == nil && s.blobs.MakePreview(staged.SHA256) == nil {
			return meta.PreviewStatusReady
		}
		return meta.PreviewStatusFailed
	case blobstore.PreviewExternal:
		return meta.PreviewStatusPending
	}
	return meta.PreviewStatusUnsupported
}

// uploadCommitError replies to a failed CommitUpload and returns false, or
// returns true when err is nil.
func (s *Server) uploadCommitError(w http.ResponseWriter, r *http.Request, a albumRequest, route string, err error) bool {
	folderID := a.Folder.Folder.ID
	switch {
	case err == nil:
		return true
	case errors.Is(err, errHidden), errors.Is(err, meta.ErrFolderNotFound):
		log.Printf("upload refused at commit: no longer a member of the folder")
		s.denied(r, a.P.UserID, route, 404, folderID)
		errorReply(w, 404, "folder_not_found")
	case errors.Is(err, errDenied):
		log.Printf("upload refused at commit: permission revoked during the upload")
		s.denied(r, a.P.UserID, route, 403, folderID)
		errorReply(w, 403, "forbidden")
	case errors.Is(err, meta.ErrIdempotencyMismatch):
		errorReply(w, 422, "idempotency_key_reused")
	case errors.Is(err, meta.ErrUploadNotFound):
		errorReply(w, 404, "upload_not_found")
	default:
		writeFailed(w, "commit upload", err, "metadata_write_failed")
	}
	return false
}

// uploadReply answers a committed (or replayed) upload. Callers who can
// read the folder get the asset; an upload-only caller (UPLOADER) gets the
// same 202 receipt whatever happened, which only repeats what it sent.
func uploadReply(w http.ResponseWriter, folderID, sha string, size int64, res meta.CommitResult, canRead bool) {
	if res.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if !canRead {
		// Upload-only: nothing about the folder's contents, the asset or
		// whether these bytes were already stored.
		jsonReply(w, 202, map[string]any{"result": "accepted", "folder_id": folderID, "sha256": sha, "bytes": size})
		return
	}
	status, result := 201, "created"
	if res.Result == meta.ResultAlreadyPresent {
		status, result = 200, "already_present"
	}
	jsonReply(w, status, map[string]any{"result": result, "asset_id": res.Asset.ID, "folder_id": folderID, "sha256": sha,
		"preview_ready": res.Asset.PreviewReady, "preview_status": res.Asset.PreviewStatus})
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
		if !preview {
			serveOriginal(w, r, s.blobs.PoolPath(asset.SHA256), asset.MimeType, asset.Filename)
			return
		}
		if !asset.PreviewReady {
			errorReply(w, 404, "preview_not_ready")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, s.blobs.PreviewPath(asset.SHA256))
	}
}

// serveOriginal serves a stored original as it is, whatever its type, as
// an attachment and never as a document of this origin (stored HTML or SVG
// must not run script next to the dashboard's session). Range and If-Range
// requests are answered by http.ServeFile.
func serveOriginal(w http.ResponseWriter, r *http.Request, path, mimeType, filename string) {
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Security-Policy", downloadCSP)
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition)
	http.ServeFile(w, r, path)
}
