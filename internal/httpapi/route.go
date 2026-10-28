package httpapi

import (
	"context"
	"io"
	"net/http"
	"path"
	"strings"
)

// Route is how a request reached the server. The listener that accepted
// the connection decides it (server/main.go, relay.go, funnel.go); nothing
// a client sends can change it.
type Route string

const (
	// RouteDirect: the main listener (this Mac's loopback, or the LAN with
	// -allow-lan). The zero value of a request without a route.
	RouteDirect Route = "direct"
	// RouteRelay: through the end-to-end encrypted relay (RELAY_DESIGN.md).
	RouteRelay Route = "relay"
	// RouteFunnel: through a Tailscale Funnel TCP forwarder to the
	// loopback funnel listener (docs/FUNNEL.md).
	RouteFunnel Route = "funnel"
)

type routeKey struct{}

// WithRoute marks the requests of next as having arrived over route.
// Wrap the handler of a listener with it, outermost.
func WithRoute(route Route, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
	})
}

// RouteFrom is the route of a request (RouteDirect when none was set).
func RouteFrom(ctx context.Context) Route {
	if r, ok := ctx.Value(routeKey{}).(Route); ok && r != "" {
		return r
	}
	return RouteDirect
}

// Remote reports whether the route comes from outside the home network
// (relay or funnel): only app routes are served there.
func (r Route) Remote() bool { return r == RouteRelay || r == RouteFunnel }

// PublicRoute is a public address that forwards the pinned TLS connection
// unchanged to this server (docs/FUNNEL.md). nil: none.
type PublicRoute struct {
	URL   string // https origin, e.g. https://home.example-tailnet.ts.net
	Route Route  // RouteFunnel
}

// SetPublicRoute records the public route. Call it before Handler.
func (s *Server) SetPublicRoute(p *PublicRoute) { s.public = p }

// publicJSON is the "public" object of GET /me and the admin TLS view.
func (s *Server) publicJSON() map[string]any {
	return map[string]any{"url": s.public.URL, "route": string(s.public.Route)}
}

// RemoteAllowed lists the paths served on a remote route (relay, funnel):
// what an app uses (login, its own account and devices, albums,
// invitations, health). Every other path, in particular the dashboard page
// and /api/v1/admin/, is not. A path that is not in canonical form is
// refused rather than interpreted.
func RemoteAllowed(p string) bool {
	if p == "" || path.Clean(p) != p {
		return false
	}
	under := func(prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	return p == "/api/v1/health" || under("/api/v1/auth") || under("/api/v1/me") || under("/api/v1/folders") || under("/api/v1/invitations")
}

// RemoteOnly wraps the API handler of a listener for connections from
// outside the home network (relay, funnel): it marks the requests with
// route and serves only the app routes of RemoteAllowed. The error code is
// not_available_via_relay for both routes, so apps handle one code.
func RemoteOnly(route Route, next http.Handler) http.Handler {
	return WithRoute(route, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !RemoteAllowed(r.URL.Path) {
			h := w.Header()
			h.Set("Content-Type", "application/json; charset=utf-8")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"not_available_via_relay","route":"`+string(route)+`"}`+"\n")
			return
		}
		next.ServeHTTP(w, r)
	}))
}
