package httpapi_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/users"
)

// The exhaustive authorization matrix (M41): every route of the router's
// policy table x every kind of caller x every way a request can arrive.
//
// Each route must be classified below by hand. A route added to
// routePolicies without a class fails TestAuthzMatrixCoversEveryRoute, so
// no route can reach the internet (relay, funnel) or the API without an
// explicit, tested decision about who may call it.
//
// The probe of a route sends no body and well-formed but unknown IDs for
// everything except {folderID}, so that a caller who is let through reaches
// the handler and gets a handler answer (415, 400, 404 asset_not_found,
// ...) without changing what the next probe sees. What is asserted is the
// access decision: 401 (no valid session), 404 folder_not_found (folder
// hidden), 403 forbidden (role), 403 not_available_via_relay (remote
// route), or "reached the handler".

// matrixClass is who may call a route.
type matrixClass struct {
	pass   []string // callers that reach the handler
	remote bool     // also served over the relay and the funnel
	// deleted: the route is probed against a deleted folder of "owner"
	// (with the same members as the family folder) and every other
	// authenticated caller, members included, gets "hidden".
	deleted bool
}

var (
	everyone      = []string{"anonymous", "owner", "contributor", "viewer", "uploader", "outsider", "admin", "archived", "inactive", "revoked"}
	authenticated = []string{"owner", "contributor", "viewer", "uploader", "outsider", "admin", "archived"}
	classPublic   = matrixClass{pass: everyone, remote: true}
	classSession  = matrixClass{pass: authenticated, remote: true}
	classCatchAll = matrixClass{pass: authenticated, remote: false} // unknown /api/v1/ paths: 404 with a session, never remote
	classAdmin    = matrixClass{pass: []string{"admin"}, remote: false}
	classCreate   = matrixClass{pass: []string{"owner", "admin", "archived"}, remote: true} // can_create_albums or SERVER_ADMIN
	classMeta     = matrixClass{pass: []string{"owner", "contributor", "viewer", "uploader"}, remote: true}
	classRead     = matrixClass{pass: []string{"owner", "contributor", "viewer"}, remote: true}
	classUpload   = matrixClass{pass: []string{"owner", "contributor", "uploader"}, remote: true}
	classOwner    = matrixClass{pass: []string{"owner"}, remote: true}
	classManage   = matrixClass{pass: []string{"owner", "contributor"}, remote: true} // remove, restore, hide assets; the trash
	classPage     = matrixClass{pass: everyone, remote: false}                        // the dashboard page: static, never remote
	classRestore  = matrixClass{pass: []string{"owner"}, remote: true, deleted: true} // restore a deleted folder
)

