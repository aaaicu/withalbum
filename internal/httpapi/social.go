package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aaaicu/withalbum/internal/authz"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Likes, comments and mentions (docs/API_v2.yaml, tag "social").
//
// Only readers of the folder (OWNER, CONTRIBUTOR, VIEWER) reach these
// routes; an UPLOADER is refused with 403 by the route policy before
// anything is read, an outsider or a server administrator who is not a
// member with 404, like every other album route. Writes re-check the
// caller inside the transaction (meta.Social), are rate limited per
// account, and are audited without the comment text.
//
// A comment body is plain text in which a mention is the token
// "<@user_id>"; the request lists the same accounts in "mentions". The
// server checks that every mentioned account can read the folder when the
// comment is written, and renders the body for each reader ("segments")
// with current display names: a mentioned account that can no longer read
// the folder becomes plain text with the name it had when it was written.

const (
	// maxCommentRunes bounds a comment as shown: after trimming, with each
	// mention counted as "@" and the display name.
	maxCommentRunes = 1000
	// maxSocialBody bounds the JSON body of a comment write.
	maxSocialBody = 64 << 10

	defaultSocialLimit = 50
	maxSocialLimit     = 100
)

var (
	mentionTokenRE = regexp.MustCompile(`<@([0-9a-f]{32})>`)
	userIDRE       = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Write limits per account (socialLimiter): a burst, then one more every
// interval.
var (
	likeLimit    = limitClass{name: "like", burst: 60, every: time.Second}
	commentLimit = limitClass{name: "comment", burst: 20, every: 3 * time.Second}
)

type limitClass struct {
	name  string
	burst float64
	every time.Duration
}

// socialLimiter is a token bucket per account and class, in memory (a
// restart forgets it, which only lets a client write a burst again).
type socialLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newSocialLimiter() *socialLimiter {
	return &socialLimiter{now: time.Now, buckets: map[string]*bucket{}}
}

// allow takes one token of class c for userID, or reports how long until
// one is available.
func (l *socialLimiter) allow(userID string, c limitClass) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	key := c.name + "/" + userID
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: c.burst, at: t}
		l.buckets[key] = b
	}
	if d := t.Sub(b.at); d > 0 {
		b.tokens = min(c.burst, b.tokens+float64(d)/float64(c.every))
	}
	b.at = t
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) * float64(c.every))
}

func (s *Server) rateOK(w http.ResponseWriter, userID string, c limitClass) bool {
	if ok, wait := s.socialLimit.allow(userID, c); !ok {
		rateLimited(w, wait)
		return false
	}
	return true
}

func (s *Server) socialRoutes(rt *router) {
	const asset = "/api/v1/folders/{folderID}/assets/{assetID}"
	rt.handleAlbum("GET "+asset+"/likes", s.listLikes)
	rt.handleAlbum("PUT "+asset+"/likes/me", s.setLike(true))
	rt.handleAlbum("DELETE "+asset+"/likes/me", s.setLike(false))
	rt.handleAlbum("GET "+asset+"/comments", s.listComments)
	rt.handleAlbum("POST "+asset+"/comments", s.createComment)
	rt.handleAlbum("PATCH "+asset+"/comments/{commentID}", s.editComment)
	rt.handleAlbum("DELETE "+asset+"/comments/{commentID}", s.deleteComment)
	rt.handleAlbum("GET /api/v1/folders/{folderID}/mentionable", s.mentionable)
	rt.handle("GET /api/v1/me/activity", s.activity)
}

// personJSON names an account to the readers of a folder.
type personJSON struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
}

// segmentJSON is one piece of a rendered comment: "text", or a "mention"
// of an account that can read the folder now (Text "@" + its current
// display name).
type segmentJSON struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	UserID string `json:"user_id,omitempty"`
}

// commentJSON is a comment as one reader sees it.
type commentJSON struct {
	ID       string `json:"id"`
	FolderID string `json:"folder_id"`
	AssetID  string `json:"asset_id"`
	// Author is null when the author can no longer read the folder (or
	// cannot log in).
	Author *personJSON `json:"author"`
	// Body is the source to edit: "<@user_id>" tokens for the mentions in
	// Segments, every other former mention already plain text.
	Body      string        `json:"body"`
	Segments  []segmentJSON `json:"segments"`
	CreatedAt string        `json:"created_at"`
	EditedAt  *string       `json:"edited_at"`
	IsMine    bool          `json:"is_mine"`
	CanEdit   bool          `json:"can_edit"`
	CanDelete bool          `json:"can_delete"`
}

