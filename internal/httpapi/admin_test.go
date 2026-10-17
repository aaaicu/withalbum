package httpapi_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/storage"
)

// inlineBody returns the text between <tag> and </tag> of page.
func inlineBody(t *testing.T, page, tag string) string {
	t.Helper()
	_, rest, ok := strings.Cut(page, "<"+tag+">")
	body, _, ok2 := strings.Cut(rest, "</"+tag+">")
	if !ok || !ok2 {
		t.Fatalf("page has no <%s>", tag)
	}
	return body
}

func cspHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// TestDashboardPage: the page is served without a session, carries no data,
// allows exactly its own inline script and style by hash, and never writes
// HTML strings into the DOM. Without a session every data API answers 401,
// which the page turns into its login view.
func TestDashboardPage(t *testing.T) {
	e := newACLEnv(t)
	r := do(t, e.Raw, "GET", "/", nil, nil)
	if r.Status != 200 || r.Header.Get("Content-Type") != "text/html; charset=utf-8" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page %d %q %q", r.Status, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"))
	}
	page := string(r.Body)
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src " + cspHash(inlineBody(t, page, "script")) + ";",
		"style-src " + cspHash(inlineBody(t, page, "style")) + ";", "connect-src 'self'", "img-src 'none'", "base-uri 'none'",
		"form-action 'none'", "frame-ancestors 'none'", "require-trusted-types-for 'script'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "*") {
		t.Errorf("CSP too broad: %q", csp)
	}
	if !strings.Contains(page, `id="loginBox"`) || !strings.Contains(page, `id="login"`) {
		t.Fatal("page has no login form")
	}
	for _, leak := range []string{e.family, e.ids["owner"], "owner.jpg"} {
		if strings.Contains(page, leak) {
			t.Fatalf("static page contains data %q", leak)
		}
	}
	// No HTML string sinks, no inline handlers or style attributes (they
	// would also be refused by the CSP).
	for _, re := range []string{`innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document\.write`, `\beval\(`, `new Function`,
		`\son[a-z]+\s*=\s*["']`, `\sstyle\s*=\s*["']`, `javascript:`, `<script[^>]`} {
		if regexp.MustCompile(re).MatchString(page) {
			t.Errorf("page matches %s", re)
		}
	}
	// Every API path the page names is a prefix of a declared route.
	var paths []string
	for _, rp := range httpapi.Routes() {
		if _, p, ok := strings.Cut(rp.Pattern, " "); ok {
			paths = append(paths, p)
		}
	}
	lits := regexp.MustCompile(`'(/api/v1/[^']*)'`).FindAllStringSubmatch(inlineBody(t, page, "script"), -1)
	if len(lits) < 10 {
		t.Fatalf("found only %d API paths in the page", len(lits))
	}
	for _, m := range lits {
		ok := false
		for _, p := range paths {
			ok = ok || strings.HasPrefix(p, m[1])
		}
		if !ok {
			t.Errorf("page calls %s, which is not a route", m[1])
		}
	}
	for _, p := range []string{"/api/v1/me", "/api/v1/folders", "/api/v1/invitations", "/api/v1/admin/users", "/api/v1/admin/server/storage"} {
		res := do(t, e.Raw, "GET", p, nil, nil)
		wantError(t, p, res, 401, "unauthorized")
		if res.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate", p)
		}
	}
}