// matrixClasses classifies every route of the policy table.
var matrixClasses = map[string]matrixClass{
	"GET /{$}":                              classPage,
	"GET /api/v1/health":                    classPublic,
	"POST /api/v1/auth/login":               classPublic,
	"POST /api/v1/auth/refresh":             classPublic,
	"POST /api/v1/auth/sign-in/nonce":       classPublic,
	"POST /api/v1/auth/sign-in":             classPublic,
	"/api/v1/":                              classCatchAll,
	"POST /api/v1/auth/logout":              classSession,
	"GET /api/v1/me":                        classSession,
	"PATCH /api/v1/me/settings":             classSession,
	"POST /api/v1/me/password":              classSession,
	"GET /api/v1/me/devices":                classSession,
	"GET /api/v1/me/sign-ins":               classSession,
	"POST /api/v1/me/sign-ins":              classSession,
	"DELETE /api/v1/me/sign-ins/{provider}": classSession,
	"GET /api/v1/me/activity":               classSession,
	"GET /api/v1/folders":                   classSession,
	"GET /api/v1/sync/state":                classSession,
	"GET /api/v1/folders/deleted":           classSession,
	"GET /api/v1/invitations":               classSession,
	"POST /api/v1/folders":                  classCreate,
	"GET /api/v1/admin/users":               classAdmin,
	"POST /api/v1/admin/users":              classAdmin,
	"GET /api/v1/admin/server/tls":          classAdmin,
	"GET /api/v1/admin/server/storage":      classAdmin,

	"DELETE /api/v1/me/devices/{deviceID}":            classSession,
	"POST /api/v1/me/devices/revoke-others":           classSession,
	"POST /api/v1/invitations/{invitationID}/accept":  classSession,
	"POST /api/v1/invitations/{invitationID}/decline": classSession,

	"GET /api/v1/admin/users/{userID}":                       classAdmin,
	"PATCH /api/v1/admin/users/{userID}":                     classAdmin,
	"PATCH /api/v1/admin/users/{userID}/permissions":         classAdmin,
	"GET /api/v1/admin/users/{userID}/devices":               classAdmin,
	"DELETE /api/v1/admin/users/{userID}/devices/{deviceID}": classAdmin,
	"GET /api/v1/admin/users/{userID}/folders":               classAdmin,
	"POST /api/v1/admin/folders/{folderID}/owner":            classAdmin,

	"GET /api/v1/folders/{folderID}":                                          classMeta,
	"PATCH /api/v1/folders/{folderID}":                                        classOwner,
	"DELETE /api/v1/folders/{folderID}":                                       classOwner,
	"POST /api/v1/folders/{folderID}/archive":                                 classOwner,
	"POST /api/v1/folders/{folderID}/restore":                                 classRestore,
	"POST /api/v1/folders/{folderID}/assets":                                  classUpload,
	"POST /api/v1/folders/{folderID}/uploads":                                 classUpload,
	"GET /api/v1/folders/{folderID}/uploads/{uploadID}":                       classUpload,
	"PATCH /api/v1/folders/{folderID}/uploads/{uploadID}":                     classUpload,
	"DELETE /api/v1/folders/{folderID}/uploads/{uploadID}":                    classUpload,
	"POST /api/v1/folders/{folderID}/uploads/{uploadID}/complete":             classUpload,
	"POST /api/v1/folders/{folderID}/bundles":                                 classUpload,
	"GET /api/v1/folders/{folderID}/bundles/{bundleID}":                       classUpload,
	"DELETE /api/v1/folders/{folderID}/bundles/{bundleID}":                    classUpload,
	"POST /api/v1/folders/{folderID}/bundles/{bundleID}/complete":             classUpload,
	"GET /api/v1/folders/{folderID}/assets":                                   classRead,
	"POST /api/v1/folders/{folderID}/assets/check":                            classRead,
	"DELETE /api/v1/folders/{folderID}/assets/{assetID}":                      classManage,
	"PATCH /api/v1/folders/{folderID}/assets/{assetID}":                       classManage,
	"GET /api/v1/folders/{folderID}/trash":                                    classManage,
	"GET /api/v1/folders/{folderID}/trash/{assetID}/preview":                  classManage,
	"POST /api/v1/folders/{folderID}/trash/{assetID}/restore":                 classManage,
	"GET /api/v1/folders/{folderID}/sync":                                     classRead,
	"PUT /api/v1/folders/{folderID}/sync/cursor":                              classRead,
	"GET /api/v1/folders/{folderID}/assets/{assetID}/original":                classRead,
	"GET /api/v1/folders/{folderID}/assets/{assetID}/preview":                 classRead,
	"GET /api/v1/folders/{folderID}/assets/{assetID}/components/{role}":       classRead,
	"GET /api/v1/folders/{folderID}/assets/{assetID}/playback":                classRead,
	"GET /api/v1/folders/{folderID}/assets/{assetID}/likes":                   classRead,
	"PUT /api/v1/folders/{folderID}/assets/{assetID}/likes/me":                classRead,
	"DELETE /api/v1/folders/{folderID}/assets/{assetID}/likes/me":             classRead,
	"GET /api/v1/folders/{folderID}/assets/{assetID}/comments":                classRead,
	"POST /api/v1/folders/{folderID}/assets/{assetID}/comments":               classRead,
	"PATCH /api/v1/folders/{folderID}/assets/{assetID}/comments/{commentID}":  classRead,
	"DELETE /api/v1/folders/{folderID}/assets/{assetID}/comments/{commentID}": classRead,
	"GET /api/v1/folders/{folderID}/mentionable":                              classRead,
	"GET /api/v1/folders/{folderID}/members":                                  classRead,
	"PATCH /api/v1/folders/{folderID}/members/{userID}":                       classOwner,
	"DELETE /api/v1/folders/{folderID}/members/{userID}":                      classOwner,
	"POST /api/v1/folders/{folderID}/owner":                                   classOwner,
	"GET /api/v1/folders/{folderID}/invitations":                              classOwner,
	"POST /api/v1/folders/{folderID}/invitations":                             classOwner,
	"DELETE /api/v1/folders/{folderID}/invitations/{invitationID}":            classOwner,
}