// renderComment returns the editable source and the segments of a comment.
func renderComment(cm meta.Comment) (string, []segmentJSON) {
	byID := map[string]meta.Mention{}
	for _, m := range cm.Mentions {
		byID[m.UserID] = m
	}
	var src strings.Builder
	segs := []segmentJSON{}
	text := func(t string) {
		src.WriteString(t)
		if t == "" {
			return
		}
		if n := len(segs); n > 0 && segs[n-1].Type == "text" {
			segs[n-1].Text += t
			return
		}
		segs = append(segs, segmentJSON{Type: "text", Text: t})
	}
	last := 0
	for _, loc := range mentionTokenRE.FindAllStringSubmatchIndex(cm.Body, -1) {
		text(cm.Body[last:loc[0]])
		last = loc[1]
		m, ok := byID[cm.Body[loc[2]:loc[3]]]
		switch {
		case !ok: // not written by this server; keep it as it is
			text(cm.Body[loc[0]:loc[1]])
		case m.Reader:
			src.WriteString(cm.Body[loc[0]:loc[1]])
			segs = append(segs, segmentJSON{Type: "mention", Text: "@" + m.Name, UserID: m.UserID})
		default:
			text("@" + m.WrittenName)
		}
	}
	text(cm.Body[last:])
	return src.String(), segs
}

func commentView(cm meta.Comment, me string, owner bool) commentJSON {
	body, segs := renderComment(cm)
	v := commentJSON{ID: cm.ID, FolderID: cm.FolderID, AssetID: cm.AssetID, Body: body, Segments: segs, CreatedAt: cm.CreatedAt,
		EditedAt: nonEmpty(cm.EditedAt), IsMine: cm.AuthorID == me}
	if cm.AuthorName != "" {
		v.Author = &personJSON{UserID: cm.AuthorID, DisplayName: cm.AuthorName}
	}
	v.CanEdit = v.IsMine
	v.CanDelete = v.IsMine || owner
	return v
}

// commentInput is the body of a comment write.
type commentInput struct {
	Body     *string  `json:"body"`
	Mentions []string `json:"mentions"`
}

// parseComment normalizes and checks a comment body and its mentions. It
// returns the stored body, the mentioned accounts (sorted, distinct) and
// "" or the 400 error code.
func parseComment(in commentInput) (string, []string, string) {
	if in.Body == nil {
		return "", nil, "comment_empty"
	}
	body := strings.ReplaceAll(*in.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.TrimSpace(body)
	if body == "" {
		return "", nil, "comment_empty"
	}
	if !utf8.ValidString(body) {
		return "", nil, "invalid_comment"
	}
	for _, r := range body {
		if unicode.IsControl(r) && r != '\n' && r != '\t' || bidiControl(r) {
			return "", nil, "invalid_comment"
		}
	}
	if len(body) > meta.MaxCommentBytes || utf8.RuneCountInString(mentionTokenRE.ReplaceAllString(body, "")) > maxCommentRunes {
		return "", nil, "comment_too_long"
	}
	tokens := map[string]bool{}
	for _, m := range mentionTokenRE.FindAllStringSubmatch(body, -1) {
		tokens[m[1]] = true
	}
	var ids []string
	for _, id := range in.Mentions {
		if !userIDRE.MatchString(id) {
			return "", nil, "invalid_mentions"
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > meta.MaxMentions {
		return "", nil, "too_many_mentions"
	}
	// The body's tokens and the list name the same accounts.
	if len(ids) != len(tokens) {
		return "", nil, "invalid_mentions"
	}
	for _, id := range ids {
		if !tokens[id] {
			return "", nil, "invalid_mentions"
		}
	}
	slices.Sort(ids)
	return body, ids, ""
}

// bidiControl reports the bidirectional embedding, override and isolate
// controls (U+202A-U+202E, U+2066-U+2069). In a comment they would let one
// member make text display in an order other than the one stored (and
// shown to others), e.g. to disguise a mention or a link. The plain marks
// U+200E/U+200F that right-to-left text needs are allowed.
func bidiControl(r rune) bool { return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 }

// plainLabel reports whether s is valid UTF-8 without control characters
// or bidirectional overrides: what a one-line name that other members see
// (album name, uploader device label) may contain.
func plainLabel(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || bidiControl(r) {
			return false
		}
	}
	return true
}

// errCommentTooLong: the body with the mentions' display names is longer
// than maxCommentRunes.
var errCommentTooLong = errors.New("comment too long")

// checkShownLength returns the CheckMentions of a comment write.
func checkShownLength(body string) func(map[string]string) error {
	return func(names map[string]string) error {
		shown := mentionTokenRE.ReplaceAllStringFunc(body, func(tok string) string { return "@" + names[tok[2:len(tok)-1]] })
		if utf8.RuneCountInString(shown) > maxCommentRunes {
			return errCommentTooLong
		}
		return nil
	}
}

// decodeSocial decodes a JSON request body of at most maxSocialBody bytes.
func decodeSocial(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		errorReply(w, 415, "json_required")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSocialBody)).Decode(v); err != nil {
		errorReply(w, 400, "invalid_json")
		return false
	}
	return true
}