// TestDashboardDesignRules pins the rules of the dashboard design (SEED
// "keep green"): no wording that claims an upload is done before the server
// verified it, icons are decorative inline SVG, nothing is loaded from
// elsewhere (fonts, images, scripts, styles), no emoji, and light and dark
// themes come from the page's own tokens.
func TestDashboardDesignRules(t *testing.T) {
	e := newACLEnv(t)
	page := string(do(t, e.Raw, "GET", "/", nil, nil).Body)
	// "업로드 완료" (upload done) and a bare "완료" (done) are not used: an
	// original is "보관됨" (kept) only after the server verified it.
	if strings.Contains(page, "업로드 완료") {
		t.Error(`page says "업로드 완료"`)
	}
	if m := regexp.MustCompile(`[>'"]\s*완료\s*[.!]?\s*[<'"]`).FindString(page); m != "" {
		t.Errorf("page uses a bare 완료: %q", m)
	}
	if !strings.Contains(page, "보관됨") {
		t.Error(`page never says "보관됨"`)
	}
	// Every icon is inline SVG hidden from assistive technology.
	svgs := regexp.MustCompile(`<svg[^>]*>`).FindAllString(page, -1)
	if len(svgs) < 10 {
		t.Fatalf("only %d inline SVG icons", len(svgs))
	}
	for _, s := range svgs {
		if !strings.Contains(s, `aria-hidden="true"`) {
			t.Errorf("icon without aria-hidden: %s", s)
		}
	}
	// Nothing remote or external: the CSP would refuse it anyway.
	for _, re := range []string{`https?://`, `@import`, `@font-face`, `url\(`, `<link\b`, `<img\b`, `<iframe\b`, `\ssrc\s*=`} {
		if regexp.MustCompile(re).MatchString(page) {
			t.Errorf("page matches %s", re)
		}
	}
	for _, r := range page {
		if (r >= 0x1F000 && r <= 0x1FAFF) || (r >= 0x2600 && r <= 0x27BF) || r == 0xFE0F {
			t.Errorf("page contains emoji or pictograph %U", r)
		}
	}
	style := inlineBody(t, page, "style")
	for _, want := range []string{"@media (prefers-color-scheme: dark)", "--bg-brand-solid: #00745f", "--bg-brand-solid: #117956",
		"--bg-neutral-solid: #2a3038", "-apple-system", "Apple SD Gothic Neo", "seed-design, Apache-2.0"} {
		if !strings.Contains(style, want) {
			t.Errorf("style lacks %q", want)
		}
	}
}

// TestAdminServerStorageUnavailable: with the volume marker gone, the
// storage state the dashboard shows as "storage stopped" (D-03) comes from
// the existing APIs, without the path or the volume identifier.
func TestAdminServerStorageUnavailable(t *testing.T) {
	e := newACLEnv(t)
	if err := os.Remove(filepath.Join(e.Root, storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	h := wantJSON(t, "health", do(t, e.Raw, "GET", "/api/v1/health", nil, nil), 200)
	if h["storage"] != "unavailable" {
		t.Fatalf("health %v", h)
	}
	r := e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil)
	m := wantJSON(t, "storage", r, 200)
	wantKeys(t, "storage", m, "storage", "integrity", "free_bytes", "min_free_bytes")
	if m["storage"] != "unavailable" {
		t.Fatalf("storage %v", m)
	}
	if strings.Contains(string(r.Body), e.Root) || strings.Contains(string(r.Body), e.Volume.ID()) || strings.Contains(string(r.Body), storage.MarkerName) {
		t.Fatalf("storage reply leaks details: %s", r.Body)
	}
	wantError(t, "create folder while stopped", e.asJSON(t, "root", "POST", "/api/v1/folders", `{"name":"x"}`), 503, "storage_unavailable")
	for _, login := range []string{"owner", "outsider"} {
		wantError(t, login, e.as(t, login, "GET", "/api/v1/admin/server/storage", nil, nil), 403, "forbidden")
	}
}

// webLogin logs in as a browser and returns the cookie header and the CSRF
// token.
func webLogin(t *testing.T, h http.Handler, login, pw string) (string, string) {
	t.Helper()
	r := do(t, h, "POST", "/api/v1/auth/login", []byte(`{"login_name":`+mustJSON(t, login)+`,"password":`+mustJSON(t, pw)+
		`,"device_name":"Web dashboard","platform":"web"}`), jsonHdr)
	m := wantJSON(t, "web login", r, 200)
	return cookieHeader((&http.Response{Header: r.Header}).Cookies()), m["csrf_token"].(string)
}

// TestDashboardCSRF: with the browser's cookie session, every change the
// dashboard makes needs the CSRF header; reads do not.
func TestDashboardCSRF(t *testing.T) {
	e := newACLEnv(t)
	jar, token := webLogin(t, e.Raw, "root", adminPassword)
	cookie := func(method, path, body, csrf string) reply {
		h := map[string]string{"Cookie": jar, "Content-Type": "application/json", "X-CSRF-Token": csrf}
		var b []byte
		if body != "" {
			b = []byte(body)
		}
		return do(t, e.Raw, method, path, b, h)
	}
	wantJSON(t, "cookie GET storage", cookie("GET", "/api/v1/admin/server/storage", "", ""), 200)
	wantJSON(t, "cookie GET owned folders", cookie("GET", "/api/v1/admin/users/"+e.ids["owner"]+"/folders", "", ""), 200)
	wantStatus204 := func(what string, r reply) {
		t.Helper()
		if r.Status != 204 {
			t.Fatalf("%s: %d %s", what, r.Status, r.Body)
		}
	}
	wantStatus204("owner archives family", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family, nil, nil))
	unsafe := []struct{ method, path, body string }{
		{"POST", "/api/v1/admin/folders/" + e.family + "/owner", `{"new_owner_user_id":"` + e.ids["viewer"] + `"}`},
		{"PATCH", "/api/v1/admin/users/" + e.ids["outsider"], `{"status":"disabled"}`},
		{"PATCH", "/api/v1/admin/users/" + e.ids["outsider"] + "/permissions", `{"can_create_albums":true}`},
		{"POST", "/api/v1/admin/users", `{"login_name":"newbie","password":"newbie password 123"}`},
		{"POST", "/api/v1/folders", `{"name":"x"}`},
		{"DELETE", "/api/v1/folders/" + e.adminF, ""},
		{"POST", "/api/v1/auth/logout", ""},
	}
	for _, u := range unsafe {
		wantError(t, u.method+" "+u.path+" without CSRF", cookie(u.method, u.path, u.body, ""), 403, "csrf_failed")
		wantError(t, u.method+" "+u.path+" wrong CSRF", cookie(u.method, u.path, u.body, token+"x"), 403, "csrf_failed")
	}
	if e.ownerOf(t, e.family) != "owner" || e.audits(t, "folder.reassign_archived_owner") != 0 || e.audits(t, "account.disable") != 0 {
		t.Fatal("a request without CSRF token changed something")
	}
	wantJSON(t, "reassign with CSRF", cookie(unsafe[0].method, unsafe[0].path, unsafe[0].body, token), 200)
	wantJSON(t, "permissions with CSRF", cookie(unsafe[2].method, unsafe[2].path, unsafe[2].body, token), 200)
}

