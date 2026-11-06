package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/session"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/users"
)

// Sign up with Google or Apple (M65). The server's administrator decided
// (user decision 2026-11-07) that an account may be made from a verified
// Google or Apple sign-in in two ways:
//
//   - with a usable album invite link (token): the account is active at
//     once and joins that album with the link's role;
//   - without one, when sign-up requests are on (the default): the account
//     is created disabled and waits for the administrator, who approves
//     (enables) or rejects (deletes) it on the dashboard. Signing in before
//     that answers 403 sign_up_pending.
//
// There is no open sign-up: the server may be reachable from the internet
// (Funnel), so a stranger who knows its address gets no usable account
// without a link or the administrator. An identity already linked to an
// account just signs in. The new account has a random password nobody
// knows, so it signs in with its identity only.

func (s *Server) signUpRoutes(rt *router) {
	rt.handle("POST /api/v1/auth/sign-up", s.signUp)
	rt.handle("GET /api/v1/admin/sign-up-requests", s.adminSignUpRequests)
	rt.handle("POST /api/v1/admin/sign-up-requests/{userID}/approve", s.adminApproveSignUp)
	rt.handle("POST /api/v1/admin/sign-up-requests/{userID}/reject", s.adminRejectSignUp)
}

// signUpModes are the ways this server lets a new account be made from a
// sign-in, for the nonce reply: the app offers only these.
func (s *Server) signUpModes() []string {
	if len(providersJSON(s.auth.SignIn)) == 0 {
		return []string{}
	}
	modes := []string{"invite"}
	if !s.auth.noSignUpRequests {
		modes = append(modes, "request")
	}
	return modes
}

