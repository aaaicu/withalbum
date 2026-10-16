package httpapi_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// contractEnv is one server under test on a fresh, initialized volume.
type contractEnv struct {
	Handler http.Handler
	Root    string
	Volume  *storage.Volume
	Meta    meta.MetadataStore
}

// newContractEnv builds a server on a fresh t.TempDir() volume with the
// metadata store returned by newStore.
func newContractEnv(t *testing.T, newStore func(t *testing.T, vol *storage.Volume) meta.MetadataStore) contractEnv {
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
	api := httpapi.New(httpapi.Config{Volume: vol, Meta: store, Blobs: blobs, Version: "test"})
	return contractEnv{Handler: api.Handler(), Root: vol.Root(), Volume: vol, Meta: store}
}

// runContract pins the observable HTTP behavior (status codes, JSON keys,
// error codes, headers) for one metadata store implementation. Every
// meta.MetadataStore must pass it unchanged.
func runContract(t *testing.T, newStore func(t *testing.T, vol *storage.Volume) meta.MetadataStore) {
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
}

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

func do(t *testing.T, h http.Handler, method, path string, body []byte, header map[string]string) reply {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range header {
		req.Header.Set(k, v)
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
		path += "?cursor=" + cursor
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
	wantKeys(t, "health", m, "status", "version", "storage")
	if m["status"] != "ok" || m["storage"] != "ok" {
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
	wantKeys(t, "folder", f, "id", "name", "created_at")
	if f["name"] != "family" {
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
		wantKeys(t, "listed folder", item.(map[string]any), "id", "name", "created_at")
	}
}

func contractUpload(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "a")["id"].(string)
	other := createFolder(t, h, "b")["id"].(string)
	pic := jpegBytes(t, 4, 4)
	sha := shaHex(pic)
	hdr := map[string]string{"X-Filename": "pic.jpg", "X-Device-ID": "iphone-A"}

	wantError(t, "unknown folder", uploadTo(t, h, strings.Repeat("0", 32), pic, hdr), 404, "folder_not_found")

	check := func(what string, r reply, status int, result string, preview bool, folderID string) {
		t.Helper()
		m := wantJSON(t, what, r, status)
		wantKeys(t, what, m, "result", "asset_id", "folder_id", "sha256", "preview_ready")
		if m["result"] != result || m["asset_id"] != sha || m["sha256"] != sha || m["folder_id"] != folderID || m["preview_ready"] != preview {
			t.Fatalf("%s: %v", what, m)
		}
	}
	check("first", uploadTo(t, h, fid, pic, hdr), 201, "created", true, fid)
	check("repeat", uploadTo(t, h, fid, pic, hdr), 200, "already_present", true, fid)
	check("other folder", uploadTo(t, h, other, pic, hdr), 201, "linked", true, other)
	check("repeat other", uploadTo(t, h, other, pic, nil), 200, "already_present", true, other)
	// A matching declared hash (any case, surrounding space) is accepted.
	third := createFolder(t, h, "c")["id"].(string)
	check("declared hash", uploadTo(t, h, third, pic, map[string]string{"X-Content-SHA256": " " + strings.ToUpper(sha) + " "}), 201, "linked", true, third)

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
		wantJSON(t, "upload "+tc.header, uploadTo(t, h, fid, body, hdr), 201)
		want := tc.want
		if want == "" {
			want = shaHex(body)
		}
		assets := syncPage(t, h, fid, "")["assets"].(map[string]any)
		a := assets[shaHex(body)].(map[string]any)
		if a["filename"] != want {
			t.Fatalf("filename %q -> %v, want %q", tc.header, a["filename"], want)
		}
		if a["mime_type"] != "text/plain; charset=utf-8" {
			t.Fatalf("mime %v", a["mime_type"])
		}
	}
}