func TestAuthzMatrixCoversEveryRoute(t *testing.T) {
	declared := map[string]bool{}
	for _, p := range httpapi.Routes() {
		declared[p.Pattern] = true
		if _, ok := matrixClasses[p.Pattern]; !ok {
			t.Errorf("route %q has no class in matrixClasses: decide who may call it (and whether it is served remotely) and add it", p.Pattern)
		}
	}
	for p := range matrixClasses {
		if !declared[p] {
			t.Errorf("matrixClasses lists %q, which is not a declared route", p)
		}
	}
}

// matrixEnv is aclEnv plus an archived folder owned by "owner", a deleted
// folder owned by "owner" with the family folder's members, an account
// that was a VIEWER and is now disabled ("inactive") and a VIEWER whose
// device was revoked ("revoked").
type matrixEnv struct {
	*aclEnv
	archived string
	deleted  string
	logins   int
}

func randomHexID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// login logs login in from a fresh client address (the login rate limit
// is per address) and returns the access token and device ID.
func (e *matrixEnv) login(t *testing.T, login, pw string) (string, string) {
	t.Helper()
	e.logins++
	body := fmt.Sprintf(`{"login_name":%q,"password":%q,"device_name":"Matrix","platform":"ios"}`, login, pw)
	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = fmt.Sprintf("10.41.%d.%d:4100", e.logins/250, e.logins%250+1)
	w := httptest.NewRecorder()
	e.Raw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", login, w.Code, w.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m["access_token"].(string), m["device_id"].(string)
}

func newMatrixEnv(t *testing.T) *matrixEnv {
	t.Helper()
	e := &matrixEnv{aclEnv: newACLEnv(t)}
	e.archived = wantJSON(t, "create archived", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"archived"}`), nil), 201)["id"].(string)
	if r := e.as(t, "owner", "POST", "/api/v1/folders/"+e.archived+"/archive", nil, nil); r.Status != 204 {
		t.Fatalf("archive: %d %s", r.Status, r.Body)
	}
	us := users.New(e.Meta.DB())
	hash, _ := password.Hash(userPassword, fastParams)
	for _, login := range []string{"inactive", "revoked"} {
		u, err := us.Create(t.Context(), users.NewUser{LoginName: login, DisplayName: login, PasswordHash: hash, Role: users.RoleUser}, users.Console)
		if err != nil {
			t.Fatal(err)
		}
		e.ids[login] = u.ID
		e.setMember(t, login, "VIEWER")
		var device string
		e.tokens[login], device = e.login(t, login, userPassword)
		if login == "inactive" {
			if err := us.SetStatus(t.Context(), u.ID, users.StatusDisabled, users.Console); err != nil {
				t.Fatal(err)
			}
		} else if r := e.as(t, login, "DELETE", "/api/v1/me/devices/"+device, nil, nil); r.Status != 204 {
			t.Fatalf("revoke device: %d %s", r.Status, r.Body)
		}
	}
	e.tokens["admin"] = e.tokens["root"]
	e.deleted = wantJSON(t, "create deleted", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"deleted"}`), nil), 201)["id"].(string)
	if _, err := e.Meta.DB().Exec(`INSERT INTO folder_members (folder_id, user_id, role, joined_at)
		SELECT ?, user_id, role, joined_at FROM folder_members WHERE folder_id = ?`, e.deleted, e.family); err != nil {
		t.Fatal(err)
	}
	if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.deleted, nil, nil); r.Status != 204 {
		t.Fatalf("delete: %d %s", r.Status, r.Body)
	}
	return e
}

// probePath fills in a route pattern: the folder the caller is probed
// against, and fresh unknown IDs for everything else.
func probePath(pattern, folder string) (method, path string) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = "GET", pattern+"matrix-unknown-path"
	}
	path = strings.ReplaceAll(path, "{$}", "")
	path = strings.ReplaceAll(path, "{folderID}", folder)
	path = strings.ReplaceAll(path, "{role}", "paired_video")
	for _, p := range []string{"{assetID}", "{uploadID}", "{bundleID}", "{commentID}", "{userID}", "{deviceID}", "{invitationID}"} {
		path = strings.ReplaceAll(path, p, randomHexID())
	}
	return method, path
}

