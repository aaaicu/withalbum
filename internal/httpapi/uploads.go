package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// Resumable uploads. A client that sends an original larger than one
// request can carry (or over an unreliable network) creates an upload
// session with the size and SHA-256 of the whole file, appends chunks in
// order at the offset the server reports, and completes the session:
//
//	POST   /folders/{fid}/uploads                      create (Idempotency-Key)
//	GET    /folders/{fid}/uploads/{uid}                offset and state
//	PATCH  /folders/{fid}/uploads/{uid}                append (Upload-Offset)
//	POST   /folders/{fid}/uploads/{uid}/complete       verify and commit
//	DELETE /folders/{fid}/uploads/{uid}                cancel
//
// Every route needs upload permission on the folder, checked per request
// like the single-request upload, and a session is visible only to the
// account and device (session) that created it, in its folder: any other
// upload ID is 404 upload_not_found. Creation reserves the whole size: it
// is refused with 507 unless the free space minus the reserve and minus
// what other open sessions still expect can hold it, and single-request
// uploads leave the reserved space alone. Chunks are appended durably
// (fsync) before the offset is recorded; a chunk with X-Chunk-SHA256 is
// all-or-nothing, a chunk without it keeps whatever arrived before the
// connection broke. Completion hashes the stored file, compares it with the
// declared SHA-256 and commits through exactly the same path as a
// single-request upload (commitOriginal): the permission and the volume are
// re-checked inside the commit transaction, the receipt is recorded under
// the session's Idempotency-Key, and an UPLOADER gets the same 202 receipt.
// Sessions expire 24 hours after their last change and are pruned by the
// periodic maintenance.

// MaxResumableUploadSize is the largest original an upload session accepts.
const MaxResumableUploadSize int64 = 64 << 30

// MaxChunkSize is the largest chunk one PATCH may carry.
const MaxChunkSize = MaxUploadSize

const (
	headerUploadOffset = "Upload-Offset"
	headerChunkSHA256  = "X-Chunk-SHA256"
)

// uploadLocks marks upload sessions a request is working on, so that two
// requests never append to (or complete) one session at the same time. One
// server process owns the volume, so an in-process set is enough.
type uploadLocks struct {
	mu   sync.Mutex
	busy map[string]bool
}

func (l *uploadLocks) acquire(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy[id] {
		return false
	}
	if l.busy == nil {
		l.busy = map[string]bool{}
	}
	l.busy[id] = true
	return true
}

func (l *uploadLocks) release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, id)
}

func (s *Server) uploadSessionRoutes(rt *router) {
	rt.handleAlbum("POST /api/v1/folders/{folderID}/uploads", s.createUpload)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/uploads/{uploadID}", s.withUpload(false, s.getUpload))
	rt.handleAlbum("PATCH /api/v1/folders/{folderID}/uploads/{uploadID}", s.withUpload(true, s.appendUpload))
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/uploads/{uploadID}", s.withUpload(true, s.cancelUpload))
	rt.handleAlbum("POST /api/v1/folders/{folderID}/uploads/{uploadID}/complete", s.withUpload(true, s.completeUpload))
}

// uploadView is an upload session as its owner sees it.
func uploadView(us meta.UploadSession) map[string]any {
	v := map[string]any{"upload_id": us.ID, "folder_id": us.FolderID, "bytes": us.TotalBytes, "sha256": us.SHA256,
		"offset": us.Received, "status": us.Status, "expires_at": us.ExpiresAt, "max_chunk_bytes": MaxChunkSize}
	if us.BundleID != "" {
		// A component of an upload bundle (bundles.go): chunks go here,
		// completion and cancellation through the bundle.
		v["bundle_id"], v["role"] = us.BundleID, us.Role
	}
	return v
}

// inBundle refuses a completion or cancellation of a bundle's component
// session (409 upload_in_bundle); it reports whether it replied.
func inBundle(w http.ResponseWriter, us meta.UploadSession) bool {
	if us.BundleID == "" {
		return false
	}
	jsonReply(w, 409, map[string]any{"error": "upload_in_bundle", "bundle_id": us.BundleID})
	return true
}

func uploadStateReply(w http.ResponseWriter, status int, us meta.UploadSession) {
	w.Header().Set(headerUploadOffset, strconv.FormatInt(us.Received, 10))
	jsonReply(w, status, uploadView(us))
}

