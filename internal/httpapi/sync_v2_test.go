package httpapi_test

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Server side of the client sync scenarios (docs/CLIENT_SYNC_STATES.md,
// "기본 실패 시나리오") and of sync API v2: idempotent uploads, the feed with
// tombstones and cursors, the snapshot, the folder-scoped duplicate check,
// asset removal and the cursor acknowledgement.

func (e *aclEnv) upload(t *testing.T, login, folder string, body []byte, key string) reply {
	t.Helper()
	return e.as(t, login, "POST", "/api/v1/folders/"+folder+"/assets", body, map[string]string{idemKey: key, "X-Filename": "p.jpg"})
}

func (e *aclEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.Meta.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *aclEnv) poolFiles(t *testing.T) int {
	return countFilesUnder(t, filepath.Join(e.Root, ".system", "pool"))
}

// feed is a client's local view of one folder: asset id -> sha256.
type feed map[string]string

// pull applies the feed from cursor to the end, page by page, like a client
// that persists each page before moving on.
func (e *aclEnv) pull(t *testing.T, login, folder, cursor string, local feed, limit int) string {
	t.Helper()
	for {
		path := fmt.Sprintf("/api/v1/folders/%s/sync?limit=%d", folder, limit)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		m := wantJSON(t, "pull", e.as(t, login, "GET", path, nil, nil), 200)
		assets := m["assets"].(map[string]any)
		for _, ev := range m["events"].([]any) {
			ev := ev.(map[string]any)
			id := ev["asset_id"].(string)
			switch ev["kind"] {
			case "asset_added":
				if a, ok := assets[id].(map[string]any); ok {
					local[id] = a["sha256"].(string)
				}
			case "asset_removed":
				delete(local, id)
			default:
				t.Fatalf("event kind %v", ev["kind"])
			}
		}
		cursor = m["next_cursor"].(string)
		if m["has_more"] != true {
			return cursor
		}
	}
}

// snapshot loads the folder's current assets page by page and returns them
// with the cursor of the first page.
func (e *aclEnv) snapshot(t *testing.T, login, folder string, limit int) (feed, string) {
	t.Helper()
	local := feed{}
	cursor, after := "", ""
	for {
		path := fmt.Sprintf("/api/v1/folders/%s/assets?limit=%d", folder, limit)
		if after != "" {
			path += "&after=" + after
		}
		m := wantJSON(t, "snapshot", e.as(t, login, "GET", path, nil, nil), 200)
		if after == "" {
			wantKeys(t, "first snapshot page", m, "folder_id", "assets", "has_more", "next_after", "cursor")
			cursor = m["cursor"].(string)
		} else {
			wantKeys(t, "snapshot page", m, "folder_id", "assets", "has_more", "next_after")
		}
		for _, a := range m["assets"].([]any) {
			a := a.(map[string]any)
			wantKeys(t, "snapshot asset", a, "id", "sha256", "filename", "mime_type", "bytes", "preview_ready", "preview_status", "uploaded_at", "captured_at", "duration_ms", "uploaded_by_name", "media_kind", "like_count", "liked_by_me", "comment_count")
			local[a["id"].(string)] = a["sha256"].(string)
		}
		if m["has_more"] != true {
			if m["next_after"] != "" {
				t.Fatalf("last page next_after %v", m["next_after"])
			}
			return local, cursor
		}
		after = m["next_after"].(string)
	}
}

// Scenario 3: the server stored the file but the response was lost. The
// retry with the same Idempotency-Key gets the same answer and nothing is
// stored twice.
func TestScenario3ResponseLostRetry(t *testing.T) {
	e := newACLEnv(t)
	body := jpegBytes(t, 9, 9)
	events, receipts, pool := e.events(t, e.family), e.count(t, "SELECT count(*) FROM upload_receipts"), e.poolFiles(t)
	first := e.upload(t, "contributor", e.family, body, "queue-item-1")
	m1 := wantJSON(t, "first", first, 201)
	if first.Header.Get("Idempotency-Replayed") != "" {
		t.Fatal("first attempt marked as replay")
	}
	for i := 0; i < 3; i++ {
		r := e.upload(t, "contributor", e.family, body, "queue-item-1")
		m := wantJSON(t, "retry", r, 201)
		if fmt.Sprint(m) != fmt.Sprint(m1) || r.Header.Get("Idempotency-Replayed") != "true" {
			t.Fatalf("retry %d: %v (replayed %q), want %v", i, m, r.Header.Get("Idempotency-Replayed"), m1)
		}
	}
	if e.events(t, e.family) != events+1 || e.poolFiles(t) != pool+1 || e.count(t, "SELECT count(*) FROM upload_receipts") != receipts+1 {
		t.Fatal("retries stored something")
	}
	if e.count(t, "SELECT count(*) FROM folder_assets WHERE folder_id = ? AND asset_id = ?", e.family, m1["asset_id"]) != 1 {
		t.Fatal("link missing")
	}
	// Bytes the folder already holds under a new key: 200, and its retry too.
	dup := wantJSON(t, "already present", e.upload(t, "contributor", e.family, e.pic, "queue-item-2"), 200)
	again := e.upload(t, "contributor", e.family, e.pic, "queue-item-2")
	if fmt.Sprint(wantJSON(t, "already present retry", again, 200)) != fmt.Sprint(dup) || again.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("already-present retry %s", again.Body)
	}
	if e.events(t, e.family) != events+1 {
		t.Fatal("already present created an event")
	}
	// An upload-only member gets the same receipt on a retry.
	up1 := wantJSON(t, "uploader", e.upload(t, "uploader", e.family, []byte("uploader bytes"), "u-1"), 202)
	up2 := wantJSON(t, "uploader retry", e.upload(t, "uploader", e.family, []byte("uploader bytes"), "u-1"), 202)
	if fmt.Sprint(up1) != fmt.Sprint(up2) {
		t.Fatalf("uploader retry %v != %v", up2, up1)
	}
	// Concurrent retries of one request: one commit, one answer.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]int{}
	body2 := jpegBytes(t, 11, 7)
	for range 8 {
		wg.Go(func() {
			r := e.upload(t, "owner", e.family, body2, "parallel")
			mu.Lock()
			defer mu.Unlock()
			if r.Status != 201 {
				t.Errorf("parallel retry %d %s", r.Status, r.Body)
				return
			}
			ids[r.object(t)["asset_id"].(string)]++
		})
	}
	wg.Wait()
	if len(ids) != 1 || e.events(t, e.family) != events+3 {
		t.Fatalf("parallel retries: asset ids %v, events %d", ids, e.events(t, e.family)-events)
	}
}