func (s *Server) signUp(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var req struct {
		Provider    string `json:"provider"`
		IDToken     string `json:"id_token"`
		Nonce       string `json:"nonce"`
		InviteToken string `json:"invite_token"`
		DisplayName string `json:"display_name"`
		DeviceName  string `json:"device_name"`
		Platform    string `json:"platform"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := session.ValidateDevice(req.DeviceName, req.Platform); err != nil {
		errorReply(w, 400, "invalid_device")
		return
	}
	c, sc := client(r), rateScope(r)
	key := "sign-up:" + req.Provider + ":" + rateKey(c.IP)
	if ok, wait := s.auth.Limiter.AllowIn(sc, rateKey(c.IP), key); !ok {
		rateLimited(w, wait)
		return
	}
	// The link is checked before the nonce is spent, so a wrong link can be
	// fixed without a new Google window.
	var linkHash string
	if req.InviteToken != "" {
		if !s.storageReady(w) {
			return
		}
		hash, ok := s.lookupLink(w, r, "", req.InviteToken, func(st int, code string) { errorReply(w, st, code) })
		if !ok {
			return
		}
		if _, err := s.invites.PreviewInviteLink(r.Context(), hash, time.Now()); err != nil {
			s.linkFailed(r, "", err)
			membershipError(w, "sign-up invite link", err)
			return
		}
		linkHash = hash
	} else if s.auth.noSignUpRequests {
		errorReply(w, 403, "sign_up_closed")
		return
	}
	claims, ok := s.verifyIdentity(w, r, req.Provider, req.IDToken, req.Nonce, 401)
	if !ok {
		s.auth.Limiter.FailureIn(sc, rateKey(c.IP), key)
		return
	}
	login := session.LoginRequest{DeviceName: req.DeviceName, Platform: req.Platform, Client: c}

	// Already an account: sign in (and join the album of the link).
	t, id, err := s.auth.Sessions.SignInLinked(r.Context(), string(claims.Provider), claims.Subject, login)
	switch {
	case err == nil:
		log.Printf("sign-up: %s identity already has an account; signed in", claims.Provider)
		s.signUpReply(w, r, 200, t, id, linkHash)
		return
	case errors.Is(err, session.ErrInvalidCredentials):
		s.disabledSignIn(w, r, claims)
		return
	case !errors.Is(err, session.ErrNotLinked):
		internalError(w, "sign-up", err)
		return
	}

	pending := linkHash == ""
	if _, err := s.createExternal(r, claims, req.DisplayName, pending); err != nil {
		switch {
		case errors.Is(err, users.ErrTooManyRequests):
			log.Printf("sign-up: %s request refused: %d requests already wait for approval", claims.Provider, users.MaxPendingSignUps)
			errorReply(w, 429, "sign_up_queue_full")
		case errors.Is(err, users.ErrIdentityTaken):
			errorReply(w, 409, "identity_in_use") // another request made it just now
		case errors.Is(err, users.ErrNotInitialized):
			errorReply(w, 503, "not_initialized")
		case errors.Is(err, errHashBusy):
			busy(w)
		default:
			internalError(w, "sign-up", err)
		}
		return
	}
	if pending {
		log.Printf("sign-up: %s account requested; waits for the administrator", claims.Provider)
		jsonReply(w, 202, map[string]any{"status": "pending"})
		return
	}
	log.Printf("sign-up: %s account created with an invite link", claims.Provider)
	t, id, err = s.auth.Sessions.SignInLinked(r.Context(), string(claims.Provider), claims.Subject, login)
	if err != nil {
		internalError(w, "sign-up sign-in", err)
		return
	}
	s.signUpReply(w, r, 201, t, id, linkHash)
}

// signUpReply sends the new session, after joining the link's album when
// there is one. A link that ran out between the check and here leaves the
// account without the album (invite_link_gone in "join_error"); the
// account itself is kept, like one made by the administrator.
func (s *Server) signUpReply(w http.ResponseWriter, r *http.Request, status int, t session.Tokens, id session.Identity, linkHash string) {
	body := s.tokenBody(w, r, t, id)
	body["created"] = status == 201
	if linkHash != "" {
		p := principal(id)
		c := meta.AlbumChange{ActorID: id.UserID, SessionID: id.SessionID,
			Authorize: func(p authz.Principal, _ authz.AlbumAccess) error {
				if !p.Active {
					return errDenied
				}
				return nil
			},
			BeforeCommit: func() error { return s.fault(FaultMembershipBeforeCommit) }}
		fa, err := s.invites.RedeemInviteLink(r.Context(), c, linkHash)
		switch {
		case err == nil:
			body["folder"] = folderView(p, fa)
		case errors.Is(err, meta.ErrAlreadyMember):
			body["folder_id"] = fa.Folder.ID
		default:
			log.Printf("sign-up: joining the link's album failed: %v", err)
			body["join_error"] = joinErrorCode(err)
		}
	}
	jsonReply(w, status, body)
}

func joinErrorCode(err error) string {
	if errors.Is(err, meta.ErrInviteLinkNotFound) {
		return "invite_link_not_found"
	}
	return "invite_link_gone"
}

// disabledSignIn answers a sign-in or sign-up of a disabled account:
// sign_up_pending when it waits for approval, else invalid_credentials.
func (s *Server) disabledSignIn(w http.ResponseWriter, r *http.Request, claims oidc.Claims) {
	if userID := s.linkedUser(r, claims); userID != "" {
		if pending, err := s.auth.Users.PendingSignUp(r.Context(), userID); err == nil && pending {
			log.Printf("sign-in: %s account waits for the administrator's approval", claims.Provider)
			errorReply(w, 403, "sign_up_pending")
			return
		}
	}
	errorReply(w, 401, "invalid_credentials")
}

// linkedUser returns the account that linked the identity of claims, or "".
func (s *Server) linkedUser(r *http.Request, claims oidc.Claims) string {
	var id string
	if err := s.auth.db.QueryRowContext(r.Context(), "SELECT user_id FROM user_identities WHERE provider = ? AND subject = ?",
		string(claims.Provider), claims.Subject).Scan(&id); err != nil {
		return ""
	}
	return id
}

var errHashBusy = errors.New("password hashing busy")

// createExternal makes the account of a new identity with a free login
// name derived from its e-mail address.
func (s *Server) createExternal(r *http.Request, claims oidc.Claims, displayName string, pending bool) (users.User, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return users.User{}, err
	}
	h, err := s.auth.Passwords.Hash(r.Context(), base64.RawURLEncoding.EncodeToString(secret))
	if err != nil {
		return users.User{}, errors.Join(errHashBusy, err)
	}
	ext := users.External{Provider: string(claims.Provider), Subject: claims.Subject, Email: claims.Email, Pending: pending}
	n := users.NewUser{DisplayName: signUpDisplayName(displayName, claims), PasswordHash: h, Role: users.RoleUser}
	base := loginBase(claims.Email)
	for i := 0; ; i++ {
		n.LoginName = base
		if i > 0 {
			suffix, err := randomHex(2)
			if err != nil {
				return users.User{}, err
			}
			n.LoginName = base + "-" + suffix
		}
		u, err := s.auth.Users.CreateExternal(r.Context(), n, ext, users.Actor{Via: "sign-up"})
		if errors.Is(err, users.ErrLoginTaken) && i < 5 {
			continue
		}
		return u, err
	}
}

// signUpDisplayName: the name the app sent, else the token's name, else
// the e-mail address's local part, else "가족".
func signUpDisplayName(sent string, claims oidc.Claims) string {
	local, _, _ := strings.Cut(claims.Email, "@")
	for _, n := range []string{strings.TrimSpace(sent), strings.TrimSpace(claims.Name), local} {
		if users.ValidateDisplayName(n) == nil {
			return n
		}
	}
	return "가족"
}

// loginBase derives a login name from an e-mail address's local part:
// lower case, only letters, digits, '.', '_' and '-', starting with a
// letter or digit, 3 to 26 characters (room for a suffix); else "family".
func loginBase(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	var b strings.Builder
	for _, c := range local {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			if b.Len() == 0 && (c == '.' || c == '_' || c == '-') {
				continue
			}
			b.WriteRune(c)
		}
		if b.Len() == 26 {
			break
		}
	}
	if b.Len() < 3 {
		return "family"
	}
	return b.String()
}

type signUpRequestJSON struct {
	UserID      string  `json:"user_id"`
	LoginName   string  `json:"login_name"`
	DisplayName string  `json:"display_name"`
	Provider    string  `json:"provider"`
	Email       *string `json:"email"`
	RequestedAt string  `json:"requested_at"`
}

// adminSignUpRequests: the accounts waiting for approval, oldest first.
func (s *Server) adminSignUpRequests(w http.ResponseWriter, r *http.Request) {
	rs, err := s.auth.Users.SignUpRequests(r.Context())
	if err != nil {
		internalError(w, "list sign-up requests", err)
		return
	}
	items := make([]signUpRequestJSON, 0, len(rs))
	for _, q := range rs {
		j := signUpRequestJSON{UserID: q.User.ID, LoginName: q.User.LoginName, DisplayName: q.User.DisplayName,
			Provider: q.Provider, RequestedAt: q.RequestedAt}
		if q.Email != "" {
			j.Email = &q.Email
		}
		items = append(items, j)
	}
	jsonReply(w, 200, map[string]any{"items": items, "open": !s.auth.noSignUpRequests})
}

func (s *Server) adminApproveSignUp(w http.ResponseWriter, r *http.Request) {
	s.decideSignUp(w, r, s.auth.Users.ApproveSignUp)
}

func (s *Server) adminRejectSignUp(w http.ResponseWriter, r *http.Request) {
	s.decideSignUp(w, r, s.auth.Users.RejectSignUp)
}

func (s *Server) decideSignUp(w http.ResponseWriter, r *http.Request, decide func(context.Context, string, users.Actor) error) {
	err := decide(r.Context(), r.PathValue("userID"), adminActor(r))
	switch {
	case err == nil:
		w.WriteHeader(204)
	case errors.Is(err, users.ErrNoSignUpRequest), errors.Is(err, users.ErrNotFound):
		errorReply(w, 404, "sign_up_request_not_found")
	default:
		internalError(w, "decide sign-up", err)
	}
}
