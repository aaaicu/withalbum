package httpapi_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
)

// sessionClient drives the upload session API for one caller.
type sessionClient struct {
	t      *testing.T
	h      http.Handler
	folder string
	hdr    map[string]string // e.g. Authorization
}

func (c sessionClient) with(extra map[string]string) map[string]string {
	h := map[string]string{}
	for k, v := range c.hdr {
		h[k] = v
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func (c sessionClient) create(key string, body []byte, declared string) reply {
	c.t.Helper()
	if declared == "" {
		declared = shaHex(body)
	}
	req := fmt.Sprintf(`{"bytes":%d,"sha256":%q,"filename":"clip.mov"}`, len(body), declared)
	return do(c.t, c.h, "POST", "/api/v1/folders/"+c.folder+"/uploads", []byte(req),
		c.with(map[string]string{"Content-Type": "application/json", idemKey: key, "X-Device-ID": "phone"}))
}

func (c sessionClient) path(id string) string {
	return "/api/v1/folders/" + c.folder + "/uploads/" + id
}

func (c sessionClient) get(id string) reply {
	c.t.Helper()
	return do(c.t, c.h, "GET", c.path(id), nil, c.hdr)
}

func (c sessionClient) patch(id string, offset int64, chunk []byte, chunkSHA string) reply {
	c.t.Helper()
	return do(c.t, c.h, "PATCH", c.path(id), chunk, c.with(map[string]string{"Upload-Offset": strconv.FormatInt(offset, 10), "X-Chunk-SHA256": chunkSHA}))
}

func (c sessionClient) complete(id string) reply {
	c.t.Helper()
	return do(c.t, c.h, "POST", c.path(id)+"/complete", nil, c.hdr)
}

func (c sessionClient) cancel(id string) reply {
	c.t.Helper()
	return do(c.t, c.h, "DELETE", c.path(id), nil, c.hdr)
}

// patchBroken sends a chunk whose body breaks after n bytes.
func (c sessionClient) patchBroken(id string, offset int64, chunk []byte, n int, chunkSHA string) reply {
	c.t.Helper()
	req := httptest.NewRequest("PATCH", c.path(id), io.MultiReader(bytes.NewReader(chunk[:n]), errReader{}))
	req.ContentLength = int64(len(chunk))
	for k, v := range c.with(map[string]string{"Upload-Offset": strconv.FormatInt(offset, 10), "X-Chunk-SHA256": chunkSHA}) {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, req)
	return reply{Status: w.Code, Header: w.Header(), Body: w.Body.Bytes()}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func offsetOf(t *testing.T, r reply, status int) int64 {
	t.Helper()
	m := wantJSON(t, "upload state", r, status)
	n, ok := m["offset"].(float64)
	if !ok || r.Header.Get("Upload-Offset") != strconv.FormatInt(int64(n), 10) {
		t.Fatalf("offset %v, header %q", m["offset"], r.Header.Get("Upload-Offset"))
	}
	return int64(n)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// sendAll appends body[from:] in chunks of size, with chunk hashes.
func sendAll(t *testing.T, c sessionClient, id string, body []byte, from int64, size int) {
	t.Helper()
	for off := from; off < int64(len(body)); {
		end := min(off+int64(size), int64(len(body)))
		chunk := body[off:end]
		if got := offsetOf(t, c.patch(id, off, chunk, shaHex(chunk)), 200); got != end {
			t.Fatalf("offset after chunk = %d, want %d", got, end)
		}
		off = end
	}
}

// TestResumableUploadRoundTrip: create, chunks, a broken connection, a
// stale offset, a damaged chunk, resume, complete, retry of the
// completion; the original downloads byte for byte.
func TestResumableUploadRoundTrip(t *testing.T) {
	env := newContractEnv(t, openSQLiteStore)
	fid := createFolder(t, env.Handler, "videos")["id"].(string)
	c := sessionClient{t: t, h: env.Handler, folder: fid}
	body := randomBytes(t, 300_000)
	const key = "queue-item-1"

	m := wantJSON(t, "create", c.create(key, body, ""), 201)
	wantKeys(t, "session", m, "upload_id", "folder_id", "bytes", "sha256", "offset", "status", "expires_at", "max_chunk_bytes")
	id := m["upload_id"].(string)
	if m["status"] != "open" || m["offset"] != float64(0) || m["bytes"] != float64(len(body)) || m["folder_id"] != fid {
		t.Fatalf("session %v", m)
	}
	// A lost creation response: the same key returns the same session.
	if again := wantJSON(t, "create again", c.create(key, body, ""), 200); again["upload_id"] != id {
		t.Fatalf("second create %v", again)
	}
	wantError(t, "same key other bytes", c.create(key, body[1:], ""), 422, "idempotency_key_reused")

	sendAll(t, c, id, body[:100_000], 0, 64<<10)
	// A stale offset is refused with the current one.
	r := c.patch(id, 50_000, body[50_000:60_000], "")
	if offsetOf(t, r, 409) != 100_000 || r.object(t)["error"] != "offset_mismatch" {
		t.Fatalf("stale offset: %s", r.Body)
	}
	// A damaged chunk (its hash does not match) is dropped.
	chunk := append([]byte{}, body[100_000:150_000]...)
	sum := shaHex(chunk)
	chunk[10] ^= 0xff
	r = c.patch(id, 100_000, chunk, sum)
	wantError(t, "damaged chunk", r, 422, "chunk_hash_mismatch")
	if got := offsetOf(t, c.get(id), 200); got != 100_000 {
		t.Fatalf("offset after damaged chunk = %d", got)
	}
	// The connection breaks in a chunk with a hash: nothing of it stays.
	wantError(t, "broken hashed chunk", c.patchBroken(id, 100_000, body[100_000:150_000], 20_000, shaHex(body[100_000:150_000])), 400, "upload_failed")
	if got := offsetOf(t, c.get(id), 200); got != 100_000 {
		t.Fatalf("offset after broken hashed chunk = %d", got)
	}
	// ... in a chunk without a hash: what arrived is kept.
	wantError(t, "broken chunk", c.patchBroken(id, 100_000, body[100_000:150_000], 20_000, ""), 400, "upload_failed")
	off := offsetOf(t, c.get(id), 200)
	if off != 120_000 {
		t.Fatalf("offset after broken chunk = %d, want 120000", off)
	}
	// More than is left of the file.
	wantError(t, "past the end", c.patch(id, off, append(append([]byte{}, body[off:]...), 'x'), ""), 413, "chunk_too_large")
	wantError(t, "empty chunk", c.patch(id, off, []byte{}, ""), 400, "empty_chunk")
	wantError(t, "bad offset", do(t, c.h, "PATCH", c.path(id), body[off:off+10], map[string]string{"Upload-Offset": "-1"}), 400, "invalid_offset")
	// Not complete yet.
	r = c.complete(id)
	if m := wantJSON(t, "early complete", r, 409); m["error"] != "upload_incomplete" || m["offset"] != float64(off) {
		t.Fatalf("early complete %v", m)
	}
	sendAll(t, c, id, body, off, 64<<10)
	if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "uploads")); n != 1 {
		t.Fatalf("uploads holds %d files before completion", n)
	}

	m = wantJSON(t, "complete", c.complete(id), 201)
	wantKeys(t, "complete", m, "result", "asset_id", "folder_id", "sha256", "preview_ready", "preview_status")
	assetID := m["asset_id"].(string)
	if m["result"] != "created" || m["sha256"] != shaHex(body) || m["preview_status"] != "unsupported" {
		t.Fatalf("complete %v", m)
	}
	// A lost completion response: the same result again, nothing new.
	r = c.complete(id)
	if again := wantJSON(t, "complete again", r, 201); again["asset_id"] != assetID || r.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("complete again %v %v", again, r.Header)
	}
	// The key names one request whatever the transport: the single-request
	// upload with it replays too.
	r = uploadTo(t, env.Handler, fid, body, map[string]string{idemKey: key})
	if again := wantJSON(t, "single upload with the session key", r, 201); again["asset_id"] != assetID || r.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("single upload replay %v", again)
	}
	if st := wantJSON(t, "state", c.get(id), 200); st["status"] != "completed" || st["offset"] != float64(len(body)) {
		t.Fatalf("completed state %v", st)
	}
	wantError(t, "append after completion", c.patch(id, int64(len(body)), []byte("x"), ""), 409, "upload_completed")
	if again := wantJSON(t, "create after completion", c.create(key, body, ""), 200); again["status"] != "completed" {
		t.Fatalf("create after completion %v", again)
	}

	got := do(t, env.Handler, "GET", "/api/v1/folders/"+fid+"/assets/"+assetID+"/original", nil, nil)
	if got.Status != 200 || !bytes.Equal(got.Body, body) {
		t.Fatalf("download: %d, %d bytes, equal %v", got.Status, len(got.Body), bytes.Equal(got.Body, body))
	}
	if n := len(syncPage(t, env.Handler, fid, "")["events"].([]any)); n != 1 {
		t.Fatalf("%d events", n)
	}
	for _, dir := range []string{"uploads", "incoming"} {
		if n := countFilesUnder(t, filepath.Join(env.Root, ".system", dir)); n != 0 {
			t.Fatalf("%s holds %d files after completion", dir, n)
		}
	}
	// Cancelling a completed session only forgets it.
	if r := c.cancel(id); r.Status != 204 {
		t.Fatalf("cancel: %d %s", r.Status, r.Body)
	}
	wantError(t, "after cancel", c.get(id), 404, "upload_not_found")
	if got := do(t, env.Handler, "GET", "/api/v1/folders/"+fid+"/assets/"+assetID+"/original", nil, nil); got.Status != 200 {
		t.Fatalf("original after cancel: %d", got.Status)
	}
	wantError(t, "create for a committed key", c.create(key, body, ""), 409, "upload_already_committed")
}

