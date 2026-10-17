package httpapi_test

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

// aclEnv is a server with one folder ("family") owned by a USER allowed to
// create albums, one member per role, a USER without any relation, and the
// server administrator ("root", the claimed bootstrap account), who owns a
// folder of its own ("admin-only") and is not a member of "family".
type aclEnv struct {
	contractEnv
	faults *faultMap
	ids    map[string]string // login -> user_id
	tokens map[string]string // login -> access token
	family string            // folder ids
	adminF string
	photo  string // asset id of a photo in family (uploaded by the owner)
	pic    []byte
	other  string // asset id of a photo in admin-only
}

var aclRoles = []string{"owner", "contributor", "viewer", "uploader", "outsider", "root"}

func newACLEnv(t *testing.T) *aclEnv {
	t.Helper()
	faults := &faultMap{}
	e := &aclEnv{contractEnv: newContractEnvWith(t, openSQLiteStore, func(c *httpapi.Config) { c.Faults = faults }),
		faults: faults, ids: map[string]string{}, tokens: map[string]string{}}
	us := users.New(e.Meta.DB())
	root, err := us.GetByLogin(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	e.ids["root"], e.tokens["root"] = root.ID, e.Token
	hash, _ := password.Hash(userPassword, fastParams)
	for _, login := range aclRoles[:5] {
		u, err := us.Create(t.Context(), users.NewUser{LoginName: login, DisplayName: login, PasswordHash: hash,
			Role: users.RoleUser, CanCreateAlbums: login == "owner"}, users.Console)
		if err != nil {
			t.Fatal(err)
		}
		e.ids[login] = u.ID
		e.tokens[login] = loginToken(t, e.Raw, login, userPassword)
	}
	e.family = wantJSON(t, "create family", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"family"}`), nil), 201)["id"].(string)
	for _, m := range [][2]string{{"contributor", "CONTRIBUTOR"}, {"viewer", "VIEWER"}, {"uploader", "UPLOADER"}} {
		e.setMember(t, m[0], m[1])
	}
	e.pic = jpegBytes(t, 40, 30)
	e.photo = wantJSON(t, "owner upload", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/assets", e.pic, map[string]string{"X-Filename": "owner.jpg"}), 201)["asset_id"].(string)
	e.adminF = wantJSON(t, "create admin folder", e.as(t, "root", "POST", "/api/v1/folders", []byte(`{"name":"admin-only"}`), nil), 201)["id"].(string)
	e.other = wantJSON(t, "admin upload", e.as(t, "root", "POST", "/api/v1/folders/"+e.adminF+"/assets", jpegBytes(t, 7, 7), nil), 201)["asset_id"].(string)
	return e
}

// as sends a request with the access token of login.
func (e *aclEnv) as(t *testing.T, login, method, path string, body []byte, hdr map[string]string) reply {
	t.Helper()
	h := map[string]string{"Authorization": "Bearer " + e.tokens[login]}
	for k, v := range hdr {
		h[k] = v
	}
	return do(t, e.Raw, method, path, body, h)
}

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// setMember sets login's role in family ("" removes the membership).
func (e *aclEnv) setMember(t *testing.T, login, role string) {
	t.Helper()
	db := e.Meta.DB()
	if _, err := db.Exec("DELETE FROM folder_members WHERE folder_id = ? AND user_id = ?", e.family, e.ids[login]); err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if _, err := db.Exec("INSERT INTO folder_members (folder_id, user_id, role) VALUES (?, ?, ?)", e.family, e.ids[login], role); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *aclEnv) events(t *testing.T, folder string) int {
	t.Helper()
	p, err := e.Meta.ListEvents(t.Context(), folder, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(p.Events)
}

// TestACLMatrix: every album operation for every relation to the folder.
// 404 hides the folder (outsider, server administrator); 403 is a known
// folder whose role does not allow the operation.
func TestACLMatrix(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	type op struct {
		name, method, path string
		body               func(login string) []byte
		want               map[string]int
		code               map[int]string
	}
	upload := func(login string) []byte { return []byte("matrix upload by " + login) }
	hidden := map[int]string{404: "folder_not_found", 403: "forbidden"}
	ops := []op{
		{"metadata", "GET", f, nil, map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 200, "outsider": 404, "root": 404}, hidden},
		{"sync", "GET", f + "/sync", nil, map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 403, "outsider": 404, "root": 404}, hidden},
		{"original", "GET", f + "/assets/" + e.photo + "/original", nil, map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 403, "outsider": 404, "root": 404}, hidden},
		{"preview", "GET", f + "/assets/" + e.photo + "/preview", nil, map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 403, "outsider": 404, "root": 404}, hidden},
		{"upload", "POST", f + "/assets", upload, map[string]int{"owner": 201, "contributor": 201, "viewer": 403, "uploader": 202, "outsider": 404, "root": 404}, hidden},
		{"create folder", "POST", "/api/v1/folders", func(string) []byte { return []byte(`{"name":"new"}`) },
			map[string]int{"owner": 201, "contributor": 403, "viewer": 403, "uploader": 403, "outsider": 403, "root": 201}, map[int]string{403: "forbidden"}},
		{"admin api", "GET", "/api/v1/admin/users", nil, map[string]int{"owner": 403, "contributor": 403, "viewer": 403, "uploader": 403, "outsider": 403, "root": 200}, map[int]string{403: "forbidden"}},
	}
	for _, o := range ops {
		for _, login := range aclRoles {
			t.Run(o.name+"/"+login, func(t *testing.T) {
				var body []byte
				if o.body != nil {
					body = o.body(login)
				}
				r := e.as(t, login, o.method, o.path, body, nil)
				want := o.want[login]
				if r.Status != want {
					t.Fatalf("%s %s as %s: %d %s, want %d", o.method, o.path, login, r.Status, r.Body, want)
				}
				if code, ok := o.code[want]; ok {
					wantError(t, o.name, r, want, code)
				}
			})
		}
	}
	// The folder list shows only own and shared folders, with the role.
	for _, login := range aclRoles {
		list := wantJSON(t, "list", e.as(t, login, "GET", "/api/v1/folders", nil, nil), 200)["folders"].([]any)
		roles := map[string]string{}
		for _, item := range list {
			m := item.(map[string]any)
			roles[m["name"].(string)] = m["role"].(string)
		}
		want := map[string]map[string]string{
			"owner":       {"family": "OWNER", "new": "OWNER"},
			"contributor": {"family": "CONTRIBUTOR"},
			"viewer":      {"family": "VIEWER"},
			"uploader":    {"family": "UPLOADER"},
			"outsider":    {},
			"root":        {"admin-only": "OWNER", "new": "OWNER"},
		}[login]
		if fmt.Sprint(roles) != fmt.Sprint(want) {
			t.Fatalf("%s sees %v, want %v", login, roles, want)
		}
	}
	m := wantJSON(t, "uploader metadata", e.as(t, "uploader", "GET", f, nil, nil), 200)
	if m["role"] != "UPLOADER" || m["can_read"] != false || m["can_upload"] != true {
		t.Fatalf("uploader metadata %v", m)
	}
}

// TestUnauthenticatedRequestsAreRefused: every declared route except the
// public ones answers 401 without a token, with a bad token, and with a
// cookie but nothing else; unknown API paths too.
func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	e := newACLEnv(t)
	param := regexp.MustCompile(`\{[A-Za-z]+\}`)
	var paths [][2]string
	public := 0
	for _, rp := range httpapi.Routes() {
		method, path, ok := strings.Cut(rp.Pattern, " ")
		if !ok {
			method, path = "GET", rp.Pattern
		}
		if rp.Access == httpapi.AccessPublic {
			public++
			continue
		}
		if rp.Access != httpapi.AccessSession && rp.Access != httpapi.AccessServer && rp.Access != httpapi.AccessAlbum {
			t.Fatalf("route %s has access %q", rp.Pattern, rp.Access)
		}
		// Real ids, so that a missing check could not hide behind a 404.
		path = strings.ReplaceAll(path, "{folderID}", e.family)
		path = strings.ReplaceAll(path, "{assetID}", e.photo)
		path = strings.ReplaceAll(path, "{userID}", e.ids["owner"])
		path = param.ReplaceAllString(path, "x")
		paths = append(paths, [2]string{method, path})
	}
	if public != 4 {
		t.Fatalf("%d public routes, want 4 (dashboard page, health, login, refresh)", public)
	}
	paths = append(paths, [2]string{"GET", "/api/v1/assets/" + shaHex(e.pic) + "/original"}, [2]string{"DELETE", "/api/v1/folders/" + e.family},
		[2]string{"GET", "/api/v1/nope"}, [2]string{"POST", "/api/v1/folders/" + e.family + "/sync"})
	for _, mp := range paths {
		for name, hdr := range map[string]map[string]string{
			"no token":    nil,
			"bad token":   {"Authorization": "Bearer " + strings.Repeat("A", 43)},
			"basic auth":  {"Authorization": "Basic cm9vdDpwdw=="},
			"csrf cookie": {"Cookie": "withalbum_csrf=x"},
		} {
			wantError(t, mp[0]+" "+mp[1]+" "+name, do(t, e.Raw, mp[0], mp[1], []byte("x"), hdr), 401, "unauthorized")
		}
	}
	if n := e.events(t, e.family); n != 1 {
		t.Fatalf("refused requests changed the folder: %d events", n)
	}
	// Public routes stay reachable.
	wantJSON(t, "health", do(t, e.Raw, "GET", "/api/v1/health", nil, nil), 200)
	if r := do(t, e.Raw, "GET", "/", nil, nil); r.Status != 200 {
		t.Fatalf("dashboard %d", r.Status)
	}
}

// TestUploaderLearnsNothing: an UPLOADER cannot tell new bytes from bytes
// already stored for another folder or already in this folder, and cannot
// read anything back, not even to check whether an asset id exists.
func TestUploaderLearnsNothing(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	adminBytes := []byte("already stored in a folder the uploader cannot see")
	wantJSON(t, "admin upload", e.as(t, "root", "POST", "/api/v1/folders/"+e.adminF+"/assets", adminBytes, nil), 201)
	cases := map[string][]byte{
		"new to the server":       []byte("bytes nobody uploaded before"),
		"in another folder":       adminBytes,
		"already in this folder":  e.pic,
		"own bytes, second time":  []byte("bytes nobody uploaded before"),
		"new to the server again": []byte("more fresh bytes"),
	}
	order := []string{"new to the server", "in another folder", "already in this folder", "own bytes, second time", "new to the server again"}
	for _, name := range order {
		body := cases[name]
		r := e.as(t, "uploader", "POST", f+"/assets", body, map[string]string{"X-Filename": "x.jpg"})
		m := wantJSON(t, name, r, 202)
		want := map[string]any{"result": "accepted", "folder_id": e.family, "sha256": shaHex(body), "bytes": float64(len(body))}
		if fmt.Sprint(m) != fmt.Sprint(want) {
			t.Fatalf("%s: receipt %v, want %v (only what the client sent)", name, m, want)
		}
		hdr := slices.Sorted(func(yield func(string) bool) {
			for k := range r.Header {
				if !yield(k) {
					return
				}
			}
		})
		if strings.Join(hdr, ",") != "Cache-Control,Content-Security-Policy,Content-Type,Cross-Origin-Resource-Policy,Pragma,Referrer-Policy,X-Content-Type-Options,X-Frame-Options" {
			t.Fatalf("%s: headers %v", name, hdr)
		}
	}
	// Reading back: the same 403 for an existing asset, an unknown id, the
	// asset id of another folder and the sync feed.
	var first string
	for _, path := range []string{f + "/assets/" + e.photo + "/original", f + "/assets/" + strings.Repeat("0", 32) + "/original",
		f + "/assets/" + e.other + "/preview", f + "/assets/" + e.photo + "/preview", f + "/sync", f + "/sync?cursor=abc"} {
		r := e.as(t, "uploader", "GET", path, nil, nil)
		wantError(t, path, r, 403, "forbidden")
		if first == "" {
			first = string(r.Body)
		} else if string(r.Body) != first {
			t.Fatalf("%s: body %s differs from %s", path, r.Body, first)
		}
	}
	// Same blob work in every case: the stored preview is regenerated (not
	// skipped) when the bytes are already known.
	pv := e.Root + "/.system/previews/" + shaHex(e.pic)[:2] + "/" + shaHex(e.pic) + ".jpg"
	before := mtime(t, pv)
	time.Sleep(20 * time.Millisecond)
	e.as(t, "uploader", "POST", f+"/assets", e.pic, nil)
	if !mtime(t, pv).After(before) {
		t.Fatal("known bytes skipped the preview step")
	}
	// Timing is not asserted (too noisy); log it for the record.
	measure := func(body func(i int) []byte) time.Duration {
		var ds []time.Duration
		for i := 0; i < 7; i++ {
			start := time.Now()
			e.as(t, "uploader", "POST", f+"/assets", body(i), nil)
			ds = append(ds, time.Since(start))
		}
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	fresh := measure(func(i int) []byte { return jpegBytes(t, 300+i, 200) })
	dup := measure(func(i int) []byte { return jpegBytes(t, 300+i, 200) })
	t.Logf("uploader median latency: new bytes %v, already in folder %v", fresh, dup)
}

// TestServerAdminCannotReadOthersAlbums: SERVER_ADMIN gets exactly what a
// stranger gets for a folder it neither owns nor is a member of.
func TestServerAdminCannotReadOthersAlbums(t *testing.T) {
	e := newACLEnv(t)
	unknown := strings.Repeat("0", 32)
	for _, suffix := range []string{"", "/sync", "/assets/" + e.photo + "/original", "/assets/" + e.photo + "/preview"} {
		real := e.as(t, "root", "GET", "/api/v1/folders/"+e.family+suffix, nil, nil)
		fake := e.as(t, "root", "GET", "/api/v1/folders/"+unknown+strings.ReplaceAll(suffix, e.photo, unknown), nil, nil)
		wantError(t, "admin "+suffix, real, 404, "folder_not_found")
		if real.Status != fake.Status || string(real.Body) != string(fake.Body) {
			t.Fatalf("%s: existing folder %d %s, unknown %d %s", suffix, real.Status, real.Body, fake.Status, fake.Body)
		}
	}
	wantError(t, "admin upload", e.as(t, "root", "POST", "/api/v1/folders/"+e.family+"/assets", []byte("x"), nil), 404, "folder_not_found")
	if n := e.events(t, e.family); n != 1 {
		t.Fatalf("events %d", n)
	}
}

// TestCrossFolderAssetIDs (IDOR): an asset id only works under its own
// folder, even for a user who can read both folders.
func TestCrossFolderAssetIDs(t *testing.T) {
	e := newACLEnv(t)
	// The owner gets a second folder and reads both.
	second := wantJSON(t, "second", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"second"}`), nil), 201)["id"].(string)
	inSecond := wantJSON(t, "same bytes", e.as(t, "owner", "POST", "/api/v1/folders/"+second+"/assets", e.pic, nil), 201)["asset_id"].(string)
	if inSecond == e.photo {
		t.Fatal("the same bytes in two folders share an asset id")
	}
	for _, c := range []struct{ folder, asset, who string }{
		{second, e.photo, "owner"}, {e.family, inSecond, "owner"}, {e.family, e.other, "viewer"}, {e.family, e.other, "owner"},
	} {
		wantError(t, c.who+" "+c.asset, e.as(t, c.who, "GET", "/api/v1/folders/"+c.folder+"/assets/"+c.asset+"/original", nil, nil), 404, "asset_not_found")
	}
	wantError(t, "viewer via the asset's folder", e.as(t, "viewer", "GET", "/api/v1/folders/"+e.adminF+"/assets/"+e.other+"/original", nil, nil), 404, "folder_not_found")
	// Readers never see the other folder's filename for shared bytes.
	assets := wantJSON(t, "sync second", e.as(t, "owner", "GET", "/api/v1/folders/"+second+"/sync", nil, nil), 200)["assets"].(map[string]any)
	if a := assets[inSecond].(map[string]any); a["filename"] != shaHex(e.pic) {
		t.Fatalf("second folder asset %v", a)
	}
}