// socialError replies to a failed social read or write.
func socialError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, meta.ErrAssetNotFound):
		errorReply(w, 404, "asset_not_found")
	case errors.Is(err, meta.ErrCommentNotFound):
		errorReply(w, 404, "comment_not_found")
	case errors.Is(err, meta.ErrMentionNotReader):
		errorReply(w, 422, "mention_not_member")
	case errors.Is(err, errCommentTooLong):
		errorReply(w, 400, "comment_too_long")
	case errors.Is(err, meta.ErrIdempotencyMismatch):
		errorReply(w, 422, "idempotency_key_reused")
	default:
		membershipError(w, what, err)
	}
}

// encodeSocialCursor makes an opaque cursor of a list position.
func encodeSocialCursor(c meta.SocialCursor) string {
	if c.At == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(c.At + "\n" + c.ID))
}

func parseSocialCursor(v string) (meta.SocialCursor, bool) {
	if v == "" {
		return meta.SocialCursor{}, true
	}
	if len(v) > 400 {
		return meta.SocialCursor{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return meta.SocialCursor{}, false
	}
	at, id, ok := strings.Cut(string(b), "\n")
	if !ok || at == "" || id == "" || strings.ContainsRune(id, '\n') || !utf8.ValidString(id) {
		return meta.SocialCursor{}, false
	}
	return meta.SocialCursor{At: at, ID: id}, true
}

// socialPage parses the cursor and limit of a social list. On error it
// replies and returns false.
func socialPage(w http.ResponseWriter, r *http.Request) (meta.SocialCursor, int, bool) {
	cur, ok := parseSocialCursor(r.URL.Query().Get("cursor"))
	if !ok {
		errorReply(w, 400, "invalid_cursor")
		return meta.SocialCursor{}, 0, false
	}
	limit := defaultSocialLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxSocialLimit || strings.TrimLeft(v, "0123456789") != "" {
			errorReply(w, 400, "invalid_limit")
			return meta.SocialCursor{}, 0, false
		}
		limit = n
	}
	return cur, limit, true
}

func (a albumRequest) isOwner() bool { return a.Folder.Access.OwnerID == a.P.UserID }

// listLikes: GET .../likes, the current readers who like the asset.
func (s *Server) listLikes(w http.ResponseWriter, r *http.Request, a albumRequest) {
	assetID := r.PathValue("assetID")
	likes, err := s.social.ListLikes(r.Context(), a.Folder.Folder.ID, assetID, a.view())
	if err != nil {
		socialReadError(w, err)
		return
	}
	list := make([]map[string]string, 0, len(likes))
	mine := false
	for _, l := range likes {
		mine = mine || l.UserID == a.P.UserID
		list = append(list, map[string]string{"user_id": l.UserID, "display_name": l.DisplayName, "liked_at": l.LikedAt})
	}
	jsonReply(w, 200, map[string]any{"folder_id": a.Folder.Folder.ID, "asset_id": assetID, "like_count": len(list), "liked_by_me": mine, "likes": list})
}

