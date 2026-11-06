package httpapi

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/aaaicu/withalbum/internal/auth/ratelimit"
	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

// Album invite links (M57). The album's owner creates a link with a role,
// an expiry and a number of uses, and shares it (for example through a
// messenger). Whoever holds it learns the album's name and the inviter's
// name (preview, landing page) and, signed in with an account of this
// server, joins the album with the link's role (redeem). The link carries
// the server's pairing fields, so a phone that never paired can connect
// and pin the server; the token is the only secret in it.
//
// The token is 32 random bytes (base64url, 43 characters). Only its
// SHA-256 is stored; the token is in the create reply and nowhere else on
// the server: never in a log line, an audit row or an error. Lookups by
// token are rate limited per client address (preview, landing page) and
// per account (redeem).
//
// There is no sign-up here: a person without an account is told to ask
// this server's administrator for one. Signing up through a link (M54) is
// meant to use the same token, at a new public endpoint, deciding the
// link again in the transaction that creates the account.

const linkTokenBytes = 32

// linkTokenLen is the length of a token: base64url of linkTokenBytes.
var linkTokenLen = base64.RawURLEncoding.EncodedLen(linkTokenBytes)

// newLinkToken returns a fresh token and its stored hash.
func newLinkToken() (token, hash string, err error) {
	b := make([]byte, linkTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, tokenHash(token), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// linkTokenHash checks the shape of a token and returns its hash.
func linkTokenHash(token string) (string, bool) {
	if len(token) != linkTokenLen {
		return "", false
	}
	for _, c := range []byte(token) {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return "", false
		}
	}
	return tokenHash(token), true
}

// newLinkLimiter limits token lookups: per client address 10 at once and
// then one every 6 s; 10 unknown tokens per address (or per account for
// redeem, from any address: 30) in 15 minutes.
func newLinkLimiter() *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{IPBurst: 10, IPInterval: 6 * time.Second, PairFailures: 10, AccountFailures: 30, Window: 15 * time.Minute})
}

// linkKeys are the rate-limit keys of a token lookup: the client address
// and the account (redeem) or the address again (public lookups).
func linkKeys(r *http.Request, userID string) (ratelimit.Scope, string, string) {
	ip := rateKey(clientIP(r))
	if userID != "" {
		return rateScope(r), ip, "user:" + userID
	}
	return rateScope(r), ip, "ip:" + ip
}

func (s *Server) inviteLinkRoutes(rt *router) {
	rt.handleAlbum("GET /api/v1/folders/{folderID}/invite-links", s.listInviteLinks)
	rt.handleAlbum("POST /api/v1/folders/{folderID}/invite-links", s.createInviteLink)
	rt.handleAlbum("DELETE /api/v1/folders/{folderID}/invite-links/{linkID}", s.revokeInviteLink)
	rt.handle("POST /api/v1/invitations/link-preview", s.previewInviteLink)
	rt.handle("POST /api/v1/invitations/link-redeem", s.redeemInviteLink)
	rt.handle("GET /i/{token}", s.inviteLanding)
}

type inviteLinkJSON struct {
	ID          string  `json:"id"`
	FolderID    string  `json:"folder_id"`
	Role        string  `json:"role"`
	MaxUses     int     `json:"max_uses"`
	UseCount    int     `json:"use_count"`
	CreatedAt   string  `json:"created_at"`
	ExpiresAt   string  `json:"expires_at"`
	LastUsedAt  *string `json:"last_used_at"`
	RevokedAt   *string `json:"revoked_at"`
	Status      string  `json:"status"`
	Reason      *string `json:"reason"`
	CreatorID   string  `json:"creator_user_id"`
	CreatorName string  `json:"creator_display_name"`
}

func inviteLinkView(l meta.InviteLink) inviteLinkJSON {
	return inviteLinkJSON{ID: l.ID, FolderID: l.FolderID, Role: string(l.Role), MaxUses: l.MaxUses, UseCount: l.UseCount,
		CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt, LastUsedAt: optString(l.LastUsedAt), RevokedAt: optString(l.RevokedAt),
		Status: l.Status, Reason: optString(l.Reason), CreatorID: l.CreatorID, CreatorName: l.CreatorName}
}

// inviteAppLink is the app link of token, or "" when the server has no
// TLS identity (apps connect only to a pinned server).
func (s *Server) inviteAppLink(token string) string {
	if s.tls == nil || len(s.tls.Pairing) == 0 {
		return ""
	}
	return tlsid.InvitePayload(s.tls.Pairing[0].URL, s.tls.SPKISHA256, s.tls.Extras, token)
}