// TestResumableUploadFinalHashMismatch: bytes that do not hash to the
// declared SHA-256 are discarded at completion and the session restarts.
func TestResumableUploadFinalHashMismatch(t *testing.T) {
	env := newContractEnv(t, openSQLiteStore)
	fid := createFolder(t, env.Handler, "v")["id"].(string)
	c := sessionClient{t: t, h: env.Handler, folder: fid}
	body := randomBytes(t, 50_000)
	wrong := append([]byte{}, body...)
	wrong[49_999] ^= 1
	id := wantJSON(t, "create", c.create("k", body, ""), 201)["upload_id"].(string)
	sendAll(t, c, id, wrong, 0, 20_000) // chunk hashes of the wrong bytes match
	r := c.complete(id)
	wantError(t, "complete", r, 422, "hash_mismatch")
	if r.Header.Get("Upload-Offset") != "0" || offsetOf(t, c.get(id), 200) != 0 {
		t.Fatal("session not restarted")
	}
	if n := len(syncPage(t, env.Handler, fid, "")["events"].([]any)); n != 0 {
		t.Fatalf("%d events after a refused completion", n)
	}
	sendAll(t, c, id, body, 0, 20_000)
	wantJSON(t, "complete after resend", c.complete(id), 201)
	if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "pool")); n != 1 {
		t.Fatalf("pool holds %d files", n)
	}
}