// TestIdempotencyKeyRules: the key is required and bounded; reusing it for
// other bytes is refused before anything reaches the pool.
func TestIdempotencyKeyRules(t *testing.T) {
	e := newACLEnv(t)
	body := []byte("keyed upload")
	wantError(t, "missing", e.upload(t, "owner", e.family, body, ""), 400, "idempotency_key_required")
	for _, k := range []string{strings.Repeat("k", 129), "has space", "slash/x", "ü", "a\"b"} {
		r := do(t, e.Raw, "POST", "/api/v1/folders/"+e.family+"/assets", body, map[string]string{"Authorization": "Bearer " + e.tokens["owner"], idemKey: k})
		wantError(t, "key "+k, r, 400, "invalid_idempotency_key")
	}
	wantJSON(t, "uuid key", e.upload(t, "owner", e.family, body, "3f2b8c1e-7d4a-4f0e-9a51-0c6b2d8e4f17"), 201)
	pool := e.poolFiles(t)
	wantError(t, "reused for other bytes", e.upload(t, "owner", e.family, []byte("other bytes"), "3f2b8c1e-7d4a-4f0e-9a51-0c6b2d8e4f17"), 422, "idempotency_key_reused")
	if e.poolFiles(t) != pool || countFilesUnder(t, filepath.Join(e.Root, ".system", "incoming")) != 0 {
		t.Fatal("refused key reuse left a file")
	}
	// The VIEWER is refused before the key matters.
	wantError(t, "viewer", e.upload(t, "viewer", e.family, body, "v"), 403, "forbidden")
}