// socialReadError replies to a failed social read.
func socialReadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, meta.ErrAssetNotFound):
		errorReply(w, 404, "asset_not_found")
	case errors.Is(err, meta.ErrFolderNotFound):
		errorReply(w, 404, "folder_not_found")
	default:
		metaReadFailed(w, err)
	}
}

// setLike: PUT (liked) or DELETE .../likes/me, idempotent; both answer 200
// with the caller's summary and whether anything changed.
func (s *Server) setLike(liked bool) albumHandler {
	return func(w http.ResponseWriter, r *http.Request, a albumRequest) {
		if !s.rateOK(w, a.P.UserID, likeLimit) || !s.storageReady(w) {
			return
		}
		assetID := r.PathValue("assetID")
		sum, changed, err := s.social.SetLike(r.Context(), s.change(a, authz.ReactToAsset, assetID), liked)
		if err != nil {
			socialError(w, "set like", err)
			return
		}
		jsonReply(w, 200, map[string]any{"folder_id": a.Folder.Folder.ID, "asset_id": assetID, "like_count": sum.LikeCount,
			"liked_by_me": sum.LikedByMe, "comment_count": sum.CommentCount, "changed": changed})
	}
}

// listComments: GET .../comments?cursor=&limit=, oldest first.
func (s *Server) listComments(w http.ResponseWriter, r *http.Request, a albumRequest) {
	cur, limit, ok := socialPage(w, r)
	if !ok {
		return
	}
	folderID, assetID := a.Folder.Folder.ID, r.PathValue("assetID")
	page, err := s.social.ListComments(r.Context(), folderID, assetID, a.view(), cur, limit)
	if err != nil {
		socialReadError(w, err)
		return
	}
	sums, err := s.social.SocialSummaries(r.Context(), folderID, a.P.UserID, []string{assetID})
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	list := make([]commentJSON, 0, len(page.Comments))
	for _, cm := range page.Comments {
		list = append(list, commentView(cm, a.P.UserID, a.isOwner()))
	}
	next := ""
	if page.HasMore {
		next = encodeSocialCursor(page.Next)
	}
	jsonReply(w, 200, map[string]any{"folder_id": folderID, "asset_id": assetID, "comments": list, "comment_count": sums[assetID].CommentCount,
		"next_cursor": next, "has_more": page.HasMore})
}

// commentChange builds the write of a comment request. The caller is
// re-checked for ReactToAsset inside the transaction; then, for an edit or
// a delete, the stored comment must be the caller's own, or (moderate, a
// delete) the caller may moderate the folder's comments (the owner).
func (s *Server) commentChange(a albumRequest, assetID string, moderate bool) meta.CommentChange {
	c := meta.CommentChange{AlbumChange: s.change(a, authz.ReactToAsset, assetID)}
	var p authz.Principal
	var access authz.AlbumAccess
	base := c.Authorize
	c.Authorize = func(pp authz.Principal, aa authz.AlbumAccess) error {
		p, access = pp, aa
		return base(pp, aa)
	}
	c.AuthorizeComment = func(authorID string) error {
		if authorID == p.UserID || moderate && authz.Allowed(p, authz.ModerateComments, &access) {
			return nil
		}
		return errDenied
	}
	return c
}

