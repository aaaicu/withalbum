package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/authz"
)

func wantPanic(t *testing.T, what, contains string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("%s: no panic", what)
		}
		if msg, _ := r.(string); !strings.Contains(msg, contains) {
			t.Fatalf("%s: panic %v, want it to mention %q", what, r, contains)
		}
	}()
	fn()
}

// TestRouterRefusesUndeclaredRoutes: a handler can only be registered under
// a declared policy of the matching kind, once, and every declared route
// must get a handler.
func TestRouterRefusesUndeclaredRoutes(t *testing.T) {
	nop := func(http.ResponseWriter, *http.Request) {}
	nopAlbum := func(http.ResponseWriter, *http.Request, albumRequest) {}
	wantPanic(t, "undeclared", "without a declared access policy", func() { newRouter(&Server{}).handle("GET /api/v1/secret", nop) })
	wantPanic(t, "album route as plain", "is declared album", func() {
		newRouter(&Server{}).handle("GET /api/v1/folders/{folderID}/sync", nop)
	})
	wantPanic(t, "plain route as album", "is declared session", func() { newRouter(&Server{}).handleAlbum("GET /api/v1/folders", nopAlbum) })
	wantPanic(t, "twice", "registered twice", func() {
		rt := newRouter(&Server{})
		rt.handle("GET /api/v1/folders", nop)
		rt.handle("GET /api/v1/folders", nop)
	})
	wantPanic(t, "missing handler", "has no handler", func() {
		rt := newRouter(&Server{})
		rt.handle("GET /api/v1/folders", nop)
		rt.finish()
	})
}

// TestRoutePolicyTable pins the protection of the album routes and that
// only the four expected routes are public. The only folder route that is
// not an album route is the administrator's reassignment of archived
// folders, which gives no access to the folder (admin.go).
func TestRoutePolicyTable(t *testing.T) {
	var public []string
	for _, p := range routePolicies {
		if p.Access == AccessPublic {
			public = append(public, p.Pattern)
		}
		if p.Pattern == "POST /api/v1/admin/folders/{folderID}/owner" {
			if p.Access != AccessServer || p.Action != authz.ManageAccounts {
				t.Errorf("%s is %s %s, want server %s", p.Pattern, p.Access, p.Action, authz.ManageAccounts)
			}
			continue
		}
		if strings.Contains(p.Pattern, "{folderID}") && p.Access != AccessAlbum {
			t.Errorf("%s is %s, want album", p.Pattern, p.Access)
		}
	}
	if got := strings.Join(public, ", "); got != "GET /{$}, GET /api/v1/health, POST /api/v1/auth/login, POST /api/v1/auth/refresh" {
		t.Fatalf("public routes: %s", got)
	}
}