// TestAdminServerStorage: SERVER_ADMIN only; no path or volume identifier.
func TestAdminServerStorage(t *testing.T) {
	e := newACLEnv(t)
	wantError(t, "unauthenticated", do(t, e.Raw, "GET", "/api/v1/admin/server/storage", nil, nil), 401, "unauthorized")
	for _, login := range []string{"owner", "viewer", "outsider"} {
		wantError(t, login, e.as(t, login, "GET", "/api/v1/admin/server/storage", nil, nil), 403, "forbidden")
	}
	r := e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil)
	m := wantJSON(t, "admin", r, 200)
	wantKeys(t, "storage", m, "storage", "integrity", "free_bytes", "min_free_bytes")
	if m["storage"] != "ok" || m["integrity"] != "ok" {
		t.Fatalf("storage %v", m)
	}
	if strings.Contains(string(r.Body), e.Root) || strings.Contains(string(r.Body), e.Volume.ID()) {
		t.Fatalf("storage reply leaks details: %s", r.Body)
	}

	c := newContractEnvWith(t, openSQLiteStore, func(c *httpapi.Config) {
		c.IntegrityWarning, c.MinFreeBytes = true, 4096
		c.FreeBytes = func() (uint64, error) { return 1 << 30, nil }
		c.StartupCheck = &httpapi.StartupCheck{Blobs: 9, Missing: 1, SizeMismatch: 2, Orphans: 3, Unexpected: 4}
	})
	m = wantJSON(t, "admin with report", do(t, c.Handler, "GET", "/api/v1/admin/server/storage", nil, nil), 200)
	sc, _ := m["startup_check"].(map[string]any)
	if m["integrity"] != "warning" || m["free_bytes"] != float64(1<<30) || m["min_free_bytes"] != float64(4096) ||
		sc["blobs"] != float64(9) || sc["missing"] != float64(1) || sc["size_mismatch"] != float64(2) || sc["orphans"] != float64(3) || sc["unexpected"] != float64(4) {
		t.Fatalf("storage with report %v", m)
	}
}