// offsetReply is 409 offset_mismatch with the offset to continue from.
func offsetReply(w http.ResponseWriter, offset int64) {
	w.Header().Set(headerUploadOffset, strconv.FormatInt(offset, 10))
	jsonReply(w, 409, map[string]any{"error": "offset_mismatch", "offset": offset})
}

// errNoRoom refuses a new upload session for lack of space (507).
var errNoRoom = errors.New("not enough free space for the upload")

// createUpload: POST /folders/{fid}/uploads {"bytes", "sha256", "filename"}.
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request, a albumRequest) {
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
		Bytes    int64  `json:"bytes"`
		SHA256   string `json:"sha256"`
		Filename string `json:"filename"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	switch {
	case req.Bytes <= 0:
		errorReply(w, 400, "invalid_bytes")
		return
	case req.Bytes > MaxResumableUploadSize:
		errorReply(w, 413, "file_too_large")
		return
	case !sha256RE.MatchString(req.SHA256):
		errorReply(w, 400, "invalid_sha256")
		return
	}
	if !s.storageReady(w) {
		return
	}
	canRead := false
	us, created, replaced, err := s.meta.CreateUploadSession(r.Context(), meta.NewUploadSession{
		UserID: a.P.UserID, SessionID: a.ID.SessionID, DeviceID: a.ID.DeviceID, FolderID: a.Folder.Folder.ID,
		Key: key, SHA256: req.SHA256, TotalBytes: req.Bytes, Filename: storedFilename(req.Filename, req.SHA256), DeviceLabel: deviceLabel,
		Authorize: recheck(authz.UploadAsset, &canRead),
		Reserve: func(reserved int64) error {
			free, err := s.freeBytes()
			if errors.Is(err, storage.ErrFreeSpaceUnsupported) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%w: %w", errVolumeLost, err)
			}
			if free <= uint64(s.minFree) || free-uint64(s.minFree) < uint64(max(reserved, 0))+uint64(req.Bytes) {
				log.Printf("refusing upload session of %d bytes: %d bytes free, reserve %d, open sessions expect %d", req.Bytes, free, s.minFree, reserved)
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
		// The key was committed and its session is gone (expired, pruned or
		// cancelled after completion): answer the stored result exactly as
		// a replayed single-request upload, under the caller's current
		// role (an UPLOADER gets the 202 receipt).
		uploadReply(w, a.Folder.Folder.ID, req.SHA256, req.Bytes, committed.Result, canRead)
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
		writeFailed(w, "create upload session", err, "metadata_write_failed")
		return
	}
	if replaced != "" {
		if err := s.blobs.RemovePart(replaced); err != nil {
			log.Printf("upload session %s: cannot remove the expired partial file: %v", replaced, err)
		}
	}
	status := 200
	if created {
		status = 201
	}
	uploadStateReply(w, status, us)
}

type uploadHandler func(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.UploadRef, us meta.UploadSession)

// withUpload loads the caller's upload session named in the path (404
// upload_not_found for any other) and, for writes, holds the session's lock
// while h runs (409 upload_busy while another request has it).
func (s *Server) withUpload(write bool, h uploadHandler) albumHandler {
	return func(w http.ResponseWriter, r *http.Request, a albumRequest) {
		ref := meta.UploadRef{ID: r.PathValue("uploadID"), UserID: a.P.UserID, DeviceID: a.ID.DeviceID, FolderID: a.Folder.Folder.ID}
		if !assetIDRE.MatchString(ref.ID) {
			errorReply(w, 404, "upload_not_found")
			return
		}
		if write {
			if !s.uploads.acquire(ref.ID) {
				w.Header().Set("Retry-After", "1")
				errorReply(w, 409, "upload_busy")
				return
			}
			defer s.uploads.release(ref.ID)
		}
		us, err := s.meta.GetUploadSession(r.Context(), ref)
		if errors.Is(err, meta.ErrUploadNotFound) {
			errorReply(w, 404, "upload_not_found")
			return
		}
		if err != nil {
			metaReadFailed(w, err)
			return
		}
		h(w, r, a, ref, us)
	}
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.UploadRef, us meta.UploadSession) {
	uploadStateReply(w, 200, us)
}

// cancelUpload deletes the session and its partial file. A completed
// session is only forgotten; its original stays committed.
func (s *Server) cancelUpload(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.UploadRef, us meta.UploadSession) {
	if inBundle(w, us) || !s.storageReady(w) {
		return
	}
	if err := s.meta.DeleteUploadSession(r.Context(), ref); errors.Is(err, meta.ErrUploadNotFound) {
		errorReply(w, 404, "upload_not_found")
		return
	} else if err != nil {
		writeFailed(w, "cancel upload session", err, "metadata_write_failed")
		return
	}
	if err := s.blobs.RemovePart(ref.ID); err != nil {
		log.Printf("upload session %s: cannot remove the partial file (removed at the next start): %v", ref.ID, err)
	}
	w.WriteHeader(204)
}

// restartUpload sets an open session back to offset 0 after its stored
// bytes turned out to be unusable.
func (s *Server) restartUpload(ctx context.Context, ref meta.UploadRef, from int64) error {
	if err := s.blobs.TruncatePart(ref.ID, 0); err != nil {
		return err
	}
	return s.meta.AdvanceUploadSession(ctx, ref, from, 0)
}

// parseUploadOffset parses an Upload-Offset header: a non-negative decimal
// integer, digits only (no sign, space or prefix).
func parseUploadOffset(v string) (int64, bool) {
	offset, err := strconv.ParseInt(v, 10, 64)
	if v == "" || err != nil || offset < 0 || strings.TrimLeft(v, "0123456789") != "" {
		return 0, false
	}
	return offset, true
}

// appendUpload: PATCH with Upload-Offset (must be the session's offset) and
// the next bytes as the body; optional X-Chunk-SHA256 of these bytes.
func (s *Server) appendUpload(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.UploadRef, us meta.UploadSession) {
	if us.Status != meta.UploadOpen {
		errorReply(w, 409, "upload_completed")
		return
	}
	offset, ok := parseUploadOffset(r.Header.Get(headerUploadOffset))
	if !ok {
		errorReply(w, 400, "invalid_offset")
		return
	}
	declared := strings.ToLower(strings.TrimSpace(r.Header.Get(headerChunkSHA256)))
	if declared != "" && !sha256RE.MatchString(declared) {
		errorReply(w, 400, "invalid_chunk_sha256")
		return
	}
	if offset != us.Received {
		offsetReply(w, us.Received)
		return
	}
	remaining := us.TotalBytes - us.Received
	limit := min(remaining, MaxChunkSize)
	switch {
	case remaining == 0:
		offsetReply(w, us.Received)
		return
	case r.ContentLength == 0:
		errorReply(w, 400, "empty_chunk")
		return
	case r.ContentLength > limit:
		errorReply(w, 413, "chunk_too_large")
		return
	}
	if !s.storageReady(w) {
		return
	}
	// This session's space was reserved when it was created; the chunk must
	// still fit next to what the others expect.
	want := limit
	if r.ContentLength > 0 {
		want = r.ContentLength
	}
	if budget, ok := s.uploadBudget(w, r, us.ID, want); !ok {
		return
	} else if budget < want {
		errorReply(w, 507, "insufficient_storage")
		return
	}
	res, err := s.blobs.AppendPart(us.ID, offset, r.Body, limit)
	// The bytes are on disk: record them even if the client has gone away
	// meanwhile (its request context is then cancelled).
	recordCtx := context.WithoutCancel(r.Context())
	switch {
	case err == nil:
	case errors.Is(err, blobstore.ErrPartShort):
		// The stored bytes are fewer than recorded (lost or damaged
		// partial file): start this session again at 0.
		if err := s.restartUpload(r.Context(), ref, us.Received); err != nil {
			writeFailed(w, "restart upload session", err, "storage_failed")
			return
		}
		log.Printf("upload session %s: partial file shorter than recorded; restarting at 0", us.ID)
		offsetReply(w, 0)
		return
	case errors.Is(err, blobstore.ErrTooLarge):
		errorReply(w, 413, "chunk_too_large")
		return
	case errors.Is(err, blobstore.ErrCopy):
		// The client went away mid-chunk. Without a chunk hash, keep what
		// arrived (it is on disk); the final SHA-256 still checks it all.
		if declared != "" || res.Written == 0 {
			_ = s.blobs.TruncatePart(us.ID, offset)
		} else if aerr := s.meta.AdvanceUploadSession(recordCtx, ref, offset, offset+res.Written); aerr != nil {
			log.Printf("upload session %s: cannot record %d bytes of a broken chunk: %v", us.ID, res.Written, aerr)
		}
		errorReply(w, 400, "upload_failed")
		return
	case errors.Is(err, blobstore.ErrTempCreate):
		writeFailed(w, "append upload chunk", err, "temp_failed")
		return
	case errors.Is(err, blobstore.ErrWrite):
		writeFailed(w, "append upload chunk", err, "temp_write_failed")
		return
	default:
		writeFailed(w, "append upload chunk", err, "file_sync_failed")
		return
	}
	if declared != "" && declared != res.SHA256 {
		if err := s.blobs.TruncatePart(us.ID, offset); err != nil {
			writeFailed(w, "drop damaged chunk", err, "storage_failed")
			return
		}
		w.Header().Set(headerUploadOffset, strconv.FormatInt(offset, 10))
		errorReply(w, 422, "chunk_hash_mismatch")
		return
	}
	if err := s.meta.AdvanceUploadSession(recordCtx, ref, offset, offset+res.Written); errors.Is(err, meta.ErrUploadNotFound) {
		errorReply(w, 404, "upload_not_found")
		return
	} else if errors.Is(err, meta.ErrUploadConflict) {
		errorReply(w, 409, "upload_busy")
		return
	} else if err != nil {
		// Not recorded: the bytes are cut off again before the next append.
		writeFailed(w, "record upload chunk", err, "metadata_write_failed")
		return
	}
	us.Received = offset + res.Written
	us.ExpiresAt = time.Now().UTC().Add(meta.UploadSessionTTL).Format("2006-01-02T15:04:05.000000000Z")
	uploadStateReply(w, 200, us)
}

// completeUpload verifies the received original and commits it. A completed
// session answers with the stored result again (a retry after a lost
// response), re-checked under the caller's current role.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, a albumRequest, ref meta.UploadRef, us meta.UploadSession) {
	if inBundle(w, us) {
		return
	}
	idem := meta.Idempotency{DeviceID: a.ID.DeviceID, Key: us.Key}
	if us.Status == meta.UploadCompleted {
		// Receipts outlive sessions, so the commit's receipt is there; the
		// commit below only replays it (nothing is written).
		if _, err := s.meta.LookupReceipt(r.Context(), a.P.UserID, us.FolderID, idem); errors.Is(err, meta.ErrReceiptNotFound) {
			log.Printf("upload session %s: completed without a receipt", us.ID)
			errorReply(w, 404, "upload_not_found")
			return
		} else if err != nil {
			metaReadFailed(w, err)
			return
		}
		canRead := false
		res, err := s.meta.CommitUpload(r.Context(), meta.CommitRequest{FolderID: us.FolderID, DeviceID: us.DeviceLabel, UserID: a.P.UserID,
			SessionID: a.ID.SessionID, Asset: meta.Asset{SHA256: us.SHA256, Bytes: us.TotalBytes, Filename: us.Filename},
			Idempotency: &idem, Authorize: recheck(authz.UploadAsset, &canRead)})
		if !s.uploadCommitError(w, r, a, completeRoute, err) {
			return
		}
		uploadReply(w, us.FolderID, us.SHA256, us.TotalBytes, res, canRead)
		return
	}
	if us.Received != us.TotalBytes {
		w.Header().Set(headerUploadOffset, strconv.FormatInt(us.Received, 10))
		jsonReply(w, 409, map[string]any{"error": "upload_incomplete", "offset": us.Received})
		return
	}
	if !s.storageReady(w) {
		return
	}
	staged, err := s.blobs.FinishPart(us.ID, us.SHA256, us.TotalBytes)
	switch {
	case err == nil:
	case errors.Is(err, blobstore.ErrPartMismatch):
		// Some chunk was damaged on the way: the bytes are useless, and
		// the client must send them again from the start.
		if err := s.restartUpload(r.Context(), ref, us.Received); err != nil {
			writeFailed(w, "restart upload session", err, "storage_failed")
			return
		}
		w.Header().Set(headerUploadOffset, "0")
		errorReply(w, 422, "hash_mismatch")
		return
	case errors.Is(err, blobstore.ErrPartMissing):
		if err := s.restartUpload(r.Context(), ref, us.Received); err != nil {
			writeFailed(w, "restart upload session", err, "storage_failed")
			return
		}
		log.Printf("upload session %s: partial file missing at completion; restarting at 0", us.ID)
		offsetReply(w, 0)
		return
	default:
		writeFailed(w, "verify upload session", err, "storage_failed")
		return
	}
	committed := s.commitOriginal(w, r, a, original{staged: staged, name: us.Filename, deviceLabel: us.DeviceLabel, idem: idem,
		uploadID: us.ID, route: completeRoute})
	if committed {
		// Promote moved the file into the pool, or left it because the pool
		// already had these bytes.
		if err := s.blobs.RemovePart(us.ID); err != nil {
			log.Printf("upload session %s: cannot remove the partial file (removed at the next start): %v", us.ID, err)
		}
	}
}

const completeRoute = "POST /api/v1/folders/{folderID}/uploads/{uploadID}/complete"

// UploadRecovery reports what RecoverUploads did.
type UploadRecovery struct {
	Sessions       int // session rows
	Dropped        int // rows without usable bytes, deleted
	Restarted      int // component sessions of a bundle without usable bytes, set back to 0
	Truncated      int // partial files cut back to the recorded offset
	OrphansRemoved int // partial files without a session, deleted
	Unexpected     int // other entries in .system/uploads, left alone
}

// RecoverUploads lines up .system/uploads with the upload sessions at
// startup, before any request: partial files longer than recorded (a crash
// after the write, before the record) are cut back, open sessions whose
// recorded bytes are gone (for example after a restore) are deleted (their
// client creates a new session), and partial files without a session are
// deleted. A component session of an upload bundle whose bytes are gone
// is set back to offset 0 instead (the other component keeps its bytes;
// the client resumes each from the offset it reads). Completed sessions
// need no file.
func (s *Server) RecoverUploads(ctx context.Context) (UploadRecovery, error) {
	var rec UploadRecovery
	if err := s.vol.Check(); err != nil {
		return rec, err
	}
	sessions, err := s.meta.ListUploadSessions(ctx)
	if err != nil {
		return rec, err
	}
	parts, other, err := s.blobs.ListParts()
	if err != nil {
		return rec, err
	}
	rec.Sessions, rec.Unexpected = len(sessions), len(other)
	size := map[string]int64{}
	for _, p := range parts {
		size[p.UploadID] = p.Size
	}
	known := map[string]bool{}
	for _, us := range sessions {
		known[us.ID] = true
		if us.Status != meta.UploadOpen {
			continue
		}
		n, ok := size[us.ID]
		ref := meta.UploadRef{ID: us.ID, UserID: us.UserID, DeviceID: us.DeviceID, FolderID: us.FolderID}
		switch {
		case us.Received == 0 && !ok:
		case (!ok || n < us.Received) && us.BundleID != "":
			if err := s.blobs.RemovePart(us.ID); err != nil {
				return rec, err
			}
			// An expired bundle is left to the next prune.
			if err := s.meta.AdvanceUploadSession(ctx, ref, us.Received, 0); err != nil && !errors.Is(err, meta.ErrUploadNotFound) {
				return rec, err
			}
			rec.Restarted++
		case !ok || n < us.Received:
			if err := s.meta.DeleteUploadSession(ctx, ref); err != nil {
				return rec, err
			}
			if err := s.blobs.RemovePart(us.ID); err != nil {
				return rec, err
			}
			rec.Dropped++
		case n > us.Received:
			if err := s.blobs.TruncatePart(us.ID, us.Received); err != nil {
				return rec, err
			}
			rec.Truncated++
		}
	}
	for _, p := range parts {
		if known[p.UploadID] {
			continue
		}
		if err := s.vol.Check(); err != nil {
			return rec, err
		}
		if err := s.blobs.RemovePart(p.UploadID); err != nil {
			return rec, err
		}
		rec.OrphansRemoved++
	}
	return rec, nil
}

// PruneUploads deletes upload sessions that expired before now, and their
// partial files (periodic maintenance).
func (s *Server) PruneUploads(ctx context.Context, now time.Time) (int, error) {
	if err := s.vol.Check(); err != nil {
		return 0, err
	}
	ids, err := s.meta.PruneUploadSessions(ctx, now)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.blobs.RemovePart(id); err != nil {
			log.Printf("upload session %s: cannot remove the partial file (removed at the next start): %v", id, err)
		}
	}
	return len(ids), nil
}
