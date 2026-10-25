package httpapi_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

// storeOpener opens the metadata store of a fresh volume.
type storeOpener func(t *testing.T, vol *storage.Volume) *sqlitestore.Store

// contractEnv is one server under test on a fresh, initialized volume.
// Handler sends every request as the server administrator that took over
// the bootstrap account (so it owns every folder it creates), unless the
// request already carries an Authorization header; Raw adds nothing.
type contractEnv struct {
	Handler http.Handler
	Raw     http.Handler
	Token   string
	Root    string
	Volume  *storage.Volume
	Meta    *sqlitestore.Store
	Blobs   *blobstore.Store
	API     *httpapi.Server
}

// newContractEnv builds a server on a fresh t.TempDir() volume with the
// metadata store returned by newStore.
func newContractEnv(t *testing.T, newStore storeOpener) contractEnv {
	t.Helper()
	return newContractEnvWith(t, newStore, nil)
}

// loginToken logs in through h and returns the Bearer access token.
func loginToken(t *testing.T, h http.Handler, login, pw string) string {
	t.Helper()
	r := do(t, h, "POST", "/api/v1/auth/login", []byte(`{"login_name":`+mustJSON(t, login)+`,"password":`+mustJSON(t, pw)+
		`,"device_name":"Test device","platform":"ios"}`), map[string]string{"Content-Type": "application/json"})
	m := wantJSON(t, "login "+login, r, 200)
	return m["access_token"].(string)
}

// asUser sends every request with token unless it has an Authorization header.
func asUser(h http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		h.ServeHTTP(w, r)
	})
}

// newContractEnvWith is newContractEnv with a hook to adjust the Config.
func newContractEnvWith(t *testing.T, newStore storeOpener, adjust func(*httpapi.Config)) contractEnv {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.New(vol)
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t, vol)
	t.Cleanup(func() { store.Close() })
	auth, err := httpapi.NewAuth(store.DB(), httpapi.AuthOptions{PasswordParams: fastParams})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := password.Hash(adminPassword, fastParams)
	if _, err := users.New(store.DB()).ClaimBootstrap(t.Context(), users.Claim{LoginName: "root", DisplayName: "Root", PasswordHash: hash}, users.Console); err != nil {
		t.Fatal(err)
	}
	cfg := httpapi.Config{Volume: vol, Meta: store, Blobs: blobs, Version: "test", Auth: auth}
	if adjust != nil {
		adjust(&cfg)
	}
	api := httpapi.New(cfg)
	raw := api.Handler()
	token := loginToken(t, raw, "root", adminPassword)
	return contractEnv{Handler: asUser(raw, token), Raw: raw, Token: token, Root: vol.Root(), Volume: vol, Meta: store, Blobs: blobs, API: api}
}

// runContract pins the observable HTTP behavior (status codes, JSON keys,
// error codes, headers) for one metadata store implementation. Every
// meta.MetadataStore must pass it unchanged.
func runContract(t *testing.T, newStore storeOpener) {
	newEnv := func(t *testing.T) contractEnv { return newContractEnv(t, newStore) }
	t.Run("Health", func(t *testing.T) { contractHealth(t, newEnv(t)) })
	t.Run("Dashboard", func(t *testing.T) { contractDashboard(t, newEnv(t)) })
	t.Run("Folders", func(t *testing.T) { contractFolders(t, newEnv(t)) })
	t.Run("Upload", func(t *testing.T) { contractUpload(t, newEnv(t)) })
	t.Run("UploadFilenames", func(t *testing.T) { contractUploadFilenames(t, newEnv(t)) })
	t.Run("Sync", func(t *testing.T) { contractSync(t, newEnv(t)) })
	t.Run("SyncPaging", func(t *testing.T) { contractSyncPaging(t, newEnv(t)) })
	t.Run("Download", func(t *testing.T) { contractDownload(t, newEnv(t)) })
	t.Run("FailClosed", func(t *testing.T) { contractFailClosed(t, newEnv(t)) })
	t.Run("ConcurrentUploads", func(t *testing.T) { contractConcurrentUploads(t, newEnv(t)) })
	t.Run("InsufficientStorage", func(t *testing.T) { contractInsufficientStorage(t, newStore) })
	t.Run("WriteErrors", func(t *testing.T) { contractWriteErrors(t, newStore) })
	t.Run("RestoreMissingOriginal", func(t *testing.T) { contractRestoreMissingOriginal(t, newEnv(t)) })
}

const idemKey = "Idempotency-Key"

type reply struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("status %d: body is not a JSON object: %v: %q", r.Status, err, r.Body)
	}
	return m
}

