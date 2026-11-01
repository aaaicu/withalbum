package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/tlsid"
)

// Invitation history and invite links (M57).

var linkAddr atomic.Int64

// linkCall sends a request as login ("" for none) from addr ("" for a
// fresh address, so that the per-address limits of token lookups do not
// carry from one call to the next).
func (e *aclEnv) linkCall(t *testing.T, login, addr, method, path, body string) reply {
	t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if login != "" {
		req.Header.Set("Authorization", "Bearer "+e.tokens[login])
	}
	if addr == "" {
		n := linkAddr.Add(1)
		addr = fmt.Sprintf("10.57.%d.%d:5000", n/250%250, n%250+1)
	}
	req.RemoteAddr = addr
	w := httptest.NewRecorder()
	e.Raw.ServeHTTP(w, req)
	return reply{Status: w.Code, Header: w.Header(), Body: w.Body.Bytes()}
}

// newLink creates an invite link of family and returns the create reply.
func (e *aclEnv) newLink(t *testing.T, body string) map[string]any {
	t.Helper()
	return wantJSON(t, "create link", e.asJSON(t, "owner", "POST", "/api/v1/folders/"+e.family+"/invite-links", body), 201)
}

func (e *aclEnv) redeem(t *testing.T, login, token string) reply {
	t.Helper()
	return e.linkCall(t, login, "", "POST", "/api/v1/invitations/link-redeem", `{"token":`+mustJSON(t, token)+`}`)
}