// TestIdempotencyKeyIsolation: the same key string of another account,
// another device of the same account, or another folder is another request:
// nothing is replayed across, and nothing about the other request leaks.
func TestIdempotencyKeyIsolation(t *testing.T) {
	e := newACLEnv(t)
	const key = "same-key"
	ownerRes := wantJSON(t, "owner", e.upload(t, "owner", e.family, []byte("owner bytes"), key), 201)
	// Another account, same folder, same key, other bytes: its own upload.
	c := e.upload(t, "contributor", e.family, []byte("contributor bytes"), key)
	cm := wantJSON(t, "contributor", c, 201)
	if cm["asset_id"] == ownerRes["asset_id"] || c.Header.Get("Idempotency-Replayed") != "" {
		t.Fatalf("contributor got the owner's request: %v", cm)
	}
	// Same bytes as the owner under the owner's key: a normal duplicate,
	// not a replay of the owner's request.
	c = e.upload(t, "contributor", e.family, []byte("owner bytes"), "contributor-2")
	if wantJSON(t, "contributor dup", c, 200)["asset_id"] != ownerRes["asset_id"] || c.Header.Get("Idempotency-Replayed") != "" {
		t.Fatalf("contributor duplicate %s", c.Body)
	}
	// An UPLOADER using the key of a reader learns nothing (generic 202).
	wantJSON(t, "uploader same key", e.upload(t, "uploader", e.family, []byte("owner bytes"), key), 202)
	// Same account, second device (new login): a new request.
	second := loginToken(t, e.Raw, "owner", userPassword)
	r := do(t, e.Raw, "POST", "/api/v1/folders/"+e.family+"/assets", []byte("second device bytes"), map[string]string{"Authorization": "Bearer " + second, idemKey: key})
	if wantJSON(t, "second device", r, 201); r.Header.Get("Idempotency-Replayed") != "" {
		t.Fatal("second device replayed the first device's request")
	}
	// Same account and device, another folder: a new request.
	other := wantJSON(t, "folder", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"other"}`), nil), 201)["id"].(string)
	if r := e.upload(t, "owner", other, []byte("other folder bytes"), key); r.Status != 201 || r.Header.Get("Idempotency-Replayed") != "" {
		t.Fatalf("other folder: %d %s", r.Status, r.Body)
	}
	if n := e.count(t, "SELECT count(*) FROM upload_receipts WHERE idempotency_key = ?", key); n != 5 {
		t.Fatalf("receipts with the key: %d, want 5", n)
	}
}

// failingReader returns n bytes, then an error (a dropped connection).
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection reset")
	}
	k := min(len(p), f.n)
	for i := range p[:k] {
		p[i] = 'x'
	}
	f.n -= k
	return k, nil
}

// Scenario 2: the network drops during the upload. Nothing is committed;
// the retry of the same request stores it exactly once.
func TestScenario2NetworkDropDuringUpload(t *testing.T) {
	e := newACLEnv(t)
	events := e.events(t, e.family)
	req := httptest.NewRequest("POST", "/api/v1/folders/"+e.family+"/assets", io.NopCloser(&failingReader{n: 4096}))
	req.ContentLength = 1 << 20
	req.Header.Set("Authorization", "Bearer "+e.tokens["contributor"])
	req.Header.Set(idemKey, "dropped-1")
	w := httptest.NewRecorder()
	e.Raw.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "upload_failed") {
		t.Fatalf("dropped upload: %d %s", w.Code, w.Body)
	}
	if e.events(t, e.family) != events || e.count(t, "SELECT count(*) FROM upload_receipts WHERE idempotency_key = 'dropped-1'") != 0 ||
		countFilesUnder(t, filepath.Join(e.Root, ".system", "incoming")) != 0 {
		t.Fatal("dropped upload left state behind")
	}
	body := []byte(strings.Repeat("x", 1<<20))
	wantJSON(t, "retry", e.upload(t, "contributor", e.family, body, "dropped-1"), 201)
	wantJSON(t, "retry again", e.upload(t, "contributor", e.family, body, "dropped-1"), 201)
	if e.events(t, e.family) != events+1 {
		t.Fatalf("events %d, want %d", e.events(t, e.family), events+1)
	}
}

// Scenario 6: two people upload the same photo to the same and to another
// folder: one original on disk, one asset per folder.
func TestScenario6SamePhotoTwoPeople(t *testing.T) {
	e := newACLEnv(t)
	photo := jpegBytes(t, 21, 13)
	pool := e.poolFiles(t)
	a := wantJSON(t, "owner", e.upload(t, "owner", e.family, photo, "o"), 201)
	b := wantJSON(t, "contributor", e.upload(t, "contributor", e.family, photo, "c"), 200)
	if a["asset_id"] != b["asset_id"] || b["result"] != "already_present" {
		t.Fatalf("same folder: %v / %v", a, b)
	}
	wantJSON(t, "uploader", e.upload(t, "uploader", e.family, photo, "u"), 202)
	c := wantJSON(t, "other folder", e.upload(t, "root", e.adminF, photo, "r"), 201)
	if c["asset_id"] == a["asset_id"] {
		t.Fatal("two folders share an asset")
	}
	if e.poolFiles(t) != pool+1 {
		t.Fatalf("pool files %d, want %d", e.poolFiles(t), pool+1)
	}
	if n := e.count(t, "SELECT count(*) FROM folder_assets fa JOIN assets x ON x.asset_id = fa.asset_id WHERE fa.folder_id = ? AND x.original_sha256 = ?", e.family, shaHex(photo)); n != 1 {
		t.Fatalf("links in family %d", n)
	}
	// Removing it from one folder leaves the other folder's asset and the
	// original untouched.
	if r := e.as(t, "root", "DELETE", "/api/v1/folders/"+e.adminF+"/assets/"+c["asset_id"].(string), nil, nil); r.Status != 204 {
		t.Fatalf("remove %d %s", r.Status, r.Body)
	}
	if r := e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/assets/"+a["asset_id"].(string)+"/original", nil, nil); r.Status != 200 || shaHex(r.Body) != shaHex(photo) {
		t.Fatalf("family copy after removal elsewhere: %d", r.Status)
	}
	if e.poolFiles(t) != pool+1 {
		t.Fatal("removal deleted the original")
	}
}

// Scenario 7: a device offline for a long time catches up from its cursor
// through many pages (additions and removals) and ends with exactly the
// folder's content. A cursor older than the retained events is 409
// cursor_expired; the client reloads the snapshot and continues from its
// cursor.
func TestScenario7LongOfflineDevice(t *testing.T) {
	e := newACLEnv(t)
	local := feed{}
	cursor := e.pull(t, "viewer", e.family, "", local, 100)
	var ids []string
	for i := 0; i < 230; i++ {
		ids = append(ids, wantJSON(t, "upload", e.upload(t, "contributor", e.family, []byte(fmt.Sprintf("offline %03d", i)), fmt.Sprintf("k%d", i)), 201)["asset_id"].(string))
	}
	for i := 0; i < 230; i += 3 {
		if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family+"/assets/"+ids[i], nil, nil); r.Status != 204 {
			t.Fatalf("remove %d: %d %s", i, r.Status, r.Body)
		}
	}
	cursor = e.pull(t, "viewer", e.family, cursor, local, 100)
	snap, snapCursor := e.snapshot(t, "viewer", e.family, 50)
	if fmt.Sprint(sortedKeys(local)) != fmt.Sprint(sortedKeys(snap)) || len(local) != 1+230-77 {
		t.Fatalf("caught-up state has %d assets, snapshot %d", len(local), len(snap))
	}
	if cursor != snapCursor {
		t.Fatalf("cursor after catching up %s, snapshot cursor %s", cursor, snapCursor)
	}

	// Retention moves past the device's old cursor.
	old := cursorAt(t, cursor, 5)
	if _, err := e.Meta.DB().Exec("DELETE FROM folder_changes WHERE folder_id = ? AND seq <= 100", e.family); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Meta.DB().Exec("UPDATE folder_sequences SET pruned_seq = 100 WHERE folder_id = ?", e.family); err != nil {
		t.Fatal(err)
	}
	wantError(t, "expired cursor", e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/sync?cursor="+old, nil, nil), 409, "cursor_expired")
	wantError(t, "from the beginning", e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/sync", nil, nil), 409, "cursor_expired")
	wantJSON(t, "cursor at the retention edge", e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/sync?cursor="+cursorAt(t, cursor, 100), nil, nil), 200)
	// Recovery: snapshot, then changes made meanwhile through the feed.
	fresh, from := e.snapshot(t, "viewer", e.family, 40)
	wantJSON(t, "new", e.upload(t, "contributor", e.family, []byte("after the snapshot"), "late"), 201)
	if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family+"/assets/"+ids[1], nil, nil); r.Status != 204 {
		t.Fatal(r.Status)
	}
	e.pull(t, "viewer", e.family, from, fresh, 100)
	final, _ := e.snapshot(t, "viewer", e.family, 500)
	if fmt.Sprint(sortedKeys(fresh)) != fmt.Sprint(sortedKeys(final)) {
		t.Fatal("snapshot + feed does not match the folder")
	}
}

func sortedKeys(f feed) []string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Scenario 8: a member's permission is revoked; every sync route refuses
// the next request (404 after removal, 403 after a downgrade to UPLOADER).
func TestScenario8PermissionRevoked(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	cursor := e.pull(t, "viewer", e.family, "", feed{}, 100)
	check := `{"sha256":["` + shaHex(e.pic) + `"]}`
	ack := `{"cursor":"` + cursor + `"}`
	routes := []struct{ method, path, body string }{
		{"GET", f + "/sync", ""}, {"GET", f + "/sync?cursor=" + cursor, ""}, {"GET", f + "/assets", ""},
		{"POST", f + "/assets/check", check}, {"PUT", f + "/sync/cursor", ack},
		{"GET", f + "/assets/" + e.photo + "/original", ""},
	}
	for _, rt := range routes {
		if r := e.asJSON(t, "viewer", rt.method, rt.path, rt.body); r.Status != 200 && r.Status != 204 {
			t.Fatalf("viewer %s %s before: %d %s", rt.method, rt.path, r.Status, r.Body)
		}
	}
	e.setMember(t, "viewer", "UPLOADER")
	for _, rt := range routes {
		wantError(t, "downgraded "+rt.path, e.asJSON(t, "viewer", rt.method, rt.path, rt.body), 403, "forbidden")
	}
	e.setMember(t, "viewer", "")
	for _, rt := range routes {
		wantError(t, "removed "+rt.path, e.asJSON(t, "viewer", rt.method, rt.path, rt.body), 404, "folder_not_found")
	}
}

// TestSyncV2RouteMatrix: who may use the snapshot, the duplicate check, the
// cursor acknowledgement and removal (ACCESS_CONTROL §7-4, §7-5).
func TestSyncV2RouteMatrix(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	cursor := e.pull(t, "owner", e.family, "", feed{}, 100)
	readers := map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 403, "outsider": 404, "root": 404}
	acks := map[string]int{"owner": 204, "contributor": 204, "viewer": 204, "uploader": 403, "outsider": 404, "root": 404}
	for _, login := range aclRoles {
		if r := e.as(t, login, "GET", f+"/assets", nil, nil); r.Status != readers[login] {
			t.Fatalf("snapshot as %s: %d", login, r.Status)
		}
		if r := e.asJSON(t, login, "POST", f+"/assets/check", `{"sha256":["`+shaHex(e.pic)+`"]}`); r.Status != readers[login] {
			t.Fatalf("check as %s: %d", login, r.Status)
		}
		if r := e.asJSON(t, login, "PUT", f+"/sync/cursor", `{"cursor":"`+cursor+`"}`); r.Status != acks[login] {
			t.Fatalf("ack as %s: %d %s", login, r.Status, r.Body)
		}
	}
	if n := e.count(t, "SELECT count(*) FROM device_sync_cursors WHERE folder_id = ?", e.family); n != 3 {
		t.Fatalf("acknowledged cursors %d, want 3", n)
	}
	// §7-5: only the owner removes photos, a contributor not even its own.
	own := wantJSON(t, "contributor upload", e.upload(t, "contributor", e.family, []byte("contributor photo"), "cp"), 201)["asset_id"].(string)
	for _, target := range []string{e.photo, own} {
		for login, want := range map[string]int{"contributor": 403, "viewer": 403, "uploader": 403, "outsider": 404, "root": 404} {
			r := e.as(t, login, "DELETE", f+"/assets/"+target, nil, nil)
			if r.Status != want {
				t.Fatalf("delete %s as %s: %d %s", target, login, r.Status, r.Body)
			}
		}
	}
	if e.count(t, "SELECT count(*) FROM folder_assets WHERE folder_id = ? AND removed_at IS NOT NULL", e.family) != 0 {
		t.Fatal("refused removal removed something")
	}
	wantError(t, "asset of another folder", e.as(t, "owner", "DELETE", f+"/assets/"+e.other, nil, nil), 404, "asset_not_found")
	wantError(t, "unknown asset", e.as(t, "owner", "DELETE", f+"/assets/"+strings.Repeat("0", 32), nil, nil), 404, "asset_not_found")
	if r := e.as(t, "owner", "DELETE", f+"/assets/"+own, nil, nil); r.Status != 204 {
		t.Fatalf("owner removes: %d %s", r.Status, r.Body)
	}
	if e.audits(t, "asset.remove") != 1 {
		t.Fatal("removal not audited")
	}
	// An upload-only member's attempts are summarized in the audit log.
	if n := e.count(t, "SELECT count(*) FROM audit_log WHERE action = 'access.denied' AND actor_user_id = ?", e.ids["uploader"]); n < 4 {
		t.Fatalf("uploader denials audited: %d", n)
	}
}

// TestSnapshotAndCheck: snapshot paging and the folder-scoped duplicate
// check.
func TestSnapshotAndCheck(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	for i := 0; i < 5; i++ {
		wantJSON(t, "upload", e.upload(t, "owner", e.family, []byte(fmt.Sprintf("snap %d", i)), fmt.Sprintf("s%d", i)), 201)
	}
	snap, _ := e.snapshot(t, "viewer", e.family, 2)
	if len(snap) != 6 {
		t.Fatalf("snapshot %d assets", len(snap))
	}
	for _, q := range []string{"after=xyz", "after=" + strings.Repeat("A", 32), "limit=0", "limit=501"} {
		r := e.as(t, "viewer", "GET", f+"/assets?"+q, nil, nil)
		if r.Status != 400 {
			t.Fatalf("snapshot %s: %d", q, r.Status)
		}
	}
	// Check: this folder's hashes only.
	adminOnly := []byte("only in the admin folder")
	wantJSON(t, "admin", e.upload(t, "root", e.adminF, adminOnly, "a"), 201)
	m := wantJSON(t, "check", e.asJSON(t, "viewer", "POST", f+"/assets/check",
		`{"sha256":["`+strings.ToUpper(shaHex(e.pic))+`","`+shaHex(adminOnly)+`","`+shaHex([]byte("nowhere"))+`","`+shaHex(e.pic)+`"]}`), 200)
	present := m["present"].([]any)
	if len(present) != 1 || present[0].(map[string]any)["sha256"] != shaHex(e.pic) || present[0].(map[string]any)["asset_id"] != e.photo {
		t.Fatalf("check %v", m)
	}
	for _, body := range []string{`{"sha256":[]}`, `{"sha256":["abc"]}`, `{}`, `{"sha256":[` + strings.Repeat(`"`+shaHex(e.pic)+`",`, 100) + `"` + shaHex(e.pic) + `"]}`} {
		if r := e.asJSON(t, "viewer", "POST", f+"/assets/check", body); r.Status != 400 {
			t.Fatalf("check %s: %d", body[:min(len(body), 40)], r.Status)
		}
	}
	// Ack validation.
	cursor := e.pull(t, "viewer", e.family, "", feed{}, 100)
	for body, want := range map[string]int{`{"cursor":""}`: 400, `{"cursor":"abc"}`: 400, `{"cursor":"` + cursorAt(t, cursor, 99) + `"}`: 409,
		`{"cursor":"0000000000000000.1"}`: 409, `{"cursor":"` + cursor + `"}`: 204} {
		if r := e.asJSON(t, "viewer", "PUT", f+"/sync/cursor", body); r.Status != want {
			t.Fatalf("ack %s: %d %s, want %d", body, r.Status, r.Body, want)
		}
	}
}