// do sends one request. An upload (POST .../assets) without an
// Idempotency-Key in header gets a fresh one, as a new client request
// would; an empty value sends none.
func do(t *testing.T, h http.Handler, method, path string, body []byte, header map[string]string) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if _, set := header[idemKey]; !set && method == "POST" && strings.HasSuffix(req.URL.Path, "/assets") {
		req.Header.Set(idemKey, rand.Text())
	}
	for k, v := range header {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return reply{Status: w.Code, Header: w.Header(), Body: w.Body.Bytes()}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// wantKeys asserts that m has exactly the given keys.
func wantKeys(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	sort.Strings(keys)
	if got := keysOf(m); strings.Join(got, ",") != strings.Join(keys, ",") {
		t.Fatalf("%s keys = %v, want %v", what, got, keys)
	}
}

// wantJSON asserts the status and the JSON content type.
func wantJSON(t *testing.T, what string, r reply, status int) map[string]any {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s: status %d, want %d: %s", what, r.Status, status, r.Body)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("%s: Content-Type %q", what, ct)
	}
	return r.object(t)
}

// wantError asserts an error reply of exactly {"error": code}.
func wantError(t *testing.T, what string, r reply, status int, code string) {
	t.Helper()
	m := wantJSON(t, what, r, status)
	wantKeys(t, what, m, "error")
	if m["error"] != code {
		t.Fatalf("%s: error %v, want %q", what, m["error"], code)
	}
}