// TestReassignArchivedOwner: the permission matrix of the administrator's
// reassignment of archived folders, and that it grants nobody access.
func TestReassignArchivedOwner(t *testing.T) {
	e := newACLEnv(t)
	reassign := func(login, folder, newOwner string) reply {
		t.Helper()
		return e.asJSON(t, login, "POST", "/api/v1/admin/folders/"+folder+"/owner", `{"new_owner_user_id":"`+newOwner+`"}`)
	}
	owned := func() map[string]any {
		t.Helper()
		return wantJSON(t, "owned folders", e.as(t, "root", "GET", "/api/v1/admin/users/"+e.ids["owner"]+"/folders", nil, nil), 200)
	}
	if m := owned(); m["active_owned"] != float64(1) || len(m["archived"].([]any)) != 0 {
		t.Fatalf("before archiving: %v", m)
	}
	// Not archived: refused like an unknown folder, for its owner's folder
	// and for the administrator's own.
	wantError(t, "folder in use", reassign("root", e.family, e.ids["contributor"]), 404, "folder_not_found")
	wantError(t, "own folder in use", reassign("root", e.adminF, e.ids["owner"]), 404, "folder_not_found")
	wantError(t, "unknown folder", reassign("root", "nope", e.ids["owner"]), 404, "folder_not_found")
	if e.ownerOf(t, e.family) != "owner" || e.ownerOf(t, e.adminF) != "root" {
		t.Fatal("a folder in use changed owner")
	}
	wantError(t, "disable owner of a folder in use", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["owner"], `{"status":"disabled"}`), 409, "owns_albums")

	if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family, nil, nil); r.Status != 204 {
		t.Fatalf("archive %d", r.Status)
	}
	m := owned()
	arch := m["archived"].([]any)
	if m["active_owned"] != float64(0) || len(arch) != 1 {
		t.Fatalf("after archiving: %v", m)
	}
	item := arch[0].(map[string]any)
	wantKeys(t, "archived folder", item, "folder_id", "created_at", "archived_at")
	if item["folder_id"] != e.family {
		t.Fatalf("archived %v", item)
	}
	wantError(t, "still owns an archived folder", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["owner"], `{"status":"disabled"}`), 409, "owns_albums")

	// Only a SERVER_ADMIN, whatever the relation to the folder.
	for _, login := range []string{"owner", "contributor", "viewer", "uploader", "outsider"} {
		wantError(t, login+" reassigns", reassign(login, e.family, e.ids[login]), 403, "forbidden")
		wantError(t, login+" lists owned folders", e.as(t, login, "GET", "/api/v1/admin/users/"+e.ids["owner"]+"/folders", nil, nil), 403, "forbidden")
	}
	wantError(t, "unauthenticated", do(t, e.Raw, "POST", "/api/v1/admin/folders/"+e.family+"/owner", []byte(`{}`), jsonHdr), 401, "unauthorized")
	wantError(t, "unknown user", e.as(t, "root", "GET", "/api/v1/admin/users/nope/folders", nil, nil), 404, "user_not_found")
	wantError(t, "same owner", reassign("root", e.family, e.ids["owner"]), 409, "target_is_owner")
	wantError(t, "unknown new owner", reassign("root", e.family, "nope"), 404, "user_not_found")
	wantError(t, "no new owner", e.asJSON(t, "root", "POST", "/api/v1/admin/folders/"+e.family+"/owner", `{}`), 400, "invalid_request")
	wantError(t, "not JSON", e.as(t, "root", "POST", "/api/v1/admin/folders/"+e.family+"/owner", []byte(`{}`), nil), 415, "json_required")
	wantJSON(t, "disable outsider", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["outsider"], `{"status":"disabled"}`), 200)
	wantError(t, "disabled new owner", reassign("root", e.family, e.ids["outsider"]), 409, "user_inactive")

	// A failure right before COMMIT changes nothing.
	e.faults.set(httpapi.FaultMembershipBeforeCommit, func() error { return errors.New("injected") })
	wantError(t, "fault", reassign("root", e.family, e.ids["contributor"]), 500, "metadata_write_failed")
	e.faults.set(httpapi.FaultMembershipBeforeCommit, nil)
	if e.ownerOf(t, e.family) != "owner" || e.audits(t, "folder.reassign_archived_owner") != 0 {
		t.Fatal("failed reassignment left a change")
	}

	// The contributor (a member) becomes owner; its membership goes, the
	// previous owner does not become a member, and the folder stays hidden.
	r := wantJSON(t, "reassign", reassign("root", e.family, e.ids["contributor"]), 200)
	wantKeys(t, "reassign", r, "folder_id", "previous_owner_user_id", "owner_user_id", "archived_at")
	if r["folder_id"] != e.family || r["previous_owner_user_id"] != e.ids["owner"] || r["owner_user_id"] != e.ids["contributor"] || r["archived_at"] == "" {
		t.Fatalf("reassign reply %v", r)
	}
	if e.ownerOf(t, e.family) != "contributor" || e.audits(t, "folder.reassign_archived_owner") != 1 {
		t.Fatal("owner or audit not written")
	}
	var members int
	if err := e.Meta.DB().QueryRow("SELECT count(*) FROM folder_members WHERE folder_id = ? AND user_id IN (?, ?)", e.family, e.ids["contributor"], e.ids["owner"]).Scan(&members); err != nil || members != 0 {
		t.Fatalf("memberships of new and previous owner: %d %v", members, err)
	}
	var actor, details string
	if err := e.Meta.DB().QueryRow("SELECT actor_user_id, details_json FROM audit_log WHERE action = 'folder.reassign_archived_owner'").Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	if actor != e.ids["root"] || !strings.Contains(details, e.ids["owner"]) || !strings.Contains(details, `"CONTRIBUTOR"`) {
		t.Fatalf("audit %s %s", actor, details)
	}
	for _, login := range []string{"contributor", "owner", "root", "viewer"} {
		wantError(t, login+" reads reassigned folder", e.as(t, login, "GET", "/api/v1/folders/"+e.family, nil, nil), 404, "folder_not_found")
		wantError(t, login+" syncs reassigned folder", e.as(t, login, "GET", "/api/v1/folders/"+e.family+"/sync", nil, nil), 404, "folder_not_found")
		list := wantJSON(t, "folders of "+login, e.as(t, login, "GET", "/api/v1/folders", nil, nil), 200)
		for _, f := range list["folders"].([]any) {
			if f.(map[string]any)["id"] == e.family {
				t.Fatalf("%s lists the archived folder", login)
			}
		}
	}
	wantError(t, "new owner cannot download", e.as(t, "contributor", "GET", "/api/v1/folders/"+e.family+"/assets/"+e.photo+"/original", nil, nil), 404, "folder_not_found")
	// Now the previous owner's account can be disabled.
	wantJSON(t, "disable previous owner", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["owner"], `{"status":"disabled"}`), 200)
}