// TestRevocationTakesEffectOnNextRequest: role changes, removal and
// disabling the account apply to the very next request.
func TestRevocationTakesEffectOnNextRequest(t *testing.T) {
	e := newACLEnv(t)
	orig := "/api/v1/folders/" + e.family + "/assets/" + e.photo + "/original"
	if r := e.as(t, "viewer", "GET", orig, nil, nil); r.Status != 200 {
		t.Fatalf("viewer read %d", r.Status)
	}
	e.setMember(t, "viewer", "UPLOADER")
	wantError(t, "downgraded", e.as(t, "viewer", "GET", orig, nil, nil), 403, "forbidden")
	e.setMember(t, "viewer", "")
	wantError(t, "removed", e.as(t, "viewer", "GET", orig, nil, nil), 404, "folder_not_found")
	if list := wantJSON(t, "list", e.as(t, "viewer", "GET", "/api/v1/folders", nil, nil), 200)["folders"].([]any); len(list) != 0 {
		t.Fatalf("removed member still lists %v", list)
	}
	e.setMember(t, "viewer", "CONTRIBUTOR")
	wantJSON(t, "upgraded upload", e.as(t, "viewer", "POST", "/api/v1/folders/"+e.family+"/assets", []byte("contrib"), nil), 201)
	// Album creation permission.
	us := users.New(e.Meta.DB())
	if err := us.SetCanCreateAlbums(t.Context(), e.ids["owner"], false, users.Console); err != nil {
		t.Fatal(err)
	}
	wantError(t, "creation revoked", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"x"}`), nil), 403, "forbidden")
	// Disabled account: 401 everywhere.
	if err := us.SetStatus(t.Context(), e.ids["contributor"], users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	wantError(t, "disabled", e.as(t, "contributor", "GET", orig, nil, nil), 401, "unauthorized")
}

// TestPermissionRevokedDuringUpload: the permission is checked again right
// before the commit; an upload whose permission disappears meanwhile is
// refused and leaves no asset, link or event.
func TestPermissionRevokedDuringUpload(t *testing.T) {
	e := newACLEnv(t)
	for _, c := range []struct {
		name   string
		revoke func()
		status int
		code   string
	}{
		{"downgraded to viewer", func() { e.setMember(t, "contributor", "VIEWER") }, 403, "forbidden"},
		{"removed", func() { e.setMember(t, "contributor", "") }, 404, "folder_not_found"},
		{"session revoked", func() {
			if _, err := e.Meta.DB().Exec("UPDATE auth_sessions SET revoked_at = 'x' WHERE device_id IN (SELECT device_id FROM devices WHERE user_id = ?)", e.ids["contributor"]); err != nil {
				t.Fatal(err)
			}
		}, 404, "folder_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.setMember(t, "contributor", "CONTRIBUTOR")
			e.tokens["contributor"] = loginToken(t, e.Raw, "contributor", userPassword)
			body := []byte("uploaded while being revoked: " + c.name)
			before := e.events(t, e.family)
			var once sync.Once
			// After the body is stored and promoted, before the commit.
			e.faults.set(httpapi.FaultAfterPreview, func() error { once.Do(c.revoke); return nil })
			r := e.as(t, "contributor", "POST", "/api/v1/folders/"+e.family+"/assets", body, nil)
			e.faults.set(httpapi.FaultAfterPreview, nil)
			wantError(t, c.name, r, c.status, c.code)
			if n := e.events(t, e.family); n != before {
				t.Fatalf("events %d -> %d", before, n)
			}
			var n int
			if err := e.Meta.DB().QueryRow("SELECT count(*) FROM assets WHERE original_sha256 = ?", shaHex(body)).Scan(&n); err != nil || n != 0 {
				t.Fatalf("asset rows %d %v", n, err)
			}
		})
	}
	// Restored permission: the next upload works.
	e.setMember(t, "contributor", "CONTRIBUTOR")
	e.tokens["contributor"] = loginToken(t, e.Raw, "contributor", userPassword)
	r := e.as(t, "contributor", "POST", "/api/v1/folders/"+e.family+"/assets", []byte("fine"), nil)
	wantJSON(t, "allowed again", r, 201)
}

// TestDashboardCookieSession: the browser flow (cookies + CSRF) reaches the
// folder routes with the same rules.
func TestDashboardCookieSession(t *testing.T) {
	e := newACLEnv(t)
	r := do(t, e.Raw, "POST", "/api/v1/auth/login", []byte(`{"login_name":"owner","password":"`+userPassword+`","device_name":"Web dashboard","platform":"web"}`),
		map[string]string{"Content-Type": "application/json"})
	m := wantJSON(t, "web login", r, 200)
	csrf := m["csrf_token"].(string)
	var cookies []string
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	jar := strings.Join(cookies, "; ")
	list := wantJSON(t, "cookie list", do(t, e.Raw, "GET", "/api/v1/folders", nil, map[string]string{"Cookie": jar}), 200)["folders"].([]any)
	if len(list) != 1 {
		t.Fatalf("folders %v", list)
	}
	wantError(t, "no csrf", do(t, e.Raw, "POST", "/api/v1/folders", []byte(`{"name":"w"}`), map[string]string{"Cookie": jar}), 403, "csrf_failed")
	wantError(t, "upload no csrf", do(t, e.Raw, "POST", "/api/v1/folders/"+e.family+"/assets", []byte("w"), map[string]string{"Cookie": jar}), 403, "csrf_failed")
	wantJSON(t, "csrf create", do(t, e.Raw, "POST", "/api/v1/folders", []byte(`{"name":"w"}`), map[string]string{"Cookie": jar, "X-CSRF-Token": csrf}), 201)
	wantJSON(t, "csrf upload", do(t, e.Raw, "POST", "/api/v1/folders/"+e.family+"/assets", []byte("w"), map[string]string{"Cookie": jar, "X-CSRF-Token": csrf}), 201)
}

func TestHandlerRequiresAuth(t *testing.T) {
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
	store := openSQLiteStore(t, vol)
	defer store.Close()
	defer func() {
		if recover() == nil {
			t.Fatal("Handler without Auth did not panic")
		}
	}()
	httpapi.New(httpapi.Config{Volume: vol, Meta: store, Blobs: blobs}).Handler()
}