func createFolder(t *testing.T, h http.Handler, name string) map[string]any {
	t.Helper()
	m := wantJSON(t, "create folder", do(t, h, "POST", "/api/v1/folders", []byte(`{"name":`+mustJSON(t, name)+`}`), nil), 201)
	return m
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func uploadTo(t *testing.T, h http.Handler, folderID string, body []byte, header map[string]string) reply {
	t.Helper()
	return do(t, h, "POST", "/api/v1/folders/"+folderID+"/assets", body, header)
}

func syncPage(t *testing.T, h http.Handler, folderID, cursor string) map[string]any {
	t.Helper()
	path := "/api/v1/folders/" + folderID + "/sync"
	if cursor != "" {
		path += "?cursor=" + url.QueryEscape(cursor)
	}
	m := wantJSON(t, "sync", do(t, h, "GET", path, nil, nil), 200)
	wantKeys(t, "sync", m, "folder_id", "events", "assets", "next_cursor", "has_more")
	if m["folder_id"] != folderID {
		t.Fatalf("sync folder_id %v", m["folder_id"])
	}
	return m
}

func countFilesUnder(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func contractHealth(t *testing.T, env contractEnv) {
	m := wantJSON(t, "health", do(t, env.Handler, "GET", "/api/v1/health", nil, nil), 200)
	wantKeys(t, "health", m, "status", "version", "storage", "integrity")
	if m["status"] != "ok" || m["storage"] != "ok" || m["integrity"] != "ok" {
		t.Fatalf("health %v", m)
	}
}

func contractDashboard(t *testing.T, env contractEnv) {
	r := do(t, env.Handler, "GET", "/", nil, nil)
	if r.Status != 200 || r.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("dashboard %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	for _, h := range []string{"Content-Security-Policy", "Cache-Control", "X-Content-Type-Options"} {
		if r.Header.Get(h) == "" {
			t.Fatalf("dashboard missing %s", h)
		}
	}
	if r := do(t, env.Handler, "GET", "/does-not-exist", nil, nil); r.Status != 404 {
		t.Fatalf("unknown path %d", r.Status)
	}
}

func contractFolders(t *testing.T, env contractEnv) {
	h := env.Handler
	m := wantJSON(t, "list empty", do(t, h, "GET", "/api/v1/folders", nil, nil), 200)
	wantKeys(t, "list", m, "folders")
	if list, ok := m["folders"].([]any); !ok || len(list) != 0 {
		t.Fatalf("empty folders = %#v, want []", m["folders"])
	}

	f := createFolder(t, h, "  family  ")
	wantKeys(t, "folder", f, folderKeys...)
	if f["name"] != "family" || f["role"] != "OWNER" || f["can_read"] != true || f["can_upload"] != true {
		t.Fatalf("name not trimmed: %v", f["name"])
	}
	if id, _ := f["id"].(string); len(id) != 32 {
		t.Fatalf("folder id %q", id)
	}
	createFolder(t, h, strings.Repeat("가", 80))

	for _, tc := range []struct{ body, code string }{
		{`not json`, "invalid_json"},
		{`{"name":"   "}`, "invalid_folder_name"},
		{`{}`, "invalid_folder_name"},
		{`{"name":` + mustJSON(t, strings.Repeat("가", 81)) + `}`, "invalid_folder_name"},
	} {
		wantError(t, "create "+tc.body, do(t, h, "POST", "/api/v1/folders", []byte(tc.body), nil), 400, tc.code)
	}

	m = wantJSON(t, "list", do(t, h, "GET", "/api/v1/folders", nil, nil), 200)
	list := m["folders"].([]any)
	if len(list) != 2 {
		t.Fatalf("folders = %d, want 2", len(list))
	}
	for _, item := range list {
		wantKeys(t, "listed folder", item.(map[string]any), folderKeys...)
	}
	g := wantJSON(t, "get folder", do(t, h, "GET", "/api/v1/folders/"+f["id"].(string), nil, nil), 200)
	if fmt.Sprint(g) != fmt.Sprint(f) {
		t.Fatalf("get folder %v, want %v", g, f)
	}
	wantError(t, "unknown folder", do(t, h, "GET", "/api/v1/folders/"+strings.Repeat("0", 32), nil, nil), 404, "folder_not_found")
}

var folderKeys = []string{"id", "name", "created_at", "role", "can_read", "can_upload", "member_count", "hide_location"}

func contractUpload(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "a")["id"].(string)
	other := createFolder(t, h, "b")["id"].(string)
	pic := jpegBytes(t, 4, 4)
	sha := shaHex(pic)
	hdr := map[string]string{"X-Filename": "pic.jpg", "X-Device-ID": "iphone-A"}

	wantError(t, "unknown folder", uploadTo(t, h, strings.Repeat("0", 32), pic, hdr), 404, "folder_not_found")

	ids := map[string]string{} // folder -> asset id
	check := func(what string, r reply, status int, result string, preview bool, folderID string) {
		t.Helper()
		m := wantJSON(t, what, r, status)
		wantKeys(t, what, m, "result", "asset_id", "folder_id", "sha256", "preview_ready", "preview_status")
		if m["result"] != result || m["sha256"] != sha || m["folder_id"] != folderID || m["preview_ready"] != preview {
			t.Fatalf("%s: %v", what, m)
		}
		id, _ := m["asset_id"].(string)
		if len(id) != 32 || id == sha[:32] || (ids[folderID] != "" && ids[folderID] != id) {
			t.Fatalf("%s: asset_id %q (folder had %q)", what, id, ids[folderID])
		}
		ids[folderID] = id
	}
	check("first", uploadTo(t, h, fid, pic, hdr), 201, "created", true, fid)
	check("repeat", uploadTo(t, h, fid, pic, hdr), 200, "already_present", true, fid)
	// Bytes stored for another folder: the answer is the same as for new
	// bytes, and the folder gets an asset of its own.
	check("other folder", uploadTo(t, h, other, pic, hdr), 201, "created", true, other)
	check("repeat other", uploadTo(t, h, other, pic, nil), 200, "already_present", true, other)
	if ids[fid] == ids[other] {
		t.Fatal("two folders share an asset id")
	}
	// A matching declared hash (any case, surrounding space) is accepted.
	third := createFolder(t, h, "c")["id"].(string)
	check("declared hash", uploadTo(t, h, third, pic, map[string]string{"X-Content-SHA256": " " + strings.ToUpper(sha) + " "}), 201, "created", true, third)

	wantError(t, "empty", uploadTo(t, h, fid, []byte{}, nil), 400, "empty_file")
	wantError(t, "hash mismatch", uploadTo(t, h, fid, []byte("hello"), map[string]string{"X-Content-SHA256": strings.Repeat("0", 64)}), 422, "hash_mismatch")
	wantError(t, "long device id", uploadTo(t, h, fid, []byte("hello"), map[string]string{"X-Device-ID": strings.Repeat("d", 101)}), 400, "invalid_device_id")

	m := wantJSON(t, "text", uploadTo(t, h, fid, []byte("plain text"), map[string]string{"X-Device-ID": strings.Repeat("d", 100)}), 201)
	if m["result"] != "created" || m["preview_ready"] != false {
		t.Fatalf("text upload %v", m)
	}
	// Refused uploads leave nothing behind in incoming/.
	if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "incoming")); n != 0 {
		t.Fatalf("incoming has %d files", n)
	}
}