// inviteWebURL is the landing page of token: on the public route if there
// is one, otherwise on the first direct URL; "" without TLS.
func (s *Server) inviteWebURL(token string) string {
	switch {
	case s.public != nil:
		return s.public.URL + "/i/" + token
	case s.tls != nil && len(s.tls.Pairing) > 0:
		return s.tls.Pairing[0].URL + "/i/" + token
	}
	return ""
}

// listInviteLinks: the album's usable links and those that stopped being
// usable in the last 30 days, newest first (OWNER). Never a token.
func (s *Server) listInviteLinks(w http.ResponseWriter, r *http.Request, a albumRequest) {
	list, err := s.invites.ListInviteLinks(r.Context(), a.Folder.Folder.ID, time.Now())
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	out := make([]inviteLinkJSON, 0, len(list))
	for _, l := range list {
		out = append(out, inviteLinkView(l))
	}
	jsonReply(w, 200, map[string]any{"invite_links": out})
}

// createInviteLink: {"role": ..., "expires_in_days": 1..30 (7),
// "max_uses": 1..20 (1)} (OWNER). The reply is the only place the token
// ever appears.
func (s *Server) createInviteLink(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var req struct {
		Role          string `json:"role"`
		ExpiresInDays *int   `json:"expires_in_days"`
		MaxUses       *int   `json:"max_uses"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	days, uses := 7, 1
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}
	if req.MaxUses != nil {
		uses = *req.MaxUses
	}
	if days < 1 || days > int(meta.InviteLinkMaxTTL/(24*time.Hour)) || uses < 1 || uses > meta.InviteLinkMaxUses {
		errorReply(w, 400, "invalid_invite_link")
		return
	}
	if !s.storageReady(w) {
		return
	}
	token, hash, err := newLinkToken()
	if err != nil {
		errorReply(w, 500, "random_failure")
		return
	}
	c := s.change(a, authz.InviteMember, "")
	c.Role = authz.AlbumRole(req.Role)
	l, err := s.invites.CreateInviteLink(r.Context(), meta.NewInviteLink{Change: c, TokenHash: hash,
		TTL: time.Duration(days) * 24 * time.Hour, MaxUses: uses})
	if err != nil {
		membershipError(w, "create invite link", err)
		return
	}
	noStore(w)
	jsonReply(w, 201, map[string]any{"invite_link": inviteLinkView(l), "token": token,
		"app_link": optString(s.inviteAppLink(token)), "web_url": optString(s.inviteWebURL(token))})
}

func (s *Server) revokeInviteLink(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.storageReady(w) {
		return
	}
	if err := s.invites.RevokeInviteLink(r.Context(), s.change(a, authz.InviteMember, r.PathValue("linkID"))); err != nil {
		membershipError(w, "revoke invite link", err)
		return
	}
	w.WriteHeader(204)
}

// lookupLink rate-limits and decides a token lookup for userID ("" for
// the public ones) and returns the hash, or replies (429, 404) and
// returns false. A token that is malformed counts as unknown.
func (s *Server) lookupLink(w http.ResponseWriter, r *http.Request, userID, token string, reply func(int, string)) (string, bool) {
	sc, ip, key := linkKeys(r, userID)
	if ok, wait := s.linkLimit.AllowIn(sc, ip, key); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		reply(429, "rate_limited")
		return "", false
	}
	hash, ok := linkTokenHash(token)
	if !ok {
		s.linkLimit.FailureIn(sc, ip, key)
		reply(404, "invite_link_not_found")
		return "", false
	}
	return hash, true
}

// linkFailed counts an unknown token against the caller.
func (s *Server) linkFailed(r *http.Request, userID string, err error) {
	if errors.Is(err, meta.ErrInviteLinkNotFound) {
		sc, ip, key := linkKeys(r, userID)
		s.linkLimit.FailureIn(sc, ip, key)
	}
}

// previewInviteLink: POST {"token": ...}, public. What the holder of a
// usable link may know before signing in; 404 for an unknown token and
// 410 invite_link_gone (with reason, without the album) for one that can
// no longer be used. sign_up_available is false: accounts are made by
// this server's administrator (M54 may change it).
func (s *Server) previewInviteLink(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	hash, ok := s.lookupLink(w, r, "", req.Token, func(st int, code string) { errorReply(w, st, code) })
	if !ok {
		return
	}
	p, err := s.invites.PreviewInviteLink(r.Context(), hash, time.Now())
	if err != nil {
		s.linkFailed(r, "", err)
		membershipError(w, "preview invite link", err)
		return
	}
	jsonReply(w, 200, map[string]any{"folder_name": p.FolderName, "inviter_display_name": p.InviterName,
		"role": string(p.Role), "expires_at": p.ExpiresAt, "sign_up_available": len(providersJSON(s.auth.SignIn)) > 0})
}

// redeemInviteLink: POST {"token": ...}, any signed-in account (the app
// shows the preview and asks first). One transaction: membership with the
// link's role, one use spent, an accepted invitation naming the link, a
// pending invitation of the same album revoked as replaced, audit row.
// 404 invite_link_not_found, 410 invite_link_gone {reason}, 409
// already_member {folder_id} (nothing spent).
func (s *Server) redeemInviteLink(w http.ResponseWriter, r *http.Request) {
	id, _ := IdentityFrom(r.Context())
	p := principal(id)
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.storageReady(w) {
		return
	}
	hash, ok := s.lookupLink(w, r, id.UserID, req.Token, func(st int, code string) { errorReply(w, st, code) })
	if !ok {
		return
	}
	c := meta.AlbumChange{ActorID: id.UserID, SessionID: id.SessionID,
		Authorize: func(p authz.Principal, _ authz.AlbumAccess) error {
			if !p.Active {
				return errDenied
			}
			return nil
		},
		BeforeCommit: func() error { return s.fault(FaultMembershipBeforeCommit) }}
	fa, err := s.invites.RedeemInviteLink(r.Context(), c, hash)
	if errors.Is(err, meta.ErrAlreadyMember) && fa.Folder.ID != "" {
		jsonReply(w, 409, map[string]string{"error": "already_member", "folder_id": fa.Folder.ID})
		return
	}
	if err != nil {
		s.linkFailed(r, id.UserID, err)
		membershipError(w, "redeem invite link", err)
		return
	}
	jsonReply(w, 200, folderView(p, fa))
}

//go:embed web/invite.html
var invitePageSource string

var invitePage = template.Must(template.New("invite").Parse(invitePageSource))

// inviteCSP is the policy of the landing page: no script at all, its one
// inline <style> by hash, nothing loaded from anywhere, no form, no frame.
var inviteCSP = "default-src 'none'; style-src " + inlineHash([]byte(invitePageSource), "style") +
	"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// roleLabels are the role names the app and the dashboard show.
var roleLabels = map[authz.AlbumRole]string{authz.Viewer: "보기", authz.Contributor: "참여", authz.Uploader: "업로드 전용"}

type invitePageData struct {
	OK          bool
	Status      int
	FolderName  string
	InviterName string
	Role        string
	Until       string
	AppLink     template.URL // built here from url.Values; never from input other than the token checked above
}

// inviteLanding: GET /i/{token}, public, also on the funnel route. A
// minimal page for a person who opened the link in a browser: the album's
// name, the inviter's name, the role and the expiry of a usable link (or
// that it cannot be used, without any album detail), how to open it in
// the app, and that an account comes from this server's administrator.
// It never spends a use (link previewers fetch URLs too).
func (s *Server) inviteLanding(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", inviteCSP)
	h.Set("X-Robots-Tag", "noindex, nofollow")
	noStore(w)
	render := func(d invitePageData) {
		var b bytes.Buffer
		if err := invitePage.Execute(&b, d); err != nil {
			log.Printf("invite page: %v", err)
			http.Error(w, "internal error", 500)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(d.Status)
		_, _ = w.Write(b.Bytes())
	}
	token := r.PathValue("token")
	hash, ok := s.lookupLink(w, r, "", token, func(st int, _ string) { render(invitePageData{Status: st}) })
	if !ok {
		return
	}
	p, err := s.invites.PreviewInviteLink(r.Context(), hash, time.Now())
	switch {
	case errors.Is(err, meta.ErrInviteLinkNotFound):
		s.linkFailed(r, "", err)
		render(invitePageData{Status: 404})
		return
	case errors.Is(err, meta.ErrInviteLinkGone):
		render(invitePageData{Status: 410})
		return
	case err != nil:
		log.Printf("invite page lookup failed: %v", err)
		render(invitePageData{Status: 500})
		return
	}
	d := invitePageData{OK: true, Status: 200, FolderName: p.FolderName, InviterName: p.InviterName, Role: roleLabels[p.Role],
		AppLink: template.URL(s.inviteAppLink(token))} // #nosec G203 -- tlsid.InvitePayload query-encodes every field; the token matched [A-Za-z0-9_-]{43}
	if t, err := time.Parse(time.RFC3339Nano, p.ExpiresAt); err == nil {
		d.Until = t.Local().Format("2006년 1월 2일 15:04")
	}
	render(d)
}
