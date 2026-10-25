package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/password"
	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/users"
)

// Auth, own-account and account administration routes (docs/API_v2.yaml).
// Their access policies are declared in middleware_authz.go.

const maxAuthBody = 8 << 10

// jsonBodyTimeout bounds the time to receive a JSON request body.
const jsonBodyTimeout = 30 * time.Second

func (s *Server) authRoutes(rt *router) {
	rt.handle("POST /api/v1/auth/login", s.login)
	rt.handle("POST /api/v1/auth/refresh", s.refresh)
	rt.handle("POST /api/v1/auth/logout", s.logout)
	rt.handle("GET /api/v1/me", s.me)
	rt.handle("POST /api/v1/me/password", s.changePassword)
	rt.handle("GET /api/v1/me/devices", s.listDevices)
	rt.handle("DELETE /api/v1/me/devices/{deviceID}", s.revokeDevice)
	rt.handle("POST /api/v1/me/devices/revoke-others", s.revokeOtherDevices)
	rt.handle("GET /api/v1/admin/users", s.adminListUsers)
	rt.handle("POST /api/v1/admin/users", s.adminCreateUser)
	rt.handle("GET /api/v1/admin/users/{userID}", s.adminGetUser)
	rt.handle("PATCH /api/v1/admin/users/{userID}", s.adminSetStatus)
	rt.handle("PATCH /api/v1/admin/users/{userID}/permissions", s.adminSetPermissions)
	rt.handle("GET /api/v1/admin/users/{userID}/devices", s.adminListDevices)
	rt.handle("DELETE /api/v1/admin/users/{userID}/devices/{deviceID}", s.adminRevokeDevice)
}

// decodeJSON reads a JSON object body. It requires Content-Type
// application/json, which a cross-site HTML form cannot send without a CORS
// preflight (the server answers none), and bounds the body size and the
// time to send it: a login body arrives from anyone who can reach the
// server (also over the relay or the funnel), and must not hold a
// connection open by trickling bytes.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		errorReply(w, 415, "json_required")
		return false
	}
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Now().Add(jsonBodyTimeout)) == nil {
		defer rc.SetReadDeadline(time.Time{})
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAuthBody))
	if err := dec.Decode(v); err != nil {
		errorReply(w, 400, "invalid_json")
		return false
	}
	return true
}

type userJSON struct {
	UserID          string `json:"user_id"`
	LoginName       string `json:"login_name"`
	DisplayName     string `json:"display_name"`
	ServerRole      string `json:"server_role"`
	CanCreateAlbums bool   `json:"can_create_albums"`
}

func identityUser(id session.Identity) userJSON {
	return userJSON{UserID: id.UserID, LoginName: id.LoginName, DisplayName: id.DisplayName,
		ServerRole: id.ServerRole, CanCreateAlbums: id.CanCreateAlbums}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func setCookie(w http.ResponseWriter, r *http.Request, name, value, path string, maxAge time.Duration, httpOnly bool) {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: httpOnly, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: int(maxAge / time.Second)}
	if maxAge < 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func clearCookies(w http.ResponseWriter, r *http.Request) {
	setCookie(w, r, cookieAccess, "", "/api/", -1, true)
	setCookie(w, r, cookieRefresh, "", "/api/v1/auth/", -1, true)
	setCookie(w, r, cookieCSRF, "", "/", -1, false)
}

// tokenReply answers a login or refresh. Apps get the tokens in the body;
// browsers get cookies and only the CSRF token in the body.
func (s *Server) tokenReply(w http.ResponseWriter, r *http.Request, t session.Tokens, id session.Identity) {
	body := map[string]any{"device_id": t.DeviceID, "access_expires_at": ts(t.AccessExpires),
		"refresh_expires_at": ts(t.RefreshExpires), "session_expires_at": ts(t.SessionExpires), "user": identityUser(id)}
	if t.CSRFToken != "" {
		now := time.Now()
		setCookie(w, r, cookieAccess, t.AccessToken, "/api/", t.AccessExpires.Sub(now), true)
		setCookie(w, r, cookieRefresh, t.RefreshToken, "/api/v1/auth/", t.RefreshExpires.Sub(now), true)
		setCookie(w, r, cookieCSRF, t.CSRFToken, "/", t.RefreshExpires.Sub(now), false)
		body["token_type"] = "cookie"
		body["csrf_token"] = t.CSRFToken
	} else {
		body["token_type"] = "Bearer"
		body["access_token"] = t.AccessToken
		body["refresh_token"] = t.RefreshToken
	}
	jsonReply(w, 200, body)
}

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	errorReply(w, 503, "busy")
}