func contractUploadFilenames(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "names")["id"].(string)
	cases := []struct{ header, want string }{
		{"%ED%95%9C.jpg", "한.jpg"},
		{`dir\sub/evil.jpg`, "evil.jpg"},
		{"", ""}, // falls back to the sha
		{strings.Repeat("n", 241), ""},
	}
	for i, tc := range cases {
		body := []byte(fmt.Sprintf("file %d", i))
		hdr := map[string]string{}
		if tc.header != "" {
			hdr["X-Filename"] = tc.header
		}
		id := wantJSON(t, "upload "+tc.header, uploadTo(t, h, fid, body, hdr), 201)["asset_id"].(string)
		want := tc.want
		if want == "" {
			want = shaHex(body)
		}
		assets := syncPage(t, h, fid, "")["assets"].(map[string]any)
		a := assets[id].(map[string]any)
		if a["filename"] != want {
			t.Fatalf("filename %q -> %v, want %q", tc.header, a["filename"], want)
		}
		if a["mime_type"] != "text/plain; charset=utf-8" {
			t.Fatalf("mime %v", a["mime_type"])
		}
	}
}

// cursorAt replaces the sequence of a sync cursor.
func cursorAt(t *testing.T, cursor string, seq int) string {
	t.Helper()
	epoch, _, ok := strings.Cut(cursor, ".")
	if !ok || len(epoch) != 16 {
		t.Fatalf("cursor %q", cursor)
	}
	return fmt.Sprintf("%s.%d", epoch, seq)
}