func contractSync(t *testing.T, env contractEnv) {
	h := env.Handler
	wantError(t, "unknown folder", do(t, h, "GET", "/api/v1/folders/nope/sync", nil, nil), 404, "folder_not_found")
	fid := createFolder(t, h, "s")["id"].(string)
	for _, c := range []string{"abc", "-1", "1.5"} {
		wantError(t, "cursor "+c, do(t, h, "GET", "/api/v1/folders/"+fid+"/sync?cursor="+c, nil, nil), 400, "invalid_cursor")
	}

	m := syncPage(t, h, fid, "")
	if ev, ok := m["events"].([]any); !ok || len(ev) != 0 {
		t.Fatalf("empty events = %#v, want []", m["events"])
	}
	if as, ok := m["assets"].(map[string]any); !ok || len(as) != 0 {
		t.Fatalf("empty assets = %#v, want {}", m["assets"])
	}
	if m["next_cursor"] != float64(0) || m["has_more"] != false {
		t.Fatalf("empty sync %v", m)
	}

	pic := jpegBytes(t, 3, 2)
	wantJSON(t, "upload", uploadTo(t, h, fid, pic, map[string]string{"X-Filename": "p.jpg", "X-Device-ID": " dev-1 "}), 201)
	wantJSON(t, "upload", uploadTo(t, h, fid, pic, nil), 200) // already present: no new event
	wantJSON(t, "upload", uploadTo(t, h, fid, []byte("second"), nil), 201)

	m = syncPage(t, h, fid, "0")
	events := m["events"].([]any)
	if len(events) != 2 || m["next_cursor"] != float64(2) || m["has_more"] != false {
		t.Fatalf("sync %v", m)
	}
	e := events[0].(map[string]any)
	wantKeys(t, "event", e, "seq", "kind", "asset_id", "folder_id", "uploader_device_id", "occurred_at")
	if e["seq"] != float64(1) || e["kind"] != "asset_added" || e["asset_id"] != shaHex(pic) || e["folder_id"] != fid || e["uploader_device_id"] != "dev-1" {
		t.Fatalf("event %v", e)
	}
	if events[1].(map[string]any)["seq"] != float64(2) {
		t.Fatalf("second event %v", events[1])
	}
	assets := m["assets"].(map[string]any)
	if len(assets) != 2 {
		t.Fatalf("assets %v", assets)
	}
	a := assets[shaHex(pic)].(map[string]any)
	wantKeys(t, "asset", a, "id", "filename", "mime_type", "bytes", "preview_ready", "uploaded_at")
	if a["id"] != shaHex(pic) || a["filename"] != "p.jpg" || a["mime_type"] != "image/jpeg" || a["bytes"] != float64(len(pic)) || a["preview_ready"] != true {
		t.Fatalf("asset %v", a)
	}

	m = syncPage(t, h, fid, "1")
	if len(m["events"].([]any)) != 1 || m["next_cursor"] != float64(2) {
		t.Fatalf("cursor 1: %v", m)
	}
	// A cursor past the end echoes back unchanged.
	m = syncPage(t, h, fid, "50")
	if len(m["events"].([]any)) != 0 || m["next_cursor"] != float64(50) || m["has_more"] != false {
		t.Fatalf("cursor 50: %v", m)
	}
}

func contractSyncPaging(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "many")["id"].(string)
	for i := 0; i < 101; i++ {
		wantJSON(t, "upload", uploadTo(t, h, fid, []byte(fmt.Sprintf("item-%03d", i)), nil), 201)
	}
	m := syncPage(t, h, fid, "")
	if len(m["events"].([]any)) != 100 || m["next_cursor"] != float64(100) || m["has_more"] != true || len(m["assets"].(map[string]any)) != 100 {
		t.Fatalf("page 1: events=%d next=%v more=%v", len(m["events"].([]any)), m["next_cursor"], m["has_more"])
	}
	m = syncPage(t, h, fid, "100")
	if len(m["events"].([]any)) != 1 || m["next_cursor"] != float64(101) || m["has_more"] != false {
		t.Fatalf("page 2: %v", m)
	}
}

func contractDownload(t *testing.T, env contractEnv) {
	h := env.Handler
	fid := createFolder(t, h, "d")["id"].(string)
	pic := jpegBytes(t, 2000, 1000)
	text := []byte("not an image")
	wantJSON(t, "upload", uploadTo(t, h, fid, pic, nil), 201)
	wantJSON(t, "upload", uploadTo(t, h, fid, text, nil), 201)

	wantError(t, "short id", do(t, h, "GET", "/api/v1/assets/abc/original", nil, nil), 400, "invalid_asset_id")
	wantError(t, "non-hex id", do(t, h, "GET", "/api/v1/assets/"+strings.Repeat("z", 64)+"/preview", nil, nil), 400, "invalid_asset_id")
	wantError(t, "unknown", do(t, h, "GET", "/api/v1/assets/"+strings.Repeat("a", 64)+"/original", nil, nil), 404, "asset_not_found")
	wantError(t, "no preview", do(t, h, "GET", "/api/v1/assets/"+shaHex(text)+"/preview", nil, nil), 404, "preview_not_ready")

	r := do(t, h, "GET", "/api/v1/assets/"+shaHex(pic)+"/original", nil, nil)
	if r.Status != 200 || !bytes.Equal(r.Body, pic) || r.Header.Get("Content-Type") != "image/jpeg" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("original %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	r = do(t, h, "GET", "/api/v1/assets/"+shaHex(text)+"/original", nil, nil)
	if r.Status != 200 || !bytes.Equal(r.Body, text) || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("text original %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	r = do(t, h, "GET", "/api/v1/assets/"+shaHex(pic)+"/preview", nil, nil)
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
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[string]int{}
	for i := 0; i < perFolder*len(folders); i++ {
		wg.Add(1)
		go func(fid string) {
			defer wg.Done()
			r := uploadTo(t, h, fid, pic, nil)
			var m map[string]any
			_ = json.Unmarshal(r.Body, &m)
			mu.Lock()
			results[fmt.Sprintf("%d %v", r.Status, m["result"])]++
			mu.Unlock()
		}(folders[i%len(folders)])
	}
	wg.Wait()
	want := map[string]int{"201 created": 1, "201 linked": 1, "200 already_present": perFolder*len(folders) - 2}
	if fmt.Sprint(results) != fmt.Sprint(want) {
		t.Fatalf("results %v, want %v", results, want)
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