// TestReassignRechecksCallerInTransaction: the store decides on the caller
// as stored inside the transaction, not on what the request carried.
func TestReassignRechecksCallerInTransaction(t *testing.T) {
	e := newACLEnv(t)
	if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family, nil, nil); r.Status != 204 {
		t.Fatalf("archive %d", r.Status)
	}
	errNo := errors.New("not an administrator")
	adminOnly := func(p authz.Principal, _ authz.AlbumAccess) error {
		if !authz.Allowed(p, authz.ManageAccounts, nil) {
			return errNo
		}
		return nil
	}
	_, err := e.Meta.ReassignArchivedOwner(t.Context(), meta.AlbumChange{FolderID: e.family, ActorID: e.ids["owner"], TargetID: e.ids["viewer"], Authorize: adminOnly})
	if !errors.Is(err, errNo) {
		t.Fatalf("USER caller: %v", err)
	}
	// A session that is not the caller's is an inactive principal.
	_, err = e.Meta.ReassignArchivedOwner(t.Context(), meta.AlbumChange{FolderID: e.family, ActorID: e.ids["root"], SessionID: "no-such-session", TargetID: e.ids["viewer"], Authorize: adminOnly})
	if !errors.Is(err, errNo) {
		t.Fatalf("revoked session: %v", err)
	}
	if e.ownerOf(t, e.family) != "owner" {
		t.Fatal("owner changed")
	}
}