// createComment: POST .../comments {"body", "mentions"}, 201 with the
// comment. An optional Idempotency-Key makes a retry return the same
// comment (201, Idempotency-Replayed: true).
func (s *Server) createComment(w http.ResponseWriter, r *http.Request, a albumRequest) {
	key, hasKey := r.Header[http.CanonicalHeaderKey(headerIdempotencyKey)]
	if hasKey && (len(key) != 1 || !validIdempotencyKey(key[0])) {
		errorReply(w, 400, "invalid_idempotency_key")
		return
	}
	var in commentInput
	if !decodeSocial(w, r, &in) {
		return
	}
	body, mentions, code := parseComment(in)
	if code != "" {
		errorReply(w, 400, code)
		return
	}
	if !s.rateOK(w, a.P.UserID, commentLimit) || !s.storageReady(w) {
		return
	}
	c := s.commentChange(a, r.PathValue("assetID"), false)
	c.Body, c.Mentions, c.CheckMentions = body, mentions, checkShownLength(body)
	if hasKey {
		c.Key = key[0]
	}
	cm, replayed, err := s.social.CreateComment(r.Context(), c)
	if err != nil {
		socialError(w, "create comment", err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	jsonReply(w, 201, commentView(cm, a.P.UserID, a.isOwner()))
}

// editComment: PATCH .../comments/{id} {"body", "mentions"} by its author.
func (s *Server) editComment(w http.ResponseWriter, r *http.Request, a albumRequest) {
	var in commentInput
	if !decodeSocial(w, r, &in) {
		return
	}
	body, mentions, code := parseComment(in)
	if code != "" {
		errorReply(w, 400, code)
		return
	}
	if !s.rateOK(w, a.P.UserID, commentLimit) || !s.storageReady(w) {
		return
	}
	c := s.commentChange(a, r.PathValue("assetID"), false)
	c.CommentID = r.PathValue("commentID")
	c.Body, c.Mentions, c.CheckMentions = body, mentions, checkShownLength(body)
	cm, err := s.social.EditComment(r.Context(), c)
	if err != nil {
		socialError(w, "edit comment", err)
		return
	}
	jsonReply(w, 200, commentView(cm, a.P.UserID, a.isOwner()))
}

// deleteComment: DELETE .../comments/{id} by its author or the owner, 204.
func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request, a albumRequest) {
	if !s.rateOK(w, a.P.UserID, commentLimit) || !s.storageReady(w) {
		return
	}
	c := s.commentChange(a, r.PathValue("assetID"), true)
	c.CommentID = r.PathValue("commentID")
	if err := s.social.DeleteComment(r.Context(), c); err != nil {
		socialError(w, "delete comment", err)
		return
	}
	w.WriteHeader(204)
}

// mentionable: GET /folders/{id}/mentionable, the accounts a comment may
// mention: the folder's readers (owner, CONTRIBUTOR, VIEWER) who can log
// in. Never upload-only members.
func (s *Server) mentionable(w http.ResponseWriter, r *http.Request, a albumRequest) {
	list, err := s.social.Mentionable(r.Context(), a.Folder.Folder.ID)
	if err != nil {
		socialReadError(w, err)
		return
	}
	people := make([]map[string]any, 0, len(list))
	for _, m := range list {
		people = append(people, map[string]any{"user_id": m.UserID, "display_name": m.DisplayName, "is_me": m.UserID == a.P.UserID})
	}
	jsonReply(w, 200, map[string]any{"folder_id": a.Folder.Folder.ID, "people": people})
}

// activity: GET /me/activity?cursor=&limit=, newest first: comments that
// mention the caller, and others' comments and likes on what the caller
// uploaded, in the folders the caller can read now.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	cur, limit, ok := socialPage(w, r)
	if !ok {
		return
	}
	page, err := s.social.ListActivity(r.Context(), p.UserID, cur, limit)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	folders, err := s.meta.ListFoldersFor(r.Context(), p.UserID)
	if err != nil {
		metaReadFailed(w, err)
		return
	}
	owner := map[string]bool{}
	for _, fa := range folders {
		owner[fa.Folder.ID] = fa.Access.OwnerID == p.UserID
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, it := range page.Items {
		item := map[string]any{"id": it.ID, "type": it.Kind, "folder_id": it.FolderID, "folder_name": it.FolderName, "asset_id": it.AssetID,
			"actor": nil, "created_at": it.At, "comment": nil}
		if it.ActorName != "" {
			item["actor"] = personJSON{UserID: it.ActorID, DisplayName: it.ActorName}
		}
		if it.Comment != nil && it.Comment.CreatedAt != "" {
			item["comment"] = commentView(*it.Comment, p.UserID, owner[it.FolderID])
		}
		items = append(items, item)
	}
	next := ""
	if page.HasMore {
		next = encodeSocialCursor(page.Next)
	}
	jsonReply(w, 200, map[string]any{"items": items, "next_cursor": next, "has_more": page.HasMore})
}

// summaries returns the social summaries of the given assets for the
// caller (missing: none). On error it replies and returns false.
func (s *Server) summaries(w http.ResponseWriter, r *http.Request, a albumRequest, ids []string) (map[string]meta.SocialSummary, bool) {
	sums, err := s.social.SocialSummaries(r.Context(), a.Folder.Folder.ID, a.P.UserID, ids)
	if err != nil {
		metaReadFailed(w, err)
		return nil, false
	}
	return sums, true
}