func list(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, v := range m["invitations"].([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

func (e *aclEnv) myInvitations(t *testing.T, login, query string) map[string]any {
	t.Helper()
	return wantJSON(t, "my invitations "+query, e.as(t, login, "GET", "/api/v1/invitations"+query, nil, nil), 200)
}

func (e *aclEnv) newAlbum(t *testing.T, login, name string) string {
	t.Helper()
	return wantJSON(t, "create "+name, e.asJSON(t, login, "POST", "/api/v1/folders", `{"name":`+mustJSON(t, name)+`}`), 201)["id"].(string)
}

func (e *aclEnv) inviteTo(t *testing.T, owner, folder, login, role string) string {
	t.Helper()
	r := e.asJSON(t, owner, "POST", "/api/v1/folders/"+folder+"/invitations", `{"invited_user_id":"`+e.ids[login]+`","role":"`+role+`"}`)
	return wantJSON(t, "invite "+login, r, 201)["id"].(string)
}

func (e *aclEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.Meta.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func stamp(d time.Duration) string {
	return time.Now().Add(d).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// TestInvitationHistory: several albums invite one account at once; the
// pending ones come newest first, then the answered ones of the window by
// answer time, with status, reason, answered_at and member_now; paging
// with limit and cursor visits each exactly once; the window, the filter
// parameters and the owner's neutral view.
func TestInvitationHistory(t *testing.T) {
	e := newACLEnv(t)
	ids := map[string]string{}
	for i, name := range []string{"a", "b", "c", "d", "e", "f"} {
		f := e.newAlbum(t, "owner", "album "+name)
		ids[name] = e.inviteTo(t, "owner", f, "outsider", "VIEWER")
		// Distinct creation times, oldest first.
		e.exec(t, "UPDATE folder_invitations SET created_at = ? WHERE invitation_id = ?", stamp(time.Duration(i-10)*time.Hour), ids[name])
	}
	// Default list: the pending ones only, newest first.
	got := list(t, e.myInvitations(t, "outsider", ""))
	if len(got) != 6 || got[0]["id"] != ids["f"] || got[5]["id"] != ids["a"] {
		t.Fatalf("pending order %v", got)
	}
	// Answer some: accept a, decline b, the owner revokes c, d expires.
	wantJSON(t, "accept a", e.as(t, "outsider", "POST", "/api/v1/invitations/"+ids["a"]+"/accept", nil, nil), 200)
	if r := e.as(t, "outsider", "POST", "/api/v1/invitations/"+ids["b"]+"/decline", nil, nil); r.Status != 204 {
		t.Fatalf("decline %d", r.Status)
	}
	var folderC string
	if err := e.Meta.DB().QueryRow("SELECT folder_id FROM folder_invitations WHERE invitation_id = ?", ids["c"]).Scan(&folderC); err != nil {
		t.Fatal(err)
	}
	if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+folderC+"/invitations/"+ids["c"], nil, nil); r.Status != 204 {
		t.Fatalf("revoke %d", r.Status)
	}
	e.exec(t, "UPDATE folder_invitations SET expires_at = ? WHERE invitation_id = ?", stamp(-time.Minute), ids["d"])
	// Times of the answers, so that the order is known: c (revoked) most
	// recently, then b, then a.
	e.exec(t, "UPDATE folder_invitations SET accepted_at = ? WHERE invitation_id = ?", stamp(-3*time.Hour), ids["a"])
	e.exec(t, "UPDATE folder_invitations SET declined_at = ? WHERE invitation_id = ?", stamp(-2*time.Hour), ids["b"])

	if got := list(t, e.myInvitations(t, "outsider", "")); len(got) != 2 || got[0]["id"] != ids["f"] || got[1]["id"] != ids["e"] {
		t.Fatalf("pending after answers %v", got)
	}
	all := list(t, e.myInvitations(t, "outsider", "?include=recent"))
	var order []string
	for _, inv := range all {
		order = append(order, inv["status"].(string))
	}
	if strings.Join(order, ",") != "pending,pending,revoked,expired,declined,accepted" {
		t.Fatalf("history order %v", order)
	}
	byID := map[string]map[string]any{}
	for _, inv := range all {
		byID[inv["id"].(string)] = inv
	}
	if inv := byID[ids["a"]]; inv["member_now"] != true || inv["answered_at"] == nil || inv["reason"] != nil || inv["source"] != "direct" {
		t.Fatalf("accepted %v", inv)
	}
	if inv := byID[ids["c"]]; inv["reason"] != "revoked_by_owner" || inv["answered_at"] == nil || inv["member_now"] != false {
		t.Fatalf("revoked %v", inv)
	}
	if inv := byID[ids["d"]]; inv["answered_at"] != inv["expires_at"] {
		t.Fatalf("expired %v", inv)
	}
	// Paging: two at a time, every invitation once, in the same order.
	var paged []string
	cursor := ""
	for range 10 {
		m := e.myInvitations(t, "outsider", "?include=recent&limit=2&cursor="+url.QueryEscape(cursor))
		for _, inv := range list(t, m) {
			paged = append(paged, inv["id"].(string))
		}
		if m["next_cursor"] == nil {
			break
		}
		cursor = m["next_cursor"].(string)
	}
	var want []string
	for _, inv := range all {
		want = append(want, inv["id"].(string))
	}
	if strings.Join(paged, ",") != strings.Join(want, ",") {
		t.Fatalf("paged %v, want %v", paged, want)
	}
	// The window: a was accepted 3 hours ago; days=1 keeps it, and moved
	// 2 days back it drops out of days=1 but not out of the default 30.
	e.exec(t, "UPDATE folder_invitations SET accepted_at = ? WHERE invitation_id = ?", stamp(-48*time.Hour), ids["a"])
	for q, n := range map[string]int{"?include=recent&days=1": 5, "?include=recent": 6} {
		if got := list(t, e.myInvitations(t, "outsider", q)); len(got) != n {
			t.Fatalf("%s: %d invitations", q, len(got))
		}
	}
	// Leaving the album: member_now false.
	var folderA string
	if err := e.Meta.DB().QueryRow("SELECT folder_id FROM folder_invitations WHERE invitation_id = ?", ids["a"]).Scan(&folderA); err != nil {
		t.Fatal(err)
	}
	e.exec(t, "DELETE FROM folder_members WHERE folder_id = ? AND user_id = ?", folderA, e.ids["outsider"])
	for _, inv := range list(t, e.myInvitations(t, "outsider", "?include=recent")) {
		if inv["id"] == ids["a"] && inv["member_now"] != false {
			t.Fatalf("removed member %v", inv)
		}
	}
	// Bad parameters.
	for q, code := range map[string]string{"?include=all": "invalid_filter", "?days=3": "invalid_filter", "?include=recent&days=31": "invalid_filter",
		"?include=recent&days=0": "invalid_filter", "?limit=0": "invalid_limit", "?limit=101": "invalid_limit", "?cursor=%21%21": "invalid_cursor",
		"?cursor=" + url.QueryEscape("eA"): "invalid_cursor"} {
		wantError(t, q, e.as(t, "outsider", "GET", "/api/v1/invitations"+q, nil, nil), 400, code)
	}
	// Someone else's history is not visible: root sees none of these.
	if got := list(t, e.myInvitations(t, "root", "?include=recent")); len(got) != 0 {
		t.Fatalf("root sees %v", got)
	}
	// The owner's view of album b: declined is "not_joined".
	var folderB string
	if err := e.Meta.DB().QueryRow("SELECT folder_id FROM folder_invitations WHERE invitation_id = ?", ids["b"]).Scan(&folderB); err != nil {
		t.Fatal(err)
	}
	owner := list(t, wantJSON(t, "owner history", e.as(t, "owner", "GET", "/api/v1/folders/"+folderB+"/invitations?include=recent", nil, nil), 200))
	if len(owner) != 1 || owner[0]["status"] != "not_joined" {
		t.Fatalf("owner history %v", owner)
	}
	if got := list(t, wantJSON(t, "owner pending", e.as(t, "owner", "GET", "/api/v1/folders/"+folderB+"/invitations", nil, nil), 200)); len(got) != 0 {
		t.Fatalf("owner pending %v", got)
	}
}

// TestInvitationReasons: the reason of a revocation, in the 410 of an
// answer and in the invitee's history.
func TestInvitationReasons(t *testing.T) {
	e := newACLEnv(t)
	archived := e.newAlbum(t, "owner", "to archive")
	deleted := e.newAlbum(t, "owner", "to delete")
	moved := e.newAlbum(t, "owner", "to transfer")
	e.exec(t, "INSERT INTO folder_members (folder_id, user_id, role) VALUES (?, ?, 'VIEWER')", moved, e.ids["viewer"])
	inv := map[string]string{
		archived: e.inviteTo(t, "owner", archived, "outsider", "VIEWER"),
		deleted:  e.inviteTo(t, "owner", deleted, "outsider", "VIEWER"),
		moved:    e.inviteTo(t, "owner", moved, "outsider", "VIEWER"),
	}
	if r := e.as(t, "owner", "POST", "/api/v1/folders/"+archived+"/archive", nil, nil); r.Status != 204 {
		t.Fatalf("archive %d", r.Status)
	}
	if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+deleted, nil, nil); r.Status != 204 {
		t.Fatalf("delete %d", r.Status)
	}
	wantJSON(t, "transfer", e.asJSON(t, "owner", "POST", "/api/v1/folders/"+moved+"/owner", `{"new_owner_user_id":"`+e.ids["viewer"]+`"}`), 200)
	want := map[string]string{archived: "album_archived", deleted: "album_deleted", moved: "owner_changed"}
	for f, id := range inv {
		wantGone(t, "accept "+want[f], e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), "invitation_gone", want[f])
		wantGone(t, "decline "+want[f], e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/decline", nil, nil), "invitation_gone", want[f])
	}
	hist := list(t, e.myInvitations(t, "outsider", "?include=recent"))
	if len(hist) != 3 {
		t.Fatalf("history %v", hist)
	}
	for _, h := range hist {
		if h["status"] != "revoked" || h["reason"] != want[h["folder_id"].(string)] || h["answered_at"] == nil {
			t.Fatalf("revoked invitation %v", h)
		}
	}
	// A row still stored pending in an archived folder (written otherwise)
	// is also refused with the folder's reason.
	other := e.newAlbum(t, "owner", "stale")
	id := e.inviteTo(t, "owner", other, "outsider", "VIEWER")
	e.exec(t, "UPDATE folders SET archived_at = ? WHERE folder_id = ?", stamp(0), other)
	wantGone(t, "stale archived", e.as(t, "outsider", "POST", "/api/v1/invitations/"+id+"/accept", nil, nil), "invitation_gone", "album_archived")
	// Legacy rows revoked before reasons were recorded: "revoked".
	e.exec(t, "UPDATE folder_invitations SET revoke_reason = NULL WHERE invitation_id = ?", inv[moved])
	wantGone(t, "legacy", e.as(t, "outsider", "POST", "/api/v1/invitations/"+inv[moved]+"/accept", nil, nil), "invitation_gone", "revoked")
}

// TestReplacePending: re-inviting with replace_pending revokes the
// pending invitation (reason replaced, listed nowhere) and leaves one card.
func TestReplacePending(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	first := e.invite(t, "outsider", "VIEWER")
	wantError(t, "again", e.asJSON(t, "owner", "POST", f+"/invitations", `{"invited_user_id":"`+e.ids["outsider"]+`","role":"CONTRIBUTOR"}`), 409, "invitation_pending")
	second := wantJSON(t, "replace", e.asJSON(t, "owner", "POST", f+"/invitations",
		`{"invited_user_id":"`+e.ids["outsider"]+`","role":"CONTRIBUTOR","replace_pending":true}`), 201)
	if second["id"] == first || second["role"] != "CONTRIBUTOR" {
		t.Fatalf("replacement %v", second)
	}
	for _, q := range []string{"", "?include=recent"} {
		got := list(t, e.myInvitations(t, "outsider", q))
		if len(got) != 1 || got[0]["id"] != second["id"] {
			t.Fatalf("%q: %v", q, got)
		}
		owner := list(t, wantJSON(t, "owner", e.as(t, "owner", "GET", f+"/invitations"+q, nil, nil), 200))
		if len(owner) != 1 || owner[0]["id"] != second["id"] {
			t.Fatalf("owner %q: %v", q, owner)
		}
	}
	wantGone(t, "old one", e.as(t, "outsider", "POST", "/api/v1/invitations/"+first+"/accept", nil, nil), "invitation_gone", "replaced")
	var details string
	if err := e.Meta.DB().QueryRow("SELECT details_json FROM audit_log WHERE action = 'invitation.create' AND target_id = ?", second["id"]).Scan(&details); err != nil ||
		!strings.Contains(details, first) {
		t.Fatalf("audit %s %v", details, err)
	}
	wantJSON(t, "accept new", e.as(t, "outsider", "POST", "/api/v1/invitations/"+second["id"].(string)+"/accept", nil, nil), 200)
	if got := wantJSON(t, "folder", e.as(t, "outsider", "GET", f, nil, nil), 200)["role"]; got != "CONTRIBUTOR" {
		t.Fatalf("role %v", got)
	}
}

// TestInviteLinkFlow: create (defaults, limits, the token once), preview,
// redeem (membership, history on both sides, audit without token), the
// app link and landing URL, already a member, used up, expired, revoked.
func TestInviteLinkFlow(t *testing.T) {
	e := newACLEnv(t)
	pin := strings.Repeat("ab", 32)
	e.API.SetTLSInfo(&httpapi.TLSInfo{SPKISHA256: pin, Pairing: []httpapi.Pairing{{URL: "https://192.0.2.10:8787"}},
		Extras: tlsid.PairingExtras{RelayAddr: "relay.example.org:443", RID: "0123456789abcdef"}})
	f := "/api/v1/folders/" + e.family
	for _, body := range []string{`{"role":"OWNER"}`, `{"role":"VIEWER","max_uses":0}`, `{"role":"VIEWER","max_uses":21}`,
		`{"role":"VIEWER","expires_in_days":0}`, `{"role":"VIEWER","expires_in_days":31}`} {
		r := e.asJSON(t, "owner", "POST", f+"/invite-links", body)
		if r.Status != 400 {
			t.Fatalf("%s: %d %s", body, r.Status, r.Body)
		}
	}
	created := e.newLink(t, `{"role":"CONTRIBUTOR","max_uses":2}`)
	wantKeys(t, "create", created, "invite_link", "token", "app_link", "web_url")
	token := created["token"].(string)
	link := created["invite_link"].(map[string]any)
	if len(token) != 43 || link["status"] != "active" || link["max_uses"] != float64(2) || link["use_count"] != float64(0) || link["role"] != "CONTRIBUTOR" {
		t.Fatalf("created %v", created)
	}
	if exp, _ := time.Parse(time.RFC3339, link["expires_at"].(string)); time.Until(exp) < 7*24*time.Hour-time.Minute || time.Until(exp) > 7*24*time.Hour {
		t.Fatalf("default expiry %v", link["expires_at"])
	}
	u, err := url.Parse(created["app_link"].(string))
	if err != nil || u.Scheme != "withalbum" || u.Host != "invite" {
		t.Fatalf("app link %v %v", created["app_link"], err)
	}
	if q := u.Query(); q.Get("token") != token || q.Get("spki_sha256") != pin || q.Get("url") != "https://192.0.2.10:8787" || q.Get("rid") != "0123456789abcdef" || q.Get("v") != "1" {
		t.Fatalf("app link query %v", q)
	}
	if created["web_url"] != "https://192.0.2.10:8787/i/"+token {
		t.Fatalf("web url %v", created["web_url"])
	}
	e.API.SetPublicRoute(&httpapi.PublicRoute{URL: "https://home.example-tailnet.ts.net", Route: httpapi.RouteFunnel})
	if pub := e.newLink(t, `{"role":"VIEWER"}`); pub["web_url"] != "https://home.example-tailnet.ts.net/i/"+pub["token"].(string) {
		t.Fatalf("public web url %v", pub["web_url"])
	}
	// Only the hash is stored, and the token is nowhere in the database.
	var n int
	if err := e.Meta.DB().QueryRow("SELECT count(*) FROM invite_links WHERE token_sha256 = ?", shaHex([]byte(token))).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hash rows %d %v", n, err)
	}
	if e.count(t, "SELECT count(*) FROM audit_log WHERE details_json LIKE ?", "%"+token+"%") != 0 {
		t.Fatal("token in the audit log")
	}
	// Preview, public.
	p := wantJSON(t, "preview", e.linkCall(t, "", "", "POST", "/api/v1/invitations/link-preview", `{"token":"`+token+`"}`), 200)
	wantKeys(t, "preview", p, "folder_name", "inviter_display_name", "role", "expires_at", "sign_up_available")
	if p["folder_name"] != "family" || p["inviter_display_name"] != "owner" || p["role"] != "CONTRIBUTOR" || p["sign_up_available"] != false {
		t.Fatalf("preview %v", p)
	}
	wantError(t, "preview unknown", e.linkCall(t, "", "", "POST", "/api/v1/invitations/link-preview", `{"token":"`+strings.Repeat("A", 43)+`"}`), 404, "invite_link_not_found")
	wantError(t, "preview malformed", e.linkCall(t, "", "", "POST", "/api/v1/invitations/link-preview", `{"token":"short"}`), 404, "invite_link_not_found")
	// Redeem needs a session; members and the owner spend nothing.
	if r := e.redeem(t, "", token); r.Status != 401 {
		t.Fatalf("anonymous redeem %d", r.Status)
	}
	for _, who := range []string{"owner", "viewer"} {
		m := wantJSON(t, "member redeems", e.redeem(t, who, token), 409)
		if m["error"] != "already_member" || m["folder_id"] != e.family {
			t.Fatalf("%s: %v", who, m)
		}
	}
	// A pending invitation of the redeeming account is replaced.
	pending := e.invite(t, "outsider", "VIEWER")
	folder := wantJSON(t, "redeem", e.redeem(t, "outsider", token), 200)
	if folder["id"] != e.family || folder["role"] != "CONTRIBUTOR" || folder["can_upload"] != true {
		t.Fatalf("redeemed %v", folder)
	}
	wantGone(t, "replaced", e.as(t, "outsider", "POST", "/api/v1/invitations/"+pending+"/accept", nil, nil), "invitation_gone", "replaced")
	hist := list(t, e.myInvitations(t, "outsider", "?include=recent"))
	if len(hist) != 1 || hist[0]["source"] != "link" || hist[0]["status"] != "accepted" || hist[0]["member_now"] != true || hist[0]["inviter_display_name"] != "owner" {
		t.Fatalf("invitee history %v", hist)
	}
	owner := list(t, wantJSON(t, "owner history", e.as(t, "owner", "GET", f+"/invitations?include=recent", nil, nil), 200))
	if len(owner) != 1 || owner[0]["source"] != "link" || owner[0]["invitee_user_id"] != e.ids["outsider"] {
		t.Fatalf("owner history %v", owner)
	}
	if e.audits(t, "invite_link.redeem") != 1 || e.count(t, "SELECT count(*) FROM audit_log WHERE details_json LIKE ?", "%"+token+"%") != 0 {
		t.Fatal("redeem audit")
	}
	// Second use by root; then it is used up.
	wantJSON(t, "second use", e.redeem(t, "root", token), 200)
	e.setMember(t, "outsider", "")
	wantGone(t, "used up", e.redeem(t, "outsider", token), "invite_link_gone", "used_up")
	wantGone(t, "preview used up", e.linkCall(t, "", "", "POST", "/api/v1/invitations/link-preview", `{"token":"`+token+`"}`), "invite_link_gone", "used_up")
	links := wantJSON(t, "links", e.as(t, "owner", "GET", f+"/invite-links", nil, nil), 200)["invite_links"].([]any)
	var used map[string]any
	for _, l := range links {
		if l.(map[string]any)["id"] == link["id"] {
			used = l.(map[string]any)
		}
		if strings.Contains(fmt.Sprint(l), token) {
			t.Fatal("token listed")
		}
	}
	if used == nil || used["status"] != "used_up" || used["use_count"] != float64(2) || used["last_used_at"] == nil {
		t.Fatalf("used link %v", used)
	}
	// Expired.
	exp := e.newLink(t, `{"role":"VIEWER"}`)
	e.exec(t, "UPDATE invite_links SET expires_at = ? WHERE link_id = ?", stamp(-time.Second), exp["invite_link"].(map[string]any)["id"])
	wantGone(t, "expired", e.redeem(t, "outsider", exp["token"].(string)), "invite_link_gone", "expired")
	// Revoked by the owner; revoking twice is 410; another folder 404.
	rev := e.newLink(t, `{"role":"VIEWER"}`)
	rid := rev["invite_link"].(map[string]any)["id"].(string)
	wantError(t, "revoke via other folder", e.as(t, "root", "DELETE", "/api/v1/folders/"+e.adminF+"/invite-links/"+rid, nil, nil), 404, "invite_link_not_found")
	if r := e.as(t, "owner", "DELETE", f+"/invite-links/"+rid, nil, nil); r.Status != 204 {
		t.Fatalf("revoke %d %s", r.Status, r.Body)
	}
	wantGone(t, "revoke twice", e.as(t, "owner", "DELETE", f+"/invite-links/"+rid, nil, nil), "invite_link_gone", "revoked_by_owner")
	wantGone(t, "redeem revoked", e.redeem(t, "outsider", rev["token"].(string)), "invite_link_gone", "revoked_by_owner")
	// Members cannot manage links.
	for _, who := range []string{"contributor", "viewer", "uploader"} {
		wantError(t, who+" lists", e.as(t, who, "GET", f+"/invite-links", nil, nil), 403, "forbidden")
		wantError(t, who+" creates", e.asJSON(t, who, "POST", f+"/invite-links", `{"role":"VIEWER"}`), 403, "forbidden")
	}
	// An inactive account cannot redeem.
	act := e.newLink(t, `{"role":"VIEWER"}`)
	e.exec(t, "UPDATE users SET status = 'disabled' WHERE user_id = ?", e.ids["outsider"])
	if r := e.redeem(t, "outsider", act["token"].(string)); r.Status != 401 && r.Status != 403 {
		t.Fatalf("disabled redeem %d %s", r.Status, r.Body)
	}
}

// TestInviteLinkLimitAndRevocation: at most 20 usable links per album;
// deleting the album, archiving it and changing its owner revoke its
// links with the matching reason.
func TestInviteLinkLimitAndRevocation(t *testing.T) {
	e := newACLEnv(t)
	for range 20 {
		e.newLink(t, `{"role":"VIEWER"}`)
	}
	wantError(t, "21st", e.asJSON(t, "owner", "POST", "/api/v1/folders/"+e.family+"/invite-links", `{"role":"VIEWER"}`), 409, "too_many_invite_links")
	e.exec(t, "UPDATE invite_links SET revoked_at = ?, revoke_reason = 'revoked_by_owner' WHERE link_id IN (SELECT link_id FROM invite_links LIMIT 1)", stamp(0))
	e.newLink(t, `{"role":"VIEWER"}`)

	cases := map[string]func(f string){
		"album_deleted": func(f string) {
			if r := e.as(t, "owner", "DELETE", "/api/v1/folders/"+f, nil, nil); r.Status != 204 {
				t.Fatalf("delete %d", r.Status)
			}
		},
		"album_archived": func(f string) {
			if r := e.as(t, "owner", "POST", "/api/v1/folders/"+f+"/archive", nil, nil); r.Status != 204 {
				t.Fatalf("archive %d", r.Status)
			}
		},
		"owner_changed": func(f string) {
			e.exec(t, "INSERT INTO folder_members (folder_id, user_id, role) VALUES (?, ?, 'VIEWER')", f, e.ids["viewer"])
			wantJSON(t, "transfer", e.asJSON(t, "owner", "POST", "/api/v1/folders/"+f+"/owner", `{"new_owner_user_id":"`+e.ids["viewer"]+`"}`), 200)
		},
	}
	for reason, do := range cases {
		f := e.newAlbum(t, "owner", reason)
		tok := wantJSON(t, "link", e.asJSON(t, "owner", "POST", "/api/v1/folders/"+f+"/invite-links", `{"role":"VIEWER"}`), 201)["token"].(string)
		do(f)
		wantGone(t, reason, e.redeem(t, "outsider", tok), "invite_link_gone", reason)
		wantGone(t, reason+" preview", e.linkCall(t, "", "", "POST", "/api/v1/invitations/link-preview", `{"token":"`+tok+`"}`), "invite_link_gone", reason)
	}
	// A restored album keeps its links revoked.
	f := e.newAlbum(t, "owner", "restored")
	tok := wantJSON(t, "link", e.asJSON(t, "owner", "POST", "/api/v1/folders/"+f+"/invite-links", `{"role":"VIEWER"}`), 201)["token"].(string)
	e.as(t, "owner", "DELETE", "/api/v1/folders/"+f, nil, nil)
	wantJSON(t, "restore", e.as(t, "owner", "POST", "/api/v1/folders/"+f+"/restore", nil, nil), 200)
	wantGone(t, "after restore", e.redeem(t, "outsider", tok), "invite_link_gone", "album_deleted")
}

// TestInviteLinkLastUseRace: many accounts redeem a link's last use at
// once; exactly one joins, the others get used_up, and the use count never
// exceeds max_uses.
func TestInviteLinkLastUseRace(t *testing.T) {
	e := newACLEnv(t)
	logins := []string{"outsider", "root"}
	tok := e.newLink(t, `{"role":"VIEWER","max_uses":1}`)["token"].(string)
	var wg sync.WaitGroup
	codes := make([]int, len(logins)*4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			login := logins[i%len(logins)]
			req := httptest.NewRequest("POST", "/api/v1/invitations/link-redeem", strings.NewReader(`{"token":"`+tok+`"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+e.tokens[login])
			req.RemoteAddr = fmt.Sprintf("10.58.0.%d:1", i+1)
			w := httptest.NewRecorder()
			e.Raw.ServeHTTP(w, req)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 409, 410: // the winner's other requests: already a member; the loser: used up
		default:
			t.Fatalf("codes %v", codes)
		}
	}
	if ok != 1 {
		t.Fatalf("%d joined: %v", ok, codes)
	}
	if n := e.count(t, "SELECT use_count FROM invite_links"); n != 1 {
		t.Fatalf("use_count %d", n)
	}
	if n := e.count(t, "SELECT count(*) FROM folder_members WHERE folder_id = ? AND user_id IN (?, ?)", e.family, e.ids["outsider"], e.ids["root"]); n != 1 {
		t.Fatalf("%d new members", n)
	}
}

// TestInviteLinkRateLimits: from one address, unknown tokens are refused
// after the budget (preview, landing page and redeem share the address
// budget), the account budget of redeem holds across addresses, and other
// addresses are not affected.
func TestInviteLinkRateLimits(t *testing.T) {
	e := newACLEnv(t)
	tok := e.newLink(t, `{"role":"VIEWER"}`)["token"].(string)
	unknown := func(i int) string { return fmt.Sprintf("%043d", i) }
	addr := "198.51.100.57:4000"
	got429 := false
	for i := range 12 {
		r := e.linkCall(t, "", addr, "POST", "/api/v1/invitations/link-preview", `{"token":"`+unknown(i)+`"}`)
		if r.Status == 429 {
			got429 = true
			if r.Header.Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
			break
		}
		if r.Status != 404 {
			t.Fatalf("unknown %d", r.Status)
		}
	}
	if !got429 {
		t.Fatal("no 429 after 12 unknown tokens from one address")
	}
	// The same address, now with the right token, and the landing page.
	if r := e.linkCall(t, "", addr, "POST", "/api/v1/invitations/link-preview", `{"token":"`+tok+`"}`); r.Status != 429 {
		t.Fatalf("blocked address: %d", r.Status)
	}
	if r := e.linkCall(t, "", addr, "GET", "/i/"+tok, ""); r.Status != 429 || !strings.Contains(string(r.Body), "잠시 후") {
		t.Fatalf("blocked landing: %d", r.Status)
	}
	// Another address is fine.
	wantJSON(t, "other address", e.linkCall(t, "", "", "POST", "/api/v1/invitations/link-preview", `{"token":"`+tok+`"}`), 200)
	// Redeem: 30 unknown tokens per account from any address.
	blocked := false
	for i := range 40 {
		r := e.linkCall(t, "outsider", "", "POST", "/api/v1/invitations/link-redeem", `{"token":"`+unknown(100+i)+`"}`)
		if r.Status == 429 {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("account not throttled after 40 unknown tokens")
	}
	if r := e.redeem(t, "outsider", tok); r.Status != 429 {
		t.Fatalf("throttled account redeems: %d", r.Status)
	}
	// Another account is not affected.
	wantJSON(t, "root redeems", e.redeem(t, "root", tok), 200)
}

// TestInviteLanding: the page of a usable link names the album, the
// inviter and the role and links to the app; others say the link cannot
// be used without album details; no script can run; it spends nothing;
// it is served over the funnel route.
func TestInviteLanding(t *testing.T) {
	e := newACLEnv(t)
	e.API.SetTLSInfo(&httpapi.TLSInfo{SPKISHA256: strings.Repeat("cd", 32), Pairing: []httpapi.Pairing{{URL: "https://192.0.2.10:8787"}}})
	e.exec(t, "UPDATE folders SET folder_name = ? WHERE folder_id = ?", `<b>"가족" & 'trip'</b>`, e.family)
	created := e.newLink(t, `{"role":"UPLOADER"}`)
	tok := created["token"].(string)
	page := func(h http.Handler, path string) reply {
		req := httptest.NewRequest("GET", path, nil)
		n := linkAddr.Add(1)
		req.RemoteAddr = fmt.Sprintf("10.59.%d.%d:5000", n/250%250, n%250+1)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return reply{Status: w.Code, Header: w.Header(), Body: w.Body.Bytes()}
	}
	for name, h := range map[string]http.Handler{"direct": e.Raw, "funnel": httpapi.RemoteOnly(httpapi.RouteFunnel, e.Raw), "relay": httpapi.RemoteOnly(httpapi.RouteRelay, e.Raw)} {
		r := page(h, "/i/"+tok)
		body := string(r.Body)
		if r.Status != 200 || r.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s: %d %s", name, r.Status, r.Header.Get("Content-Type"))
		}
		csp := r.Header.Get("Content-Security-Policy")
		if !strings.HasPrefix(csp, "default-src 'none'; style-src 'sha256-") || strings.Contains(csp, "script-src") || strings.Contains(csp, "unsafe") ||
			!strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "form-action 'none'") {
			t.Fatalf("CSP %q", csp)
		}
		for k, v := range map[string]string{"X-Robots-Tag": "noindex, nofollow", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
			if r.Header.Get(k) != v {
				t.Fatalf("%s: %q", k, r.Header.Get(k))
			}
		}
		if strings.Contains(body, "<script") || strings.Contains(body, `<b>"가족"`) || !strings.Contains(body, "&lt;b&gt;&#34;가족&#34; &amp; &#39;trip&#39;&lt;/b&gt;") ||
			!strings.Contains(body, "owner님이") || !strings.Contains(body, "업로드 전용") || !strings.Contains(body, "서버 관리자에게 계정을") {
			t.Fatalf("%s page:\n%s", name, body)
		}
		appLink := strings.ReplaceAll(created["app_link"].(string), "&", "&amp;")
		if !strings.Contains(body, `href="`+appLink+`"`) {
			t.Fatalf("%s: app link missing:\n%s", name, body)
		}
	}
	if n := e.count(t, "SELECT use_count FROM invite_links"); n != 0 {
		t.Fatalf("the page spent a use: %d", n)
	}
	// Unknown, malformed and unusable: no album details.
	e.exec(t, "UPDATE invite_links SET revoked_at = ?, revoke_reason = 'revoked_by_owner'", stamp(0))
	for path, status := range map[string]int{"/i/" + tok: 410, "/i/" + strings.Repeat("A", 43): 404, "/i/short": 404} {
		r := page(e.Raw, path)
		if r.Status != status || strings.Contains(string(r.Body), "가족") || strings.Contains(string(r.Body), "owner님") || !strings.Contains(string(r.Body), "쓸 수 없어요") {
			t.Fatalf("%s: %d\n%s", path, r.Status, r.Body)
		}
	}
	// Remote routes still refuse what is not theirs.
	if r := page(httpapi.RemoteOnly(httpapi.RouteFunnel, e.Raw), "/i/../api/v1/admin/users"); r.Status != 403 {
		t.Fatalf("non-canonical path over the funnel: %d", r.Status)
	}
}

// TestInviteLinkFeature: GET /me announces both features.
func TestInviteLinkFeature(t *testing.T) {
	e := newACLEnv(t)
	var me struct {
		Features []string `json:"server_features"`
	}
	if err := json.Unmarshal(e.as(t, "outsider", "GET", "/api/v1/me", nil, nil).Body, &me); err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(me.Features, ","); !strings.Contains(s, "invitation_history") || !strings.Contains(s, "invite_links") {
		t.Fatalf("features %v", me.Features)
	}
}