// outcome names the access decision of a reply.
func outcome(status int, body []byte) string {
	var m struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &m)
	switch {
	case status == 401:
		return "401"
	case status == 404 && m.Error == "folder_not_found":
		return "hidden"
	case status == 403 && (m.Error == "forbidden" || m.Error == "not_available_via_relay" || m.Error == "csrf_failed"):
		return "403 " + m.Error
	}
	return "reached"
}

// expected is the outcome the matrix requires for caller on a route of
// class c reached over route.
func expected(c matrixClass, album bool, caller string, route httpapi.Route) string {
	if route.Remote() && !c.remote {
		return "403 not_available_via_relay"
	}
	for _, p := range c.pass {
		if p == caller {
			return "reached"
		}
	}
	switch {
	case caller == "anonymous" || caller == "inactive" || caller == "revoked":
		return "401"
	case album && (c.deleted || caller == "outsider" || caller == "admin" || caller == "archived"):
		return "hidden"
	}
	return "403 forbidden"
}

// TestAuthzMatrix runs every route for every caller over every route.
func TestAuthzMatrix(t *testing.T) {
	e := newMatrixEnv(t)
	handlers := map[httpapi.Route]http.Handler{
		httpapi.RouteDirect: e.Raw,
		httpapi.RouteRelay:  httpapi.RemoteOnly(httpapi.RouteRelay, e.Raw),
		httpapi.RouteFunnel: httpapi.RemoteOnly(httpapi.RouteFunnel, e.Raw),
	}
	routes := httpapi.Routes()
	// Logout ends the probing session: probe it last and log back in.
	for i, p := range routes {
		if p.Pattern == "POST /api/v1/auth/logout" {
			routes = append(append(routes[:i:i], routes[i+1:]...), p)
			break
		}
	}
	n := 0
	for _, rp := range routes {
		c, ok := matrixClasses[rp.Pattern]
		if !ok {
			continue // TestAuthzMatrixCoversEveryRoute reports it
		}
		album := rp.Access == httpapi.AccessAlbum
		for _, route := range []httpapi.Route{httpapi.RouteDirect, httpapi.RouteRelay, httpapi.RouteFunnel} {
			for _, caller := range everyone {
				folder, login := e.family, caller
				if c.deleted {
					folder = e.deleted
				}
				if caller == "archived" {
					folder, login = e.archived, "owner"
				}
				method, path := probePath(rp.Pattern, folder)
				req := httptest.NewRequest(method, path, bytes.NewReader(nil))
				req.RemoteAddr = "198.51.100.41:4100"
				if caller != "anonymous" {
					req.Header.Set("Authorization", "Bearer "+e.tokens[login])
				}
				w := httptest.NewRecorder()
				handlers[route].ServeHTTP(w, req)
				n++
				got, want := outcome(w.Code, w.Body.Bytes()), expected(c, album, caller, route)
				if got != want {
					t.Errorf("%s via %s as %s (%s %s): %s, want %s (status %d %s)", rp.Pattern, route, caller, method, path, got, want, w.Code, bytes.TrimSpace(w.Body.Bytes()))
				}
				if got != "reached" {
					continue
				}
				// Undo what a probe that reached its handler changed.
				switch rp.Pattern {
				case "DELETE /api/v1/folders/{folderID}":
					if _, err := e.Meta.DB().Exec("UPDATE folders SET deleted_at = NULL WHERE folder_id = ?", e.family); err != nil {
						t.Fatal(err)
					}
				case "POST /api/v1/folders/{folderID}/archive":
					if _, err := e.Meta.DB().Exec("UPDATE folders SET archived_at = NULL WHERE folder_id = ?", e.family); err != nil {
						t.Fatal(err)
					}
				case "POST /api/v1/folders/{folderID}/restore":
					if _, err := e.Meta.DB().Exec("UPDATE folders SET deleted_at = ? WHERE folder_id = ?", time.Now().UTC().Format(time.RFC3339Nano), e.deleted); err != nil {
						t.Fatal(err)
					}
				case "POST /api/v1/auth/logout":
					pw := userPassword
					if login == "admin" || login == "root" {
						pw = adminPassword
						login = "root"
					}
					tok, _ := e.login(t, login, pw)
					e.tokens[login] = tok
					if login == "root" {
						e.tokens["admin"] = tok
					}
				}
			}
		}
	}
	if want := len(matrixClasses) * 3 * len(everyone); n != want {
		t.Fatalf("%d probes, want %d", n, want)
	}
}