func rateLimited(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
	errorReply(w, 429, "rate_limited")
}

// login: every credential failure (unknown account, wrong password,
// disabled account, account without a usable password) is the same 401
// invalid_credentials after the same Argon2id work.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var req struct {
		LoginName  string `json:"login_name"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LoginName == "" || req.Password == "" {
		errorReply(w, 400, "invalid_request")
		return
	}
	if err := session.ValidateDevice(req.DeviceName, req.Platform); err != nil {
		errorReply(w, 400, "invalid_device")
		return
	}
	c := client(r)
	if ok, wait := s.auth.Limiter.Allow(rateKey(c.IP), req.LoginName); !ok {
		rateLimited(w, wait)
		return
	}
	t, id, err := s.auth.Sessions.Login(r.Context(), session.LoginRequest{LoginName: req.LoginName, Password: req.Password,
		DeviceName: req.DeviceName, Platform: req.Platform, Client: c})
	switch {
	case err == nil:
		s.auth.Limiter.Success(rateKey(c.IP), req.LoginName)
		s.tokenReply(w, r, t, id)
	case errors.Is(err, session.ErrInvalidCredentials):
		if s.auth.Limiter.Failure(rateKey(c.IP), req.LoginName) {
			if err := s.auth.Sessions.AuditThrottled(r.Context(), req.LoginName, c); err != nil {
				internalError(w, "audit login throttle", err)
				return
			}
		}
		errorReply(w, 401, "invalid_credentials")
	case errors.Is(err, password.ErrBusy):
		busy(w)
	default:
		internalError(w, "login", err)
	}
}

// refresh takes the refresh token from the body ({"refresh_token": ...}) or,
// for browsers, from the refresh cookie together with the CSRF header.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	rr := session.RefreshRequest{RefreshToken: req.RefreshToken, Client: client(r)}
	if rr.RefreshToken == "" {
		if c, err := r.Cookie(cookieRefresh); err == nil {
			rr.RefreshToken, rr.RequireCSRF, rr.CSRFToken = c.Value, true, r.Header.Get(headerCSRF)
		}
	}
	if rr.RefreshToken == "" {
		unauthorized(w)
		return
	}
	t, id, err := s.auth.Sessions.Refresh(r.Context(), rr)
	switch {
	case err == nil:
		s.tokenReply(w, r, t, id)
	case errors.Is(err, session.ErrCSRF):
		errorReply(w, 403, "csrf_failed")
	case errors.Is(err, session.ErrUnauthenticated), errors.Is(err, session.ErrRefreshReuse):
		if rr.RequireCSRF {
			clearCookies(w, r)
		}
		unauthorized(w)
	default:
		internalError(w, "refresh", err)
	}
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	if err := s.auth.Sessions.Logout(r.Context(), id, client(r)); err != nil && !errors.Is(err, session.ErrDeviceNotFound) {
		internalError(w, "logout", err)
		return
	}
	if id.IsBrowser() {
		clearCookies(w, r)
	}
	w.WriteHeader(204)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	body := map[string]any{"user": identityUser(id), "device_id": id.DeviceID, "platform": id.Platform,
		"server_features": ServerFeatures}
	if s.relay != nil {
		// Lets an app paired before the relay was enabled learn it over
		// its pinned connection (docs/RELAY_DESIGN.md).
		body["relay"] = s.relayJSON(false)
	}
	if s.public != nil {
		// The same for a public address (Tailscale Funnel, docs/FUNNEL.md).
		body["public"] = s.publicJSON()
	}
	// How this request arrived: "direct", "relay" or "funnel".
	body["connection_route"] = string(RouteFrom(r.Context()))
	jsonReply(w, 200, body)
}

// changePassword verifies the current password (rate-limited like a login)
// and revokes every other session of the account.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := password.CheckPolicy(req.NewPassword); err != nil {
		jsonReply(w, 400, map[string]string{"error": "invalid_password", "message": err.Error()})
		return
	}
	c := client(r)
	if ok, wait := s.auth.Limiter.Allow(rateKey(c.IP), id.LoginName); !ok {
		rateLimited(w, wait)
		return
	}
	err := s.auth.Sessions.VerifyPassword(r.Context(), id.UserID, req.CurrentPassword)
	if errors.Is(err, session.ErrInvalidCredentials) {
		s.auth.Limiter.Failure(rateKey(c.IP), id.LoginName)
		if err := s.auth.Sessions.AuditPasswordChangeFailure(r.Context(), id.UserID, c); err != nil {
			internalError(w, "audit password change", err)
			return
		}
		errorReply(w, 403, "invalid_credentials")
		return
	}
	if errors.Is(err, password.ErrBusy) {
		busy(w)
		return
	}
	if err != nil {
		internalError(w, "verify password", err)
		return
	}
	s.auth.Limiter.Success(rateKey(c.IP), id.LoginName)
	h, err := s.auth.Passwords.Hash(r.Context(), req.NewPassword)
	if errors.Is(err, password.ErrBusy) {
		busy(w)
		return
	}
	if err != nil {
		internalError(w, "hash password", err)
		return
	}
	if err := s.auth.Users.ChangeOwnPassword(r.Context(), id.UserID, h, id.SessionID, users.Actor{UserID: id.UserID, Via: "http"}); err != nil {
		s.usersError(w, "change password", err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	s.devicesReply(w, r, id.UserID)
}

// devicesReply lists userID's devices with a live session; "current" marks
// the device of the request.
func (s *Server) devicesReply(w http.ResponseWriter, r *http.Request, userID string) {
	id, _ := IdentityFrom(r.Context())
	list, err := s.auth.Sessions.ListDevices(r.Context(), userID)
	if err != nil {
		internalError(w, "list devices", err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		out = append(out, map[string]any{"device_id": d.DeviceID, "device_name": d.DeviceName, "platform": d.Platform,
			"created_at": ts(d.CreatedAt), "last_used_at": ts(d.LastUsedAt), "expires_at": ts(d.Expires), "current": d.DeviceID == id.DeviceID})
	}
	jsonReply(w, 200, map[string]any{"devices": out})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	deviceID := r.PathValue("deviceID")
	err := s.auth.Sessions.RevokeDevice(r.Context(), id.UserID, deviceID, client(r))
	if errors.Is(err, session.ErrDeviceNotFound) {
		errorReply(w, 404, "device_not_found")
		return
	}
	if err != nil {
		internalError(w, "revoke device", err)
		return
	}
	if deviceID == id.DeviceID && id.IsBrowser() {
		clearCookies(w, r)
	}
	w.WriteHeader(204)
}

// revokeOtherDevices signs out every other device of the caller's account
// (a lost phone): their refresh and access tokens stop working at once.
// The caller's own session stays. Served through the relay and the funnel
// too, so a family member can do it from a phone away from home.
func (s *Server) revokeOtherDevices(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	n, err := s.auth.Sessions.RevokeOtherDevices(r.Context(), id, client(r))
	if err != nil {
		internalError(w, "revoke other devices", err)
		return
	}
	jsonReply(w, 200, map[string]any{"revoked": n})
}

// adminListDevices: another account's devices with a live session (names,
// platforms and times only), so that an administrator can sign out a lost
// device.
func (s *Server) adminListDevices(w http.ResponseWriter, r *http.Request) {
	if _, err := s.auth.Users.Get(r.Context(), r.PathValue("userID")); err != nil {
		s.usersError(w, "get user", err)
		return
	}
	s.devicesReply(w, r, r.PathValue("userID"))
}

// adminRevokeDevice revokes a device of another account (or the
// administrator's own); its tokens are refused from the next request on.
func (s *Server) adminRevokeDevice(w http.ResponseWriter, r *http.Request) {
	userID, deviceID := r.PathValue("userID"), r.PathValue("deviceID")
	if _, err := s.auth.Users.Get(r.Context(), userID); err != nil {
		s.usersError(w, "get user", err)
		return
	}
	id, _ := IdentityFrom(r.Context())
	err := s.auth.Sessions.RevokeDeviceAsAdmin(r.Context(), id.UserID, userID, deviceID, client(r))
	if errors.Is(err, session.ErrDeviceNotFound) {
		errorReply(w, 404, "device_not_found")
		return
	}
	if err != nil {
		internalError(w, "revoke device", err)
		return
	}
	if deviceID == id.DeviceID && id.IsBrowser() {
		clearCookies(w, r)
	}
	w.WriteHeader(204)
}

// Account administration (SERVER_ADMIN only). Administrators manage
// accounts; they gain no access to anyone's albums through these routes.

type adminUserJSON struct {
	userJSON
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	PasswordLocked bool   `json:"password_locked"`
}

func adminUser(u users.User) adminUserJSON {
	return adminUserJSON{userJSON: userJSON{UserID: u.ID, LoginName: u.LoginName, DisplayName: u.DisplayName,
		ServerRole: string(u.Role), CanCreateAlbums: u.CanCreateAlbums}, Status: string(u.Status), CreatedAt: u.CreatedAt,
		PasswordLocked: u.PasswordLocked}
}

func adminActor(r *http.Request) users.Actor {
	id, _ := IdentityFrom(r.Context())
	return users.Actor{UserID: id.UserID, Via: "http"}
}

// usersError maps internal/users errors to responses.
func (s *Server) usersError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, users.ErrNotFound):
		errorReply(w, 404, "user_not_found")
	case errors.Is(err, users.ErrLoginTaken):
		errorReply(w, 409, "login_taken")
	case errors.Is(err, users.ErrLastAdmin):
		errorReply(w, 409, "last_admin")
	case errors.Is(err, users.ErrOwnsAlbums):
		errorReply(w, 409, "owns_albums")
	case errors.Is(err, users.ErrBootstrapUnclaimed), errors.Is(err, users.ErrNotInitialized):
		errorReply(w, 409, "not_initialized")
	case errors.Is(err, users.ErrInvalidLoginName):
		errorReply(w, 400, "invalid_login_name")
	case errors.Is(err, users.ErrInvalidDisplayName):
		errorReply(w, 400, "invalid_display_name")
	case errors.Is(err, users.ErrInvalidRole):
		errorReply(w, 400, "invalid_role")
	case errors.Is(err, users.ErrInvalidStatus):
		errorReply(w, 400, "invalid_status")
	default:
		internalError(w, what, err)
	}
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.auth.Users.List(r.Context())
	if err != nil {
		internalError(w, "list users", err)
		return
	}
	out := make([]adminUserJSON, 0, len(list))
	for _, u := range list {
		out = append(out, adminUser(u))
	}
	jsonReply(w, 200, map[string]any{"users": out})
}

func (s *Server) adminGetUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.Users.Get(r.Context(), r.PathValue("userID"))
	if err != nil {
		s.usersError(w, "get user", err)
		return
	}
	jsonReply(w, 200, adminUser(u))
}

// adminCreateUser creates an active account with an initial password chosen
// by the administrator (to be handed over out of band).
func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoginName       string `json:"login_name"`
		DisplayName     string `json:"display_name"`
		Password        string `json:"password"`
		ServerRole      string `json:"server_role"`
		CanCreateAlbums bool   `json:"can_create_albums"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.LoginName
	}
	if req.ServerRole == "" {
		req.ServerRole = string(users.RoleUser)
	}
	n := users.NewUser{LoginName: req.LoginName, DisplayName: req.DisplayName, Role: users.Role(req.ServerRole), CanCreateAlbums: req.CanCreateAlbums}
	// Validate everything cheap before hashing.
	if err := users.ValidateLoginName(n.LoginName); err != nil {
		s.usersError(w, "create user", err)
		return
	}
	if err := users.ValidateDisplayName(n.DisplayName); err != nil {
		s.usersError(w, "create user", err)
		return
	}
	if n.Role != users.RoleUser && n.Role != users.RoleServerAdmin {
		s.usersError(w, "create user", users.ErrInvalidRole)
		return
	}
	if err := password.CheckPolicy(req.Password); err != nil {
		jsonReply(w, 400, map[string]string{"error": "invalid_password", "message": err.Error()})
		return
	}
	h, err := s.auth.Passwords.Hash(r.Context(), req.Password)
	if errors.Is(err, password.ErrBusy) {
		busy(w)
		return
	}
	if err != nil {
		internalError(w, "hash password", err)
		return
	}
	n.PasswordHash = h
	u, err := s.auth.Users.Create(r.Context(), n, adminActor(r))
	if err != nil {
		s.usersError(w, "create user", err)
		return
	}
	jsonReply(w, 201, adminUser(u))
}

// adminSetStatus enables or disables an account ({"status": "disabled"}).
// Disabling revokes all its sessions at once.
func (s *Server) adminSetStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("userID")
	if err := s.auth.Users.SetStatus(r.Context(), id, users.Status(req.Status), adminActor(r)); err != nil {
		s.usersError(w, "set status", err)
		return
	}
	s.adminGetUser(w, r)
}

// adminSetPermissions changes server_role and/or can_create_albums in one
// transaction. Omitted fields stay unchanged.
func (s *Server) adminSetPermissions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServerRole      *string `json:"server_role"`
		CanCreateAlbums *bool   `json:"can_create_albums"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var p users.Permissions
	if req.ServerRole != nil {
		role := users.Role(*req.ServerRole)
		p.Role = &role
	}
	p.CanCreateAlbums = req.CanCreateAlbums
	if p.Role == nil && p.CanCreateAlbums == nil {
		errorReply(w, 400, "invalid_request")
		return
	}
	if err := s.auth.Users.SetPermissions(r.Context(), r.PathValue("userID"), p, adminActor(r)); err != nil {
		s.usersError(w, "set permissions", err)
		return
	}
	s.adminGetUser(w, r)
}