func contractSync(t *testing.T, env contractEnv) {
	h := env.Handler
	wantError(t, "unknown folder", do(t, h, "GET", "/api/v1/folders/nope/sync", nil, nil), 404, "folder_not_found")
	fid := createFolder(t, h, "s")["id"].(string)
	other := createFolder(t, h, "other")["id"].(string)
	for _, c := range []string{"abc", "-1", "1.5", "0", "1", "0123456789abcdef.", "0123456789abcdef.-1", "0123456789ABCDEF.1", "0123456789abcdef.1.2"} {
		wantError(t, "cursor "+c, do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?cursor="+url.QueryEscape(c), nil, nil), 400, "invalid_cursor")
	}
	for _, l := range []string{"0", "-1", "501", "x", "1.5", "+5"} {
		wantError(t, "limit "+l, do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?limit="+url.QueryEscape(l), nil, nil), 400, "invalid_limit")
	}

	m := syncPage(t, h, fid, "")
	if ev, ok := m["events"].([]any); !ok || len(ev) != 0 {
		t.Fatalf("empty events = %#v, want []", m["events"])
	}
	if as, ok := m["assets"].(map[string]any); !ok || len(as) != 0 {
		t.Fatalf("empty assets = %#v, want {}", m["assets"])
	}
	start := m["next_cursor"].(string)
	if cursorAt(t, start, 0) != start || m["has_more"] != false {
		t.Fatalf("empty sync %v", m)
	}
	otherStart := syncPage(t, h, other, "")["next_cursor"].(string)
	if otherStart == start {
		t.Fatal("two folders share a feed epoch")
	}
	// A cursor of another folder, or ahead of the feed, is not a position
	// of this feed.
	wantError(t, "other folder's cursor", do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?cursor="+otherStart, nil, nil), 409, "cursor_invalid")
	wantError(t, "cursor ahead", do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?cursor="+cursorAt(t, start, 1), nil, nil), 409, "cursor_invalid")

	pic := jpegBytes(t, 3, 2)
	picID := wantJSON(t, "upload", uploadTo(t, h, fid, pic, map[string]string{"X-Filename": "p.jpg", "X-Device-ID": " dev-1 "}), 201)["asset_id"].(string)
	wantJSON(t, "upload", uploadTo(t, h, fid, pic, nil), 200) // already present: no new event
	wantJSON(t, "upload", uploadTo(t, h, fid, []byte("second"), nil), 201)

	m = syncPage(t, h, fid, start)
	events := m["events"].([]any)
	if len(events) != 2 || m["next_cursor"] != cursorAt(t, start, 2) || m["has_more"] != false {
		t.Fatalf("sync %v", m)
	}
	e := events[0].(map[string]any)
	wantKeys(t, "event", e, "seq", "kind", "asset_id", "folder_id", "uploader_device_id", "occurred_at")
	if e["seq"] != float64(1) || e["kind"] != "asset_added" || e["asset_id"] != picID || e["folder_id"] != fid || e["uploader_device_id"] != "dev-1" {
		t.Fatalf("event %v", e)
	}
	if events[1].(map[string]any)["seq"] != float64(2) {
		t.Fatalf("second event %v", events[1])
	}
	assets := m["assets"].(map[string]any)
	if len(assets) != 2 {
		t.Fatalf("assets %v", assets)
	}
	a := assets[picID].(map[string]any)
	wantKeys(t, "asset", a, "id", "sha256", "filename", "mime_type", "bytes", "preview_ready", "preview_status", "uploaded_at", "captured_at", "duration_ms", "location", "camera", "width", "height", "uploaded_by_name", "media_kind", "like_count", "liked_by_me", "comment_count")
	if a["id"] != picID || a["sha256"] != shaHex(pic) || a["filename"] != "p.jpg" || a["mime_type"] != "image/jpeg" || a["bytes"] != float64(len(pic)) || a["preview_ready"] != true {
		t.Fatalf("asset %v", a)
	}

	m = syncPage(t, h, fid, cursorAt(t, start, 1))
	if len(m["events"].([]any)) != 1 || m["next_cursor"] != cursorAt(t, start, 2) {
		t.Fatalf("cursor 1: %v", m)
	}
	// The latest cursor gives an empty page and is echoed back.
	m = syncPage(t, h, fid, cursorAt(t, start, 2))
	if len(m["events"].([]any)) != 0 || m["next_cursor"] != cursorAt(t, start, 2) || m["has_more"] != false {
		t.Fatalf("latest cursor: %v", m)
	}
	wantError(t, "cursor past the end", do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?cursor="+cursorAt(t, start, 50), nil, nil), 409, "cursor_invalid")

	// Removal: a tombstone event; the asset leaves "assets", also for a
	// client reading from the beginning.
	if r := do(t, h, "DELETE", "/api/v1/folders/"+fid+"/assets/"+picID, nil, nil); r.Status != 204 {
		t.Fatalf("remove: %d %s", r.Status, r.Body)
	}
	wantError(t, "remove again", do(t, h, "DELETE", "/api/v1/folders/"+fid+"/assets/"+picID, nil, nil), 404, "asset_not_found")
	m = syncPage(t, h, fid, cursorAt(t, start, 2))
	events = m["events"].([]any)
	if len(events) != 1 || len(m["assets"].(map[string]any)) != 0 || m["next_cursor"] != cursorAt(t, start, 3) {
		t.Fatalf("after remove: %v", m)
	}
	e = events[0].(map[string]any)
	wantKeys(t, "tombstone", e, "seq", "kind", "asset_id", "folder_id", "uploader_device_id", "occurred_at")
	if e["kind"] != "asset_removed" || e["asset_id"] != picID || e["seq"] != float64(3) || e["uploader_device_id"] != "" {
		t.Fatalf("tombstone %v", e)
	}
	m = syncPage(t, h, fid, "")
	if len(m["events"].([]any)) != 3 || len(m["assets"].(map[string]any)) != 1 || m["assets"].(map[string]any)[picID] != nil {
		t.Fatalf("full feed after remove: %v", m)
	}
	wantError(t, "removed original", do(t, h, "GET", "/api/v1/folders/"+fid+"/assets/"+picID+"/original", nil, nil), 404, "asset_not_found")
	// The same bytes again are a new asset with a new ID.
	again := wantJSON(t, "re-upload", uploadTo(t, h, fid, pic, nil), 201)["asset_id"].(string)
	if again == picID {
		t.Fatal("re-upload revived the removed asset id")
	}
}

func contractSyncPaging(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "many")["id"].(string)
	for i := 0; i < 101; i++ {
		wantJSON(t, "upload", uploadTo(t, h, fid, []byte(fmt.Sprintf("item-%03d", i)), nil), 201)
	}
	m := syncPage(t, h, fid, "")
	start := m["next_cursor"].(string)
	if len(m["events"].([]any)) != 100 || m["has_more"] != true || len(m["assets"].(map[string]any)) != 100 {
		t.Fatalf("page 1: events=%d next=%v more=%v", len(m["events"].([]any)), m["next_cursor"], m["has_more"])
	}
	m = syncPage(t, h, fid, start)
	if len(m["events"].([]any)) != 1 || m["next_cursor"] != cursorAt(t, start, 101) || m["has_more"] != false {
		t.Fatalf("page 2: %v", m)
	}
	m = wantJSON(t, "limit", do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?limit=7&cursor="+cursorAt(t, start, 90), nil, nil), 200)
	if len(m["events"].([]any)) != 7 || m["next_cursor"] != cursorAt(t, start, 97) || m["has_more"] != true {
		t.Fatalf("limit 7: %v", m)
	}
	m = wantJSON(t, "max limit", do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?limit=500", nil, nil), 200)
	if len(m["events"].([]any)) != 101 || m["has_more"] != false {
		t.Fatalf("limit 500: %d events", len(m["events"].([]any)))
	}
}

func contractDownload(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "d")["id"].(string)
	other := createFolder(t, h, "e")["id"].(string)
	pic := jpegBytes(t, 2000, 1000)
	text := []byte("not an image")
	picID := wantJSON(t, "upload", uploadTo(t, h, fid, pic, nil), 201)["asset_id"].(string)
	textID := wantJSON(t, "upload", uploadTo(t, h, fid, text, nil), 201)["asset_id"].(string)
	otherID := wantJSON(t, "upload", uploadTo(t, h, other, []byte("elsewhere"), nil), 201)["asset_id"].(string)
	path := func(folder, asset, kind string) string {
		return "/api/v1/folders/" + folder + "/assets/" + asset + "/" + kind
	}

	wantError(t, "short id", do(t, h, "GET", path(fid, "abc", "original"), nil, nil), 404, "asset_not_found")
	wantError(t, "unknown", do(t, h, "GET", path(fid, strings.Repeat("a", 32), "original"), nil, nil), 404, "asset_not_found")
	wantError(t, "sha as id", do(t, h, "GET", path(fid, shaHex(pic), "original"), nil, nil), 404, "asset_not_found")
	wantError(t, "asset of another folder", do(t, h, "GET", path(fid, otherID, "original"), nil, nil), 404, "asset_not_found")
	wantError(t, "asset under another folder", do(t, h, "GET", path(other, picID, "preview"), nil, nil), 404, "asset_not_found")
	wantError(t, "unknown folder", do(t, h, "GET", path(strings.Repeat("0", 32), picID, "original"), nil, nil), 404, "folder_not_found")
	wantError(t, "no preview", do(t, h, "GET", path(fid, textID, "preview"), nil, nil), 404, "preview_not_ready")
	// The PoC routes by SHA-256 are gone.
	for _, kind := range []string{"original", "preview"} {
		wantError(t, "legacy route", do(t, h, "GET", "/api/v1/assets/"+shaHex(pic)+"/"+kind, nil, nil), 404, "not_found")
	}

	r := do(t, h, "GET", path(fid, picID, "original"), nil, nil)
	if r.Status != 200 || !bytes.Equal(r.Body, pic) || r.Header.Get("Content-Type") != "image/jpeg" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("original %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("original Cache-Control %q", r.Header.Get("Cache-Control"))
	}
	r = do(t, h, "GET", path(fid, textID, "original"), nil, nil)
	if r.Status != 200 || !bytes.Equal(r.Body, text) || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("text original %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	r = do(t, h, "GET", path(fid, picID, "preview"), nil, nil)
	if r.Status != 200 || r.Header.Get("Content-Type") != "image/jpeg" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("preview %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(r.Body))
	if err != nil || cfg.Width != 1600 || cfg.Height != 800 {
		t.Fatalf("preview size %+v %v", cfg, err)
	}
	// Originals in the pool are read-only.
	st, err := os.Stat(filepath.Join(env.Root, ".system", "pool", shaHex(pic)[:2], shaHex(pic)))
	if err != nil || st.Mode().Perm() != 0400 {
		t.Fatalf("pool file %v %v", st, err)
	}
}

func contractFailClosed(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "family")["id"].(string)
	if err := os.Remove(filepath.Join(env.Root, storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	wantError(t, "upload", uploadTo(t, h, fid, []byte("sample bytes"), map[string]string{"X-Filename": "sample.txt"}), 503, "storage_unavailable")
	wantError(t, "create folder", do(t, h, "POST", "/api/v1/folders", []byte(`{"name":"other"}`), nil), 503, "storage_unavailable")
	for _, dir := range []string{"pool", "incoming", "previews"} {
		if n := countFilesUnder(t, filepath.Join(env.Root, ".system", dir)); n != 0 {
			t.Fatalf("%s has %d files after refused writes", dir, n)
		}
	}
	m := wantJSON(t, "health", do(t, h, "GET", "/api/v1/health", nil, nil), 200)
	if m["storage"] != "unavailable" {
		t.Fatalf("health %v", m)
	}
	// Reads keep working and show no partial state.
	if n := len(wantJSON(t, "list", do(t, h, "GET", "/api/v1/folders", nil, nil), 200)["folders"].([]any)); n != 1 {
		t.Fatalf("folders = %d, want 1", n)
	}
	if n := len(syncPage(t, h, fid, "")["events"].([]any)); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
}

// contractConcurrentUploads sends the same bytes to two folders at once: each
// folder ends up with exactly one link and one event, and the asset is
// created exactly once.
func contractConcurrentUploads(t *testing.T, env contractEnv) {
	h := env.Handler
	folders := []string{createFolder(t, h, "x")["id"].(string), createFolder(t, h, "y")["id"].(string)}
	pic := jpegBytes(t, 8, 8)
	const perFolder = 8
	logs := captureLog(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[string]int{}
	var failures []string
	for i := 0; i < perFolder*len(folders); i++ {
		wg.Add(1)
		go func(fid string) {
			defer wg.Done()
			r := uploadTo(t, h, fid, pic, nil)
			var m map[string]any
			_ = json.Unmarshal(r.Body, &m)
			mu.Lock()
			results[fmt.Sprintf("%d %v", r.Status, m["result"])]++
			if r.Status >= 300 {
				failures = append(failures, fmt.Sprintf("%d %s", r.Status, bytes.TrimSpace(r.Body)))
			}
			mu.Unlock()
		}(folders[i%len(folders)])
	}
	wg.Wait()
	want := map[string]int{"201 created": 2, "200 already_present": perFolder*len(folders) - 2}
	if fmt.Sprint(results) != fmt.Sprint(want) {
		t.Fatalf("results %v, want %v\nfailed replies: %q\nserver log:\n%s", results, want, failures, logs.String())
	}
	for _, fid := range folders {
		m := syncPage(t, h, fid, "")
		events := m["events"].([]any)
		if len(events) != 1 || events[0].(map[string]any)["seq"] != float64(1) || m["has_more"] != false {
			t.Fatalf("folder %s: %v", fid, m)
		}
	}
	if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "pool")); n != 1 {
		t.Fatalf("pool has %d files, want 1", n)
	}
	if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "incoming")); n != 0 {
		t.Fatalf("incoming has %d files", n)
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the standard logger (the server's log) to a buffer, and
// also to the original output, until the test ends. Tests of this package
// do not run in parallel, so swapping the global logger is safe.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, buf))
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// wantNoBlobs asserts that incoming/ and pool/ hold no files.
func wantNoBlobs(t *testing.T, what, root string) {
	t.Helper()
	for _, dir := range []string{"incoming", "pool"} {
		if n := countFilesUnder(t, filepath.Join(root, ".system", dir)); n != 0 {
			t.Fatalf("%s: %s has %d files", what, dir, n)
		}
	}
}

// contractInsufficientStorage: uploads that would leave less than
// MinFreeBytes are refused with 507 before anything reaches the pool.
func contractInsufficientStorage(t *testing.T, newStore storeOpener) {
	var mu sync.Mutex
	free := uint64(0)
	setFree := func(n uint64) { mu.Lock(); free = n; mu.Unlock() }
	const reserve = 1000
	env := newContractEnvWith(t, newStore, func(c *httpapi.Config) {
		c.MinFreeBytes = reserve
		c.FreeBytes = func() (uint64, error) { mu.Lock(); defer mu.Unlock(); return free, nil }
	})
	h := env.Handler
	fid := createFolder(t, h, "full")["id"].(string)
	body := []byte(strings.Repeat("p", 100))

	setFree(reserve) // nothing above the reserve
	wantError(t, "at reserve", uploadTo(t, h, fid, body, nil), 507, "insufficient_storage")
	setFree(reserve + 50) // known length does not fit: refused before reading
	wantError(t, "content-length over budget", uploadTo(t, h, fid, body, nil), 507, "insufficient_storage")
	// Unknown length (chunked): staged up to the budget, then refused.
	req := httptest.NewRequest("POST", "/api/v1/folders/"+fid+"/assets", io.MultiReader(bytes.NewReader(body)))
	req.Header.Set(idemKey, rand.Text())
	req.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	wantError(t, "chunked over budget", reply{Status: w.Code, Header: w.Header(), Body: w.Body.Bytes()}, 507, "insufficient_storage")
	wantNoBlobs(t, "after 507", env.Root)
	if n := len(syncPage(t, h, fid, "")["events"].([]any)); n != 0 {
		t.Fatalf("events = %d after 507", n)
	}
	// Larger than MaxUploadSize stays 413 even when space is short.
	big := httptest.NewRequest("POST", "/api/v1/folders/"+fid+"/assets", bytes.NewReader(body))
	big.Header.Set(idemKey, rand.Text())
	big.ContentLength = httpapi.MaxUploadSize + 1
	w = httptest.NewRecorder()
	h.ServeHTTP(w, big)
	if w.Code != 413 {
		t.Fatalf("oversized declared length: %d %s", w.Code, w.Body)
	}

	setFree(reserve + 100) // exactly fits
	wantJSON(t, "fits", uploadTo(t, h, fid, body, nil), 201)
}

// faultMap injects the error returned by the function registered for a point.
type faultMap struct {
	mu sync.Mutex
	at map[string]func() error
}

func (f *faultMap) set(point string, fn func() error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at == nil {
		f.at = map[string]func() error{}
	}
	if fn == nil {
		delete(f.at, point)
		return
	}
	f.at[point] = fn
}

func (f *faultMap) Fault(point string) error {
	f.mu.Lock()
	fn := f.at[point]
	f.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

func errFn(err error) func() error { return func() error { return err } }

// contractWriteErrors: a full disk, an I/O error, a busy database or a lost
// volume at each step of the upload gets its own status, never a success,
// never an event; the retry then succeeds exactly once.
func contractWriteErrors(t *testing.T, newStore storeOpener) {
	faults := &faultMap{}
	env := newContractEnvWith(t, newStore, func(c *httpapi.Config) { c.Faults = faults })
	h := env.Handler
	fid := createFolder(t, h, "errors")["id"].(string)
	enospc := &fs.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}
	eio := &fs.PathError{Op: "rename", Path: "x", Err: syscall.EIO}
	for i, tc := range []struct {
		point  string
		fn     func() error
		status int
		code   string
	}{
		{httpapi.FaultAfterStage, errFn(enospc), 507, "insufficient_storage"},
		{httpapi.FaultAfterStage, errFn(eio), 503, "storage_io_error"},
		{httpapi.FaultAfterPromote, errFn(enospc), 507, "insufficient_storage"},
		{httpapi.FaultAfterPromote, errFn(eio), 503, "storage_io_error"},
		{httpapi.FaultAfterPreview, errFn(errors.New("boom")), 500, "storage_failed"},
		{httpapi.FaultBeforeCommit, errFn(fmt.Errorf("%w: sqlite", storage.ErrNoSpace)), 507, "insufficient_storage"},
		{httpapi.FaultBeforeCommit, errFn(fmt.Errorf("%w: sqlite", storage.ErrIO)), 503, "storage_io_error"},
		{httpapi.FaultBeforeCommit, errFn(fmt.Errorf("%w: locked", meta.ErrBusy)), 503, "busy"},
		{httpapi.FaultBeforeCommit, errFn(errors.New("boom")), 500, "metadata_write_failed"},
		// The volume disappears between the last handler check and COMMIT.
		{httpapi.FaultBeforeCommit, func() error { return os.Remove(filepath.Join(env.Root, storage.MarkerName)) }, 503, "storage_unavailable"},
	} {
		body := []byte(fmt.Sprintf("payload %d", i))
		what := fmt.Sprintf("%s %s", tc.point, tc.code)
		faults.set(tc.point, tc.fn)
		r := uploadTo(t, h, fid, body, nil)
		faults.set(tc.point, nil)
		wantError(t, what, r, tc.status, tc.code)
		if tc.code == "busy" && r.Header.Get("Retry-After") != "1" {
			t.Fatalf("%s: Retry-After %q", what, r.Header.Get("Retry-After"))
		}
		if n := countFilesUnder(t, filepath.Join(env.Root, ".system", "incoming")); n != 0 {
			t.Fatalf("%s: incoming has %d files", what, n)
		}
		if n := len(syncPage(t, h, fid, "")["events"].([]any)); n != i {
			t.Fatalf("%s: %d events, want %d", what, n, i)
		}
		if tc.code == "storage_unavailable" {
			if _, err := storage.Init(env.Root); err != nil {
				t.Fatal(err)
			}
			// The re-created marker has a new volume id; this server
			// keeps refusing writes until it is restarted.
			wantError(t, what+" retry", uploadTo(t, h, fid, body, nil), 503, "storage_unavailable")
			continue
		}
		// The retry succeeds once, reusing a pool file left behind.
		wantJSON(t, what+" retry", uploadTo(t, h, fid, body, nil), 201)
		if n := len(syncPage(t, h, fid, "")["events"].([]any)); n != i+1 {
			t.Fatalf("%s retry: %d events, want %d", what, n, i+1)
		}
	}
}

// contractRestoreMissingOriginal: when the pool file of a known original is
// gone, uploading the same bytes again puts it back.
func contractRestoreMissingOriginal(t *testing.T, env contractEnv) {
	h := env.Handler
	a := createFolder(t, h, "a")["id"].(string)
	b := createFolder(t, h, "b")["id"].(string)
	body := []byte("an original that got lost")
	wantJSON(t, "upload", uploadTo(t, h, a, body, nil), 201)
	pool := filepath.Join(env.Root, ".system", "pool", shaHex(body)[:2], shaHex(body))
	if err := os.Remove(pool); err != nil {
		t.Fatal(err)
	}
	m := wantJSON(t, "re-upload", uploadTo(t, h, b, body, nil), 201)
	if m["result"] != "created" {
		t.Fatalf("re-upload %v", m)
	}
	if got, err := os.ReadFile(pool); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pool file not restored: %v", err)
	}
}