// TestAuthzMatrixCrossFolderIDs: an ID of another folder in the path of a
// folder the caller can use is unknown there (IDOR), for every route that
// takes an asset ID.
func TestAuthzMatrixCrossFolderIDs(t *testing.T) {
	e := newMatrixEnv(t)
	for _, rp := range httpapi.Routes() {
		if !strings.Contains(rp.Pattern, "{assetID}") {
			continue
		}
		method, path := probePath(rp.Pattern, e.family)
		// The admin's asset in its own folder, asked for through family.
		parts := strings.Split(path, "/")
		for i, s := range parts {
			if i > 0 && parts[i-1] == "assets" && len(s) == 32 {
				parts[i] = e.other
			}
		}
		path = strings.Join(parts, "/")
		var body []byte
		hdr := map[string]string{}
		if method != "GET" && method != "DELETE" {
			body, hdr["Content-Type"] = []byte(`{"body":"x","mentions":[],"hidden":true}`), "application/json"
		}
		r := e.as(t, "owner", method, path, body, hdr)
		var m struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(r.Body, &m)
		if r.Status != 404 || m.Error != "asset_not_found" && m.Error != "comment_not_found" {
			t.Errorf("%s with another folder's asset: %d %s", rp.Pattern, r.Status, r.Body)
		}
	}
}

// TestRemoteLoginFailuresDoNotLockHomeLogin: someone on the internet who
// knows a login name and fails its password over the funnel (from many
// addresses, or all sharing one unknown address) gets the account
// throttled there, but its owner can still log in at home (and pair a
// phone) with the right password.
func TestRemoteLoginFailuresDoNotLockHomeLogin(t *testing.T) {
	e := newMatrixEnv(t)
	funnel := httpapi.RemoteOnly(httpapi.RouteFunnel, e.Raw)
	post := func(h http.Handler, addr, pw string) int {
		body := fmt.Sprintf(`{"login_name":"viewer","password":%q,"device_name":"x","platform":"ios"}`, pw)
		req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	// Default limits: 20 failures per account (any address) in 15 minutes.
	for i := range 25 {
		post(funnel, fmt.Sprintf("203.0.113.%d:443", i+1), "wrong password "+fmt.Sprint(i))
	}
	if got := post(funnel, "198.51.100.200:443", userPassword); got != 429 {
		t.Fatalf("funnel login after the failures: %d, want 429", got)
	}
	if got := post(e.Raw, "192.168.1.30:50000", userPassword); got != 200 {
		t.Fatalf("home login after remote failures: %d, want 200", got)
	}
}

// TestNamesOthersSeeAreSingleLinePlainText: what one member writes and
// others see (album name, uploader device label, comment) cannot carry
// control characters or bidirectional overrides that would make it display
// as something else (M41).
func TestNamesOthersSeeAreSingleLinePlainText(t *testing.T) {
	e := newACLEnv(t)
	rlo := "\u202e"
	wantError(t, "album name", e.asJSON(t, "owner", "POST", "/api/v1/folders", `{"name":"gpj.`+rlo+`exe"}`), 400, "invalid_folder_name")
	wantError(t, "album name", e.asJSON(t, "owner", "POST", "/api/v1/folders", `{"name":"a\u0007b"}`), 400, "invalid_folder_name")
	wantError(t, "device label", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/assets", []byte("bytes"),
		map[string]string{"X-Device-ID": "Mom" + rlo + "s phone"}), 400, "invalid_device_id")
	wantError(t, "comment", e.asJSON(t, "viewer", "POST", "/api/v1/folders/"+e.family+"/assets/"+e.photo+"/comments",
		`{"body":"look `+rlo+`here","mentions":[]}`), 400, "invalid_comment")
	// Right-to-left text with its plain marks is fine.
	wantJSON(t, "rtl album", e.asJSON(t, "owner", "POST", "/api/v1/folders", `{"name":"عائلة\u200f"}`), 201)
}