// TestResumableUploadVisibility: a session is invisible to every other
// account, to the same account on another device and under another folder;
// VIEWER cannot start one; outsiders do not see the folder.
func TestResumableUploadVisibility(t *testing.T) {
	e := newACLEnv(t)
	owner := sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["owner"]}}
	body := randomBytes(t, 10_000)
	id := wantJSON(t, "create", owner.create("k", body, ""), 201)["upload_id"].(string)
	offsetOf(t, owner.patch(id, 0, body[:4000], ""), 200)

	second := loginToken(t, e.Raw, "owner", userPassword) // the same account, another device
	ownFolder := wantJSON(t, "second folder", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"other"}`), nil), 201)["id"].(string)
	for _, tc := range []struct {
		what   string
		c      sessionClient
		status int
		code   string
	}{
		{"contributor", sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["contributor"]}}, 404, "upload_not_found"},
		{"uploader", sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["uploader"]}}, 404, "upload_not_found"},
		{"other device", sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + second}}, 404, "upload_not_found"},
		{"other folder", sessionClient{t: t, h: e.Raw, folder: ownFolder, hdr: owner.hdr}, 404, "upload_not_found"},
		{"viewer", sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["viewer"]}}, 403, "forbidden"},
		{"outsider", sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["outsider"]}}, 404, "folder_not_found"},
		{"server admin", sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["root"]}}, 404, "folder_not_found"},
	} {
		wantError(t, tc.what+" get", tc.c.get(id), tc.status, tc.code)
		wantError(t, tc.what+" patch", tc.c.patch(id, 4000, body[4000:], ""), tc.status, tc.code)
		wantError(t, tc.what+" complete", tc.c.complete(id), tc.status, tc.code)
		wantError(t, tc.what+" cancel", tc.c.cancel(id), tc.status, tc.code)
		// The same key from another caller is another request.
		if tc.status == 404 && tc.code == "upload_not_found" {
			if m := wantJSON(t, tc.what+" create", tc.c.create("k", body, ""), 201); m["upload_id"] == id {
				t.Fatalf("%s got the owner's session", tc.what)
			}
		} else {
			wantError(t, tc.what+" create", tc.c.create("k", body, ""), tc.status, tc.code)
		}
	}
	wantError(t, "malformed id", owner.get("NOT-A-HEX-ID"), 404, "upload_not_found")
	if offsetOf(t, owner.get(id), 200) != 4000 {
		t.Fatal("owner's session changed")
	}
}

// TestResumableUploaderReceiptAndRecheck: an UPLOADER gets the same 202
// receipt for new and already stored bytes, and a permission revoked while
// the session runs is honored at completion.
func TestResumableUploaderReceiptAndRecheck(t *testing.T) {
	e := newACLEnv(t)
	up := sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["uploader"]}}
	receipt := func(what string, body []byte, key string) {
		t.Helper()
		id := wantJSON(t, what+" create", up.create(key, body, ""), 201)["upload_id"].(string)
		sendAll(t, up, id, body, 0, 1000)
		m := wantJSON(t, what+" complete", up.complete(id), 202)
		wantKeys(t, what, m, "result", "folder_id", "sha256", "bytes")
		if m["result"] != "accepted" || m["sha256"] != shaHex(body) || m["bytes"] != float64(len(body)) {
			t.Fatalf("%s receipt %v", what, m)
		}
	}
	receipt("new bytes", randomBytes(t, 3000), "u1")
	receipt("bytes the folder holds", e.pic, "u2") // the owner uploaded e.pic
	receipt("bytes another folder holds", randomBytes(t, 10), "u3")

	before := e.events(t, e.family)
	body := randomBytes(t, 5000)
	id := wantJSON(t, "create", up.create("u4", body, ""), 201)["upload_id"].(string)
	sendAll(t, up, id, body, 0, 5000)
	e.setMember(t, "uploader", "VIEWER")
	wantError(t, "complete as viewer", up.complete(id), 403, "forbidden")
	e.setMember(t, "uploader", "")
	wantError(t, "complete removed", up.complete(id), 404, "folder_not_found")
	if e.events(t, e.family) != before {
		t.Fatal("a refused completion added events")
	}
	// Revoked after the request was admitted, while the original is being
	// moved into the pool: the commit transaction decides again.
	e.setMember(t, "uploader", "UPLOADER")
	e.faults.set(httpapi.FaultAfterPromote, func() error { e.setMember(t, "uploader", "VIEWER"); return nil })
	wantError(t, "revoked during completion", up.complete(id), 403, "forbidden")
	e.faults.set(httpapi.FaultAfterPromote, nil)
	if e.events(t, e.family) != before {
		t.Fatal("a completion revoked at commit added events")
	}
	// Back as UPLOADER: the session is still there and completes (the
	// original is already in the pool from the refused attempt).
	e.setMember(t, "uploader", "UPLOADER")
	wantJSON(t, "complete again", up.complete(id), 202)
	if e.events(t, e.family) != before+1 {
		t.Fatal("completion did not add the asset")
	}
}

// TestResumableUploadReservesSpace: a session reserves its whole size;
// other sessions and single-request uploads cannot use that space.
func TestResumableUploadReservesSpace(t *testing.T) {
	var mu sync.Mutex
	free := uint64(0)
	const reserve = 1000
	env := newContractEnvWith(t, openSQLiteStore, func(c *httpapi.Config) {
		c.MinFreeBytes = reserve
		c.FreeBytes = func() (uint64, error) { mu.Lock(); defer mu.Unlock(); return free, nil }
	})
	fid := createFolder(t, env.Handler, "v")["id"].(string)
	c := sessionClient{t: t, h: env.Handler, folder: fid}
	mu.Lock()
	free = reserve + 1000
	mu.Unlock()
	big := randomBytes(t, 800)
	wantError(t, "larger than the space", c.create("k0", randomBytes(t, 1001), ""), 507, "insufficient_storage")
	id := wantJSON(t, "create 800", c.create("k1", big, ""), 201)["upload_id"].(string)
	wantError(t, "second session", c.create("k2", randomBytes(t, 300), ""), 507, "insufficient_storage")
	wantError(t, "single upload into reserved space", uploadTo(t, env.Handler, fid, randomBytes(t, 300), nil), 507, "insufficient_storage")
	if r := uploadTo(t, env.Handler, fid, randomBytes(t, 150), nil); r.Status != 201 {
		t.Fatalf("small single upload: %d %s", r.Status, r.Body)
	}
	// The session's own chunks use its reservation.
	sendAll(t, c, id, big, 0, 400)
	if r := c.cancel(id); r.Status != 204 {
		t.Fatalf("cancel %d", r.Status)
	}
	if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "uploads")); n != 0 {
		t.Fatalf("cancel left %d files", n)
	}
	wantJSON(t, "create after cancel", c.create("k2", randomBytes(t, 300), ""), 201)
	wantError(t, "too large", c.create("k3", nil, "00"), 400, "invalid_bytes")
}

// TestRecoverAndPruneUploads: at startup a partial file longer than
// recorded is cut back, a session whose bytes are gone is dropped and a
// partial file without a session is deleted; maintenance prunes expired
// sessions with their files.
func TestRecoverAndPruneUploads(t *testing.T) {
	env := newContractEnv(t, openSQLiteStore)
	fid := createFolder(t, env.Handler, "v")["id"].(string)
	c := sessionClient{t: t, h: env.Handler, folder: fid}
	body := randomBytes(t, 4000)
	a := wantJSON(t, "create a", c.create("a", body, ""), 201)["upload_id"].(string)
	b := wantJSON(t, "create b", c.create("b", randomBytes(t, 10), ""), 201)["upload_id"].(string)
	offsetOf(t, c.patch(a, 0, body[:1000], ""), 200)
	offsetOf(t, c.patch(b, 0, []byte("12345"), ""), 200)
	dir := filepath.Join(env.Root, ".system", "uploads")
	// a: bytes written but never recorded (a crash between the two).
	f, err := os.OpenFile(filepath.Join(dir, a+".part"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(body[1000:1500])
	f.Close()
	// b: its bytes are lost; and a partial file nobody knows.
	if err := os.Remove(filepath.Join(dir, b+".part")); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "ffffffffffffffffffffffffffffffff.part")
	if err := os.WriteFile(stray, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	rec, err := env.API.RecoverUploads(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rec != (httpapi.UploadRecovery{Sessions: 2, Dropped: 1, Truncated: 1, OrphansRemoved: 1}) {
		t.Fatalf("recovery %+v", rec)
	}
	if fi, err := os.Stat(filepath.Join(dir, a+".part")); err != nil || fi.Size() != 1000 {
		t.Fatalf("a after recovery: %v %v", fi, err)
	}
	wantError(t, "b dropped", c.get(b), 404, "upload_not_found")
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("stray part: %v", err)
	}
	// a resumes and completes.
	sendAll(t, c, a, body, 1000, 1000)
	wantJSON(t, "complete a", c.complete(a), 201)

	// Expiry and pruning.
	d := wantJSON(t, "create d", c.create("d", randomBytes(t, 100), ""), 201)["upload_id"].(string)
	offsetOf(t, c.patch(d, 0, []byte("abc"), ""), 200)
	if _, err := env.Meta.DB().Exec("UPDATE upload_sessions SET expires_at = ? WHERE upload_id = ?",
		time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z"), d); err != nil {
		t.Fatal(err)
	}
	wantError(t, "expired", c.get(d), 404, "upload_not_found")
	n, err := env.API.PruneUploads(t.Context(), time.Now())
	if err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, d+".part")); !os.IsNotExist(err) {
		t.Fatalf("pruned part: %v", err)
	}
}

// TestConcurrentChunksAreSerialized: two appends to one session at once;
// exactly one succeeds, the other is 409 (busy, or a stale offset).
func TestConcurrentChunksAreSerialized(t *testing.T) {
	env := newContractEnv(t, openSQLiteStore)
	fid := createFolder(t, env.Handler, "v")["id"].(string)
	c := sessionClient{t: t, h: env.Handler, folder: fid}
	body := randomBytes(t, 2<<20)
	id := wantJSON(t, "create", c.create("k", body, ""), 201)["upload_id"].(string)
	var wg sync.WaitGroup
	statuses := make([]int, 4)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = c.patch(id, 0, body[:1<<20], "").Status
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		switch s {
		case 200:
			ok++
		case 409:
		default:
			t.Fatalf("statuses %v", statuses)
		}
	}
	if ok != 1 || offsetOf(t, c.get(id), 200) != 1<<20 {
		t.Fatalf("statuses %v", statuses)
	}
}

// TestPreviewWorkerHEICAndVideo uploads a HEIC (made with sips) and a
// QuickTime clip (made with ffmpeg) when the tools exist: the upload
// answers "pending", the worker makes the preview, the feed gets an
// asset_updated event with "ready", and the originals are unchanged.
// Without converters the same files are "unsupported".
func TestPreviewWorkerHEICAndVideo(t *testing.T) {
	work := t.TempDir()
	samples := map[string][]byte{}
	if sips, err := exec.LookPath("sips"); err == nil {
		src := filepath.Join(work, "in.jpg")
		img := image.NewRGBA(image.Rect(0, 0, 300, 200))
		for x := 0; x < 300; x++ {
			img.Set(x, x*2/3, color.RGBA{200, 10, 10, 255})
		}
		f, _ := os.Create(src)
		_ = jpeg.Encode(f, img, nil)
		f.Close()
		out := filepath.Join(work, "in.heic")
		if exec.Command(sips, "-s", "format", "heic", src, "--out", out).Run() == nil {
			samples["heic"], _ = os.ReadFile(out)
		}
	}
	if ffmpeg := blobstore.FindConverters(func(k string) string {
		if k == "WITHALBUM_SIPS" {
			return "off"
		}
		return ""
	}); len(ffmpeg) == 1 {
		out := filepath.Join(work, "clip.mov")
		if exec.Command(ffmpeg[0].Path, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10",
			"-c:v", "mpeg4", "-y", out).Run() == nil {
			samples["mov"], _ = os.ReadFile(out)
		}
	}
	if len(samples) == 0 {
		t.Skip("neither sips nor ffmpeg can make a sample here")
	}

	t.Run("without converters", func(t *testing.T) {
		env := newContractEnv(t, openSQLiteStore)
		fid := createFolder(t, env.Handler, "p")["id"].(string)
		for name, body := range samples {
			m := wantJSON(t, name, uploadTo(t, env.Handler, fid, body, nil), 201)
			if m["preview_status"] != "unsupported" || m["preview_ready"] != false {
				t.Fatalf("%s: %v", name, m)
			}
			wantError(t, name+" preview", do(t, env.Handler, "GET", "/api/v1/folders/"+fid+"/assets/"+m["asset_id"].(string)+"/preview", nil, nil), 404, "preview_not_ready")
		}
	})
	t.Run("with converters", func(t *testing.T) {
		env := newContractEnv(t, openSQLiteStore)
		env.Blobs.SetConverters(blobstore.FindConverters(func(string) string { return "" }))
		fid := createFolder(t, env.Handler, "p")["id"].(string)
		c := sessionClient{t: t, h: env.Handler, folder: fid}
		ids := map[string]string{}
		for name, body := range samples {
			// The MOV through an upload session, the HEIC in one request.
			var m map[string]any
			if name == "mov" {
				id := wantJSON(t, "create", c.create("mov", body, ""), 201)["upload_id"].(string)
				sendAll(t, c, id, body, 0, 16<<10)
				m = wantJSON(t, name, c.complete(id), 201)
			} else {
				m = wantJSON(t, name, uploadTo(t, env.Handler, fid, body, nil), 201)
			}
			if m["preview_status"] != "pending" || m["preview_ready"] != false {
				t.Fatalf("%s: %v", name, m)
			}
			ids[name] = m["asset_id"].(string)
		}
		start := syncPage(t, env.Handler, fid, "")["next_cursor"].(string)
		n, err := env.API.ProcessPreviews(t.Context())
		if err != nil || n != len(samples) {
			t.Fatalf("ProcessPreviews = %d %v", n, err)
		}
		page := syncPage(t, env.Handler, fid, start)
		events := page["events"].([]any)
		assets := page["assets"].(map[string]any)
		if len(events) != len(samples) {
			t.Fatalf("events after previews: %v", page)
		}
		for name, id := range ids {
			a := assets[id].(map[string]any)
			if a["preview_status"] != "ready" || a["preview_ready"] != true {
				t.Fatalf("%s after worker: %v", name, a)
			}
			p := do(t, env.Handler, "GET", "/api/v1/folders/"+fid+"/assets/"+id+"/preview", nil, nil)
			if p.Status != 200 {
				t.Fatalf("%s preview %d", name, p.Status)
			}
			if _, err := jpeg.Decode(bytes.NewReader(p.Body)); err != nil {
				t.Fatalf("%s preview is not a JPEG: %v", name, err)
			}
			o := do(t, env.Handler, "GET", "/api/v1/folders/"+fid+"/assets/"+id+"/original", nil, nil)
			if !bytes.Equal(o.Body, samples[name]) {
				t.Fatalf("%s original changed", name)
			}
		}
		for _, e := range events {
			if e.(map[string]any)["kind"] != "asset_updated" {
				t.Fatalf("event %v", e)
			}
		}
		if n, err := env.API.ProcessPreviews(t.Context()); err != nil || n != 0 {
			t.Fatalf("second ProcessPreviews = %d %v", n, err)
		}
	})
}

// TestBrokenChunkOverRealConnection: a client that disconnects in the
// middle of a chunk (its request context is cancelled) still gets the bytes
// that arrived recorded; seen in the binary smoke test, where the record
// failed with "context canceled".
func TestBrokenChunkOverRealConnection(t *testing.T) {
	env := newContractEnv(t, openSQLiteStore)
	srv := httptest.NewServer(env.Handler)
	defer srv.Close()
	fid := createFolder(t, env.Handler, "v")["id"].(string)
	c := sessionClient{t: t, h: env.Handler, folder: fid}
	body := randomBytes(t, 1<<20)
	id := wantJSON(t, "create", c.create("k", body, ""), 201)["upload_id"].(string)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PATCH %s HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer %s\r\nUpload-Offset: 0\r\nContent-Length: %d\r\n\r\n",
		c.path(id), env.Token, len(body))
	if _, err := conn.Write(body[:300_000]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the server write what it got
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		off := offsetOf(t, c.get(id), 200)
		if off == 300_000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("offset %d after the connection broke, want 300000", off)
		}
		time.Sleep(20 * time.Millisecond)
	}
	sendAll(t, c, id, body, 300_000, 400_000)
	wantJSON(t, "complete", c.complete(id), 201)
}
