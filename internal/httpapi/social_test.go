package httpapi_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
)

// Likes, comments and mentions (social.go) on the aclEnv folder "family":
// owner, contributor, viewer, uploader members, an outsider and the server
// administrator "root" who is not a member.

func (e *aclEnv) assetPath(asset string) string {
	return "/api/v1/folders/" + e.family + "/assets/" + asset
}

// comment writes a comment as login and returns it.
func (e *aclEnv) comment(t *testing.T, login, body string, mentions ...string) map[string]any {
	t.Helper()
	return wantJSON(t, "comment by "+login, e.asJSON(t, login, "POST", e.assetPath(e.photo)+"/comments", commentBody(t, body, mentions...)), 201)
}

func commentBody(t *testing.T, body string, mentions ...string) string {
	t.Helper()
	if mentions == nil {
		mentions = []string{}
	}
	return `{"body":` + mustJSON(t, body) + `,"mentions":` + mustJSON(t, mentions) + `}`
}

func (e *aclEnv) mention(login string) string { return "<@" + e.ids[login] + ">" }

// feed returns the events of family after seq (as the owner) and the
// assets of the page.
func (e *aclEnv) feed(t *testing.T, login string) ([]map[string]any, map[string]any) {
	t.Helper()
	m := wantJSON(t, "sync", e.as(t, login, "GET", "/api/v1/folders/"+e.family+"/sync?limit=500", nil, nil), 200)
	var events []map[string]any
	for _, ev := range m["events"].([]any) {
		events = append(events, ev.(map[string]any))
	}
	return events, m["assets"].(map[string]any)
}

func kinds(events []map[string]any) []string {
	var out []string
	for _, e := range events {
		out = append(out, e["kind"].(string))
	}
	return out
}

func (e *aclEnv) rows(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.Meta.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSocialRouteMatrix: readers (OWNER, CONTRIBUTOR, VIEWER) like,
// comment and see likes, comments and mentionable people; an UPLOADER is
// 403 everywhere, an outsider and the server administrator 404 (the
// administrator cannot read another album's comments). Edits are the
// author's; deletes the author's or the owner's.
func TestSocialRouteMatrix(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	readers := map[string]int{"owner": 200, "contributor": 200, "viewer": 200, "uploader": 403, "outsider": 404, "root": 404}
	created := map[string]int{"owner": 201, "contributor": 201, "viewer": 201, "uploader": 403, "outsider": 404, "root": 404}
	byContributor := e.comment(t, "contributor", "first")["id"].(string)
	ops := []struct {
		name, method, path, body string
		want                     map[string]int
	}{
		{"list likes", "GET", a + "/likes", "", readers},
		{"like", "PUT", a + "/likes/me", "", readers},
		{"unlike", "DELETE", a + "/likes/me", "", readers},
		{"list comments", "GET", a + "/comments", "", readers},
		{"comment", "POST", a + "/comments", `{"body":"hi"}`, created},
		{"mentionable", "GET", "/api/v1/folders/" + e.family + "/mentionable", "", readers},
		{"edit others", "PATCH", a + "/comments/" + byContributor, `{"body":"edited"}`,
			map[string]int{"owner": 403, "contributor": 200, "viewer": 403, "uploader": 403, "outsider": 404, "root": 404}},
	}
	for _, o := range ops {
		for _, login := range aclRoles {
			r := e.asJSON(t, login, o.method, o.path, o.body)
			want := o.want[login]
			switch want {
			case 403:
				wantError(t, o.name+" as "+login, r, 403, "forbidden")
			case 404:
				wantError(t, o.name+" as "+login, r, 404, "folder_not_found")
			default:
				wantJSON(t, o.name+" as "+login, r, want)
			}
		}
	}
	// Delete: a fresh comment of the contributor per attempt.
	del := map[string]int{"owner": 204, "contributor": 204, "viewer": 403, "uploader": 403, "outsider": 404, "root": 404}
	for _, login := range aclRoles {
		id := e.comment(t, "contributor", "to delete")["id"].(string)
		r := e.as(t, login, "DELETE", a+"/comments/"+id, nil, nil)
		if r.Status != del[login] {
			t.Fatalf("delete as %s: %d %s", login, r.Status, r.Body)
		}
	}
	// A viewer deletes its own comment.
	own := e.comment(t, "viewer", "mine")["id"].(string)
	if r := e.as(t, "viewer", "DELETE", a+"/comments/"+own, nil, nil); r.Status != 204 {
		t.Fatalf("viewer deletes own: %d %s", r.Status, r.Body)
	}
	// The activity route is the caller's own: root sees nothing of family.
	for _, login := range aclRoles {
		m := wantJSON(t, "activity "+login, e.as(t, login, "GET", "/api/v1/me/activity", nil, nil), 200)
		if login == "root" || login == "uploader" || login == "outsider" {
			if n := len(m["items"].([]any)); n != 0 {
				t.Fatalf("%s activity %v", login, m["items"])
			}
		}
	}
	// The administrator's own album works for it, and nobody else sees it.
	other := "/api/v1/folders/" + e.adminF + "/assets/" + e.other
	wantJSON(t, "root likes own", e.asJSON(t, "root", "PUT", other+"/likes/me", ""), 200)
	wantError(t, "owner on admin album", e.as(t, "owner", "GET", other+"/comments", nil, nil), 404, "folder_not_found")
	// An asset of another folder under this folder's path is unknown.
	wantError(t, "foreign asset", e.asJSON(t, "owner", "PUT", a[:strings.LastIndex(a, "/")+1]+e.other+"/likes/me", ""), 404, "asset_not_found")
	wantError(t, "unknown comment", e.asJSON(t, "contributor", "PATCH", a+"/comments/0123456789abcdef0123456789abcdef", `{"body":"x"}`), 404, "comment_not_found")
}

// TestLikes: liking is idempotent, counted per account, listed with display
// names, and summarized on the asset for each reader.
func TestLikes(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	st := wantJSON(t, "like", e.asJSON(t, "viewer", "PUT", a+"/likes/me", ""), 200)
	wantKeys(t, "like state", st, "folder_id", "asset_id", "like_count", "liked_by_me", "comment_count", "changed")
	if st["like_count"] != 1.0 || st["liked_by_me"] != true || st["changed"] != true {
		t.Fatalf("first like %v", st)
	}
	if st = wantJSON(t, "like again", e.asJSON(t, "viewer", "PUT", a+"/likes/me", ""), 200); st["changed"] != false || st["like_count"] != 1.0 {
		t.Fatalf("second like %v", st)
	}
	wantJSON(t, "owner likes", e.asJSON(t, "owner", "PUT", a+"/likes/me", ""), 200)
	l := wantJSON(t, "list", e.as(t, "contributor", "GET", a+"/likes", nil, nil), 200)
	wantKeys(t, "likes", l, "folder_id", "asset_id", "like_count", "liked_by_me", "likes")
	likes := l["likes"].([]any)
	if l["like_count"] != 2.0 || l["liked_by_me"] != false || len(likes) != 2 || likes[0].(map[string]any)["display_name"] != "viewer" {
		t.Fatalf("likes %v", l)
	}
	wantKeys(t, "like", likes[0].(map[string]any), "user_id", "display_name", "liked_at")
	events, assets := e.feed(t, "viewer")
	if got := strings.Join(kinds(events), ","); got != "asset_added,likes_updated,likes_updated" {
		t.Fatalf("events %s", got)
	}
	if _, ok := events[1]["comment_id"]; ok || events[1]["uploader_device_id"] != "" {
		t.Fatalf("likes event %v", events[1])
	}
	as := assets[e.photo].(map[string]any)
	if as["like_count"] != 2.0 || as["liked_by_me"] != true || as["comment_count"] != 0.0 {
		t.Fatalf("asset for viewer %v", as)
	}
	_, assets = e.feed(t, "contributor")
	if as := assets[e.photo].(map[string]any); as["liked_by_me"] != false || as["like_count"] != 2.0 {
		t.Fatalf("asset for contributor %v", as)
	}
	snap := e.snapshotAssets(t, "viewer")[e.photo]
	if snap["like_count"] != 2.0 || snap["liked_by_me"] != true {
		t.Fatalf("snapshot %v", snap)
	}
	if st = wantJSON(t, "unlike", e.asJSON(t, "viewer", "DELETE", a+"/likes/me", ""), 200); st["changed"] != true || st["like_count"] != 1.0 || st["liked_by_me"] != false {
		t.Fatalf("unlike %v", st)
	}
	if st = wantJSON(t, "unlike again", e.asJSON(t, "viewer", "DELETE", a+"/likes/me", ""), 200); st["changed"] != false {
		t.Fatalf("unlike again %v", st)
	}
	if n := e.events(t, e.family); n != 4 {
		t.Fatalf("%d events, want 4 (no event for unchanged likes)", n)
	}
	if e.audits(t, "asset.like") != 2 || e.audits(t, "asset.unlike") != 1 {
		t.Fatal("likes not audited")
	}
	// A disabled account's like is not counted or listed.
	if _, err := e.Meta.DB().Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", e.ids["owner"]); err == nil {
		t.Fatal("the owner could be disabled")
	}
	wantJSON(t, "contributor likes", e.asJSON(t, "contributor", "PUT", a+"/likes/me", ""), 200)
	if _, err := e.Meta.DB().Exec("UPDATE users SET status = 'disabled' WHERE user_id = ?", e.ids["contributor"]); err != nil {
		t.Fatal(err)
	}
	if l := wantJSON(t, "list", e.as(t, "viewer", "GET", a+"/likes", nil, nil), 200); l["like_count"] != 1.0 {
		t.Fatalf("disabled account counted: %v", l)
	}
}

// TestLikeConcurrency: concurrent toggles of one account are serialized:
// one like at most, and exactly one likes_updated event per change.
func TestLikeConcurrency(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	before := e.events(t, e.family)
	var mu sync.Mutex
	changes := 0
	var wg sync.WaitGroup
	for i := range 24 {
		login := []string{"owner", "contributor", "viewer"}[i%3]
		method := "PUT"
		if i%4 == 3 {
			method = "DELETE"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.asJSON(t, login, method, a+"/likes/me", "")
			if r.Status != 200 {
				t.Errorf("%s %s: %d %s", method, login, r.Status, r.Body)
				return
			}
			if r.object(t)["changed"] == true {
				mu.Lock()
				changes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got := e.events(t, e.family) - before; got != changes {
		t.Fatalf("%d events for %d changes", got, changes)
	}
	rows := e.rows(t, "SELECT count(*) FROM asset_likes WHERE folder_id = ? AND asset_id = ?", e.family, e.photo)
	if l := wantJSON(t, "list", e.as(t, "owner", "GET", a+"/likes", nil, nil), 200); l["like_count"] != float64(rows) || rows > 3 {
		t.Fatalf("like_count %v, rows %d", l["like_count"], rows)
	}
	// Twenty identical likes at once: one row, one event.
	wantJSON(t, "reset", e.asJSON(t, "viewer", "DELETE", a+"/likes/me", ""), 200)
	before = e.events(t, e.family)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.asJSON(t, "viewer", "PUT", a+"/likes/me", ""); r.Status != 200 {
				t.Errorf("like: %d %s", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	if got := e.events(t, e.family) - before; got != 1 {
		t.Fatalf("%d events for one like", got)
	}
	if n := e.rows(t, "SELECT count(*) FROM asset_likes WHERE user_id = ?", e.ids["viewer"]); n != 1 {
		t.Fatalf("%d like rows", n)
	}
}

// TestComments: writing, editing and deleting comments, their shape and
// their events; deleted comments vanish.
func TestComments(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	c := e.comment(t, "viewer", "  예쁘다\r\n정말  ")
	wantKeys(t, "comment", c, "id", "folder_id", "asset_id", "author", "body", "segments", "created_at", "edited_at", "is_mine", "can_edit", "can_delete")
	if c["body"] != "예쁘다\n정말" || c["edited_at"] != nil || c["is_mine"] != true || c["can_edit"] != true || c["can_delete"] != true {
		t.Fatalf("comment %v", c)
	}
	if au := c["author"].(map[string]any); au["display_name"] != "viewer" || au["user_id"] != e.ids["viewer"] {
		t.Fatalf("author %v", au)
	}
	id := c["id"].(string)
	// As others see it.
	l := wantJSON(t, "list as owner", e.as(t, "owner", "GET", a+"/comments", nil, nil), 200)
	wantKeys(t, "thread", l, "folder_id", "asset_id", "comments", "comment_count", "next_cursor", "has_more")
	oc := l["comments"].([]any)[0].(map[string]any)
	if oc["is_mine"] != false || oc["can_edit"] != false || oc["can_delete"] != true || l["comment_count"] != 1.0 {
		t.Fatalf("owner view %v", l)
	}
	cc := wantJSON(t, "list as contributor", e.as(t, "contributor", "GET", a+"/comments", nil, nil), 200)["comments"].([]any)[0].(map[string]any)
	if cc["can_delete"] != false || cc["can_edit"] != false {
		t.Fatalf("contributor view %v", cc)
	}
	// Edit by the author; the same text changes nothing.
	ed := wantJSON(t, "edit", e.asJSON(t, "viewer", "PATCH", a+"/comments/"+id, `{"body":"고쳤어"}`), 200)
	if ed["body"] != "고쳤어" || ed["edited_at"] == nil || ed["created_at"] != c["created_at"] {
		t.Fatalf("edited %v", ed)
	}
	n := e.events(t, e.family)
	wantJSON(t, "same edit", e.asJSON(t, "viewer", "PATCH", a+"/comments/"+id, `{"body":"고쳤어"}`), 200)
	if e.events(t, e.family) != n {
		t.Fatal("an unchanged edit appended an event")
	}
	// Paging.
	for i := range 4 {
		e.comment(t, "owner", fmt.Sprintf("more %d", i))
	}
	var seen []string
	cursor := ""
	for {
		p := wantJSON(t, "page", e.as(t, "viewer", "GET", a+"/comments?limit=2&cursor="+cursor, nil, nil), 200)
		for _, x := range p["comments"].([]any) {
			seen = append(seen, x.(map[string]any)["body"].(string))
		}
		if p["has_more"] != true {
			if p["next_cursor"] != "" {
				t.Fatalf("last page cursor %v", p["next_cursor"])
			}
			break
		}
		cursor = p["next_cursor"].(string)
	}
	if strings.Join(seen, "|") != "고쳤어|more 0|more 1|more 2|more 3" {
		t.Fatalf("pages %v", seen)
	}
	wantError(t, "bad cursor", e.as(t, "viewer", "GET", a+"/comments?cursor=***", nil, nil), 400, "invalid_cursor")
	wantError(t, "bad limit", e.as(t, "viewer", "GET", a+"/comments?limit=101", nil, nil), 400, "invalid_limit")
	// Delete: the comment vanishes (no tombstone).
	if r := e.as(t, "viewer", "DELETE", a+"/comments/"+id, nil, nil); r.Status != 204 {
		t.Fatalf("delete %d %s", r.Status, r.Body)
	}
	wantError(t, "delete again", e.as(t, "viewer", "DELETE", a+"/comments/"+id, nil, nil), 404, "comment_not_found")
	if n := e.rows(t, "SELECT count(*) FROM asset_comments WHERE comment_id = ?", id); n != 0 {
		t.Fatal("deleted comment kept")
	}
	events, assets := e.feed(t, "owner")
	var social []string
	for _, ev := range events {
		if k := ev["kind"].(string); strings.HasPrefix(k, "comment_") {
			social = append(social, k)
			if ev["comment_id"] == nil || ev["asset_id"] != e.photo {
				t.Fatalf("event %v", ev)
			}
		}
	}
	if strings.Join(social, ",") != "comment_added,comment_updated,comment_added,comment_added,comment_added,comment_added,comment_removed" {
		t.Fatalf("comment events %v", social)
	}
	if assets[e.photo].(map[string]any)["comment_count"] != 4.0 {
		t.Fatalf("comment_count %v", assets[e.photo])
	}
	if e.audits(t, "comment.create") != 5 || e.audits(t, "comment.edit") != 1 || e.audits(t, "comment.delete") != 1 {
		t.Fatal("comments not audited")
	}
	// The audit log never holds the text.
	if n := e.rows(t, "SELECT count(*) FROM audit_log WHERE details_json LIKE '%고쳤어%' OR details_json LIKE '%more%'"); n != 0 {
		t.Fatal("comment text in the audit log")
	}
	// The UPLOADER never receives the feed.
	wantError(t, "uploader sync", e.as(t, "uploader", "GET", "/api/v1/folders/"+e.family+"/sync", nil, nil), 403, "forbidden")
	wantError(t, "uploader snapshot", e.as(t, "uploader", "GET", "/api/v1/folders/"+e.family+"/assets", nil, nil), 403, "forbidden")
}

// TestCommentIdempotency: a retried creation with the same key returns the
// same comment and writes nothing.
func TestCommentIdempotency(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "queue-item-1"}
	first := wantJSON(t, "first", e.as(t, "viewer", "POST", a+"/comments", []byte(`{"body":"한 번"}`), hdr), 201)
	n := e.events(t, e.family)
	r := e.as(t, "viewer", "POST", a+"/comments", []byte(`{"body":"한 번"}`), hdr)
	again := wantJSON(t, "retry", r, 201)
	if again["id"] != first["id"] || r.Header.Get("Idempotency-Replayed") != "true" || e.events(t, e.family) != n {
		t.Fatalf("retry %v", again)
	}
	// Another account's key is its own.
	if o := wantJSON(t, "other account", e.as(t, "owner", "POST", a+"/comments", []byte(`{"body":"한 번"}`), hdr), 201); o["id"] == first["id"] {
		t.Fatal("keys of two accounts met")
	}
	second := wantJSON(t, "second photo", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/assets", jpegBytes(t, 9, 9), nil), 201)["asset_id"].(string)
	wantError(t, "key on another asset", e.as(t, "viewer", "POST", e.assetPath(second)+"/comments", []byte(`{"body":"한 번"}`), hdr), 422, "idempotency_key_reused")
	hdr["Idempotency-Key"] = "bad key"
	wantError(t, "bad key", e.as(t, "viewer", "POST", a+"/comments", []byte(`{"body":"x"}`), hdr), 400, "invalid_idempotency_key")
}

// TestCommentValidation: trimming, empty, the 1,000-character limit as
// shown (mentions counted by their names), control characters and the
// mention list.
func TestCommentValidation(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo) + "/comments"
	post := func(body string) reply { return e.asJSON(t, "contributor", "POST", a, body) }
	wantError(t, "missing body", post(`{}`), 400, "comment_empty")
	wantError(t, "blank", post(`{"body":" \n\t　 "}`), 400, "comment_empty")
	wantError(t, "control", post(`{"body":"a\u0007b"}`), 400, "invalid_comment")
	wantError(t, "not json", post(`{"body":`), 400, "invalid_json")
	wantError(t, "no content type", e.as(t, "contributor", "POST", a, []byte(`{"body":"x"}`), nil), 415, "json_required")
	full := strings.Repeat("가", 1000)
	if c := wantJSON(t, "1000", post(commentBody(t, "  "+full+"  ")), 201); c["body"] != full {
		t.Fatal("not trimmed")
	}
	wantError(t, "1001", post(commentBody(t, full+"나")), 400, "comment_too_long")
	// "@viewer" is 7 characters: 993 + 7 fits, 994 + 7 does not.
	wantJSON(t, "mention fits", post(commentBody(t, strings.Repeat("가", 993)+e.mention("viewer"), e.ids["viewer"])), 201)
	wantError(t, "mention too long", post(commentBody(t, strings.Repeat("가", 994)+e.mention("viewer"), e.ids["viewer"])), 400, "comment_too_long")
	// Mentions: the list and the tokens name the same readers.
	wantError(t, "token not listed", post(commentBody(t, "hi "+e.mention("viewer"))), 400, "invalid_mentions")
	wantError(t, "listed without token", post(commentBody(t, "hi", e.ids["viewer"])), 400, "invalid_mentions")
	wantError(t, "bad id", post(commentBody(t, "hi", "nothex")), 400, "invalid_mentions")
	wantError(t, "uploader", post(commentBody(t, "hi "+e.mention("uploader"), e.ids["uploader"])), 422, "mention_not_member")
	wantError(t, "outsider", post(commentBody(t, "hi "+e.mention("outsider"), e.ids["outsider"])), 422, "mention_not_member")
	wantError(t, "admin", post(commentBody(t, "hi "+e.mention("root"), e.ids["root"])), 422, "mention_not_member")
	var many []string
	body := "x"
	for i := range 21 {
		id := fmt.Sprintf("%032x", i)
		many = append(many, id)
		body += " <@" + id + ">"
	}
	wantError(t, "21 mentions", post(commentBody(t, body, many...)), 400, "too_many_mentions")
	// Repeated tokens and a repeated list entry are one mention.
	c := wantJSON(t, "repeat", post(commentBody(t, e.mention("viewer")+" "+e.mention("viewer"), e.ids["viewer"], e.ids["viewer"])), 201)
	if segs := c["segments"].([]any); len(segs) != 3 {
		t.Fatalf("segments %v", segs)
	}
	// Edits follow the same rules.
	id := c["id"].(string)
	wantError(t, "edit empty", e.asJSON(t, "contributor", "PATCH", a+"/"+id, `{"body":""}`), 400, "comment_empty")
	wantError(t, "edit mention uploader", e.asJSON(t, "contributor", "PATCH", a+"/"+id, commentBody(t, e.mention("uploader"), e.ids["uploader"])), 422, "mention_not_member")
	if n := e.rows(t, "SELECT count(*) FROM asset_comments"); n != 3 {
		t.Fatalf("%d comments stored", n)
	}
}

// TestMentions: mentions are stored by account, rendered with the current
// display name, and become plain text with the written name once the
// account cannot read the folder; authors who lost access are shown as
// no author.
func TestMentions(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	c := e.comment(t, "contributor", e.mention("viewer")+" 여기 어디야? "+e.mention("owner"), e.ids["owner"], e.ids["viewer"])
	segs := c["segments"].([]any)
	if len(segs) != 3 {
		t.Fatalf("segments %v", segs)
	}
	m0, m1, m2 := segs[0].(map[string]any), segs[1].(map[string]any), segs[2].(map[string]any)
	if m0["type"] != "mention" || m0["text"] != "@viewer" || m0["user_id"] != e.ids["viewer"] ||
		m1["type"] != "text" || m1["text"] != " 여기 어디야? " || m2["text"] != "@owner" {
		t.Fatalf("segments %v", segs)
	}
	id := c["id"].(string)
	// A new display name shows at once.
	if _, err := e.Meta.DB().Exec("UPDATE users SET display_name = '엄마' WHERE user_id = ?", e.ids["viewer"]); err != nil {
		t.Fatal(err)
	}
	thread := func(login string) map[string]any {
		return wantJSON(t, "thread", e.as(t, login, "GET", a+"/comments", nil, nil), 200)["comments"].([]any)[0].(map[string]any)
	}
	if got := thread("owner")["segments"].([]any)[0].(map[string]any)["text"]; got != "@엄마" {
		t.Fatalf("renamed mention %v", got)
	}
	// The viewer leaves the album: plain text with the name it had when
	// written, and its token is gone from the editable body.
	wantStatusReply(t, "remove viewer", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family+"/members/"+e.ids["viewer"], nil, nil), 204)
	got := thread("owner")
	if s := got["segments"].([]any); len(s) != 2 || s[0].(map[string]any)["text"] != "@viewer 여기 어디야? " || s[0].(map[string]any)["type"] != "text" {
		t.Fatalf("segments after removal %v", s)
	}
	if got["body"] != "@viewer 여기 어디야? "+e.mention("owner") {
		t.Fatalf("body after removal %q", got["body"])
	}
	// The author edits with the body as returned (one mention left).
	wantJSON(t, "edit", e.asJSON(t, "contributor", "PATCH", a+"/comments/"+id, commentBody(t, got["body"].(string)+"!", e.ids["owner"])), 200)
	// The author becomes upload-only: shown without author, cannot edit.
	e.setMember(t, "contributor", "UPLOADER")
	if got := thread("owner"); got["author"] != nil || got["can_delete"] != true {
		t.Fatalf("comment of an uploader %v", got)
	}
	wantError(t, "uploader edits", e.asJSON(t, "contributor", "PATCH", a+"/comments/"+id, `{"body":"x"}`), 403, "forbidden")
	// Mentionable people: the readers who can log in.
	e.setMember(t, "viewer", "VIEWER")
	pl := wantJSON(t, "mentionable", e.as(t, "viewer", "GET", "/api/v1/folders/"+e.family+"/mentionable", nil, nil), 200)
	var names []string
	for _, p := range pl["people"].([]any) {
		pm := p.(map[string]any)
		wantKeys(t, "person", pm, "user_id", "display_name", "is_me")
		names = append(names, fmt.Sprint(pm["display_name"], pm["is_me"]))
	}
	if strings.Join(names, ",") != "ownerfalse,엄마true" {
		t.Fatalf("mentionable %v", names)
	}
}

// TestSocialCleanup: what happens to likes and comments when a member is
// removed or loses read access, when the photo leaves the album, when the
// owner changes and when the album is archived.
func TestSocialCleanup(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo)
	f := "/api/v1/folders/" + e.family
	for _, login := range []string{"owner", "contributor", "viewer"} {
		wantJSON(t, "like", e.asJSON(t, login, "PUT", a+"/likes/me", ""), 200)
	}
	byViewer := e.comment(t, "viewer", "viewer was here")["id"].(string)
	// Removal: the likes go (with an event), the comment stays.
	before := e.events(t, e.family)
	wantStatusReply(t, "remove", e.as(t, "owner", "DELETE", f+"/members/"+e.ids["viewer"], nil, nil), 204)
	if n := e.rows(t, "SELECT count(*) FROM asset_likes WHERE user_id = ?", e.ids["viewer"]); n != 0 {
		t.Fatal("removed member's like kept")
	}
	events, assets := e.feed(t, "owner")
	if e.events(t, e.family) != before+1 || events[len(events)-1]["kind"] != "likes_updated" || assets[e.photo].(map[string]any)["like_count"] != 2.0 {
		t.Fatalf("after removal %v", kinds(events))
	}
	var details string
	if err := e.Meta.DB().QueryRow("SELECT details_json FROM audit_log WHERE action = 'member.remove'").Scan(&details); err != nil || !strings.Contains(details, `"likes_removed":1`) {
		t.Fatalf("audit %s %v", details, err)
	}
	if c := wantJSON(t, "thread", e.as(t, "owner", "GET", a+"/comments", nil, nil), 200)["comments"].([]any); len(c) != 1 || c[0].(map[string]any)["author"] != nil {
		t.Fatalf("thread after removal %v", c)
	}
	// Becoming upload-only drops the likes too; they do not come back.
	wantJSON(t, "to uploader", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["contributor"], `{"role":"UPLOADER"}`), 200)
	wantJSON(t, "back", e.asJSON(t, "owner", "PATCH", f+"/members/"+e.ids["contributor"], `{"role":"CONTRIBUTOR"}`), 200)
	if l := wantJSON(t, "likes", e.as(t, "owner", "GET", a+"/likes", nil, nil), 200); l["like_count"] != 1.0 {
		t.Fatalf("likes after role change %v", l)
	}
	// Transfer: the new owner may delete others' comments, the previous
	// owner (now a CONTRIBUTOR) no longer.
	byOwner := e.comment(t, "owner", "owner's")["id"].(string)
	wantJSON(t, "transfer", e.asJSON(t, "owner", "POST", f+"/owner", `{"new_owner_user_id":"`+e.ids["contributor"]+`"}`), 200)
	wantError(t, "old owner deletes", e.as(t, "owner", "DELETE", a+"/comments/"+byViewer, nil, nil), 403, "forbidden")
	wantStatusReply(t, "new owner deletes", e.as(t, "contributor", "DELETE", a+"/comments/"+byViewer, nil, nil), 204)
	// The photo leaves the album: its likes and comments are deleted.
	wantStatusReply(t, "remove photo", e.as(t, "contributor", "DELETE", a, nil, nil), 204)
	if n := e.rows(t, "SELECT count(*) FROM asset_likes WHERE asset_id = ?", e.photo) + e.rows(t, "SELECT count(*) FROM asset_comments WHERE asset_id = ?", e.photo); n != 0 {
		t.Fatalf("%d social rows of a removed photo", n)
	}
	if e.rows(t, "SELECT count(*) FROM comment_mentions") != 0 {
		t.Fatal("mentions of deleted comments kept")
	}
	wantError(t, "comments of removed", e.as(t, "owner", "GET", a+"/comments", nil, nil), 404, "asset_not_found")
	wantError(t, "like removed", e.asJSON(t, "owner", "PUT", a+"/likes/me", ""), 404, "asset_not_found")
	_ = byOwner
	// Archive: everything stays stored and nothing is reachable.
	second := wantJSON(t, "second photo", e.as(t, "owner", "POST", f+"/assets", jpegBytes(t, 9, 9), nil), 201)["asset_id"].(string)
	wantJSON(t, "comment", e.asJSON(t, "contributor", "POST", e.assetPath(second)+"/comments", commentBody(t, "hey "+e.mention("owner"), e.ids["owner"])), 201)
	if n := len(wantJSON(t, "activity", e.as(t, "owner", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any)); n != 1 {
		t.Fatalf("%d activity items before archive", n)
	}
	wantStatusReply(t, "archive", e.as(t, "contributor", "DELETE", f, nil, nil), 204)
	wantError(t, "archived", e.as(t, "owner", "GET", e.assetPath(second)+"/comments", nil, nil), 404, "folder_not_found")
	if n := len(wantJSON(t, "activity", e.as(t, "owner", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any)); n != 0 {
		t.Fatalf("%d activity items of an archived album", n)
	}
	if e.rows(t, "SELECT count(*) FROM asset_comments WHERE asset_id = ?", second) != 1 {
		t.Fatal("archive deleted comments")
	}
}

// TestActivity: mentions of me and others' comments and likes on my
// uploads, newest first, paged, only in albums I can read now.
func TestActivity(t *testing.T) {
	e := newACLEnv(t)
	a := e.assetPath(e.photo) // uploaded by the owner
	byContributor := wantJSON(t, "contributor upload", e.as(t, "contributor", "POST", "/api/v1/folders/"+e.family+"/assets", jpegBytes(t, 11, 11), nil), 201)["asset_id"].(string)
	e.comment(t, "contributor", "on owner's photo")
	time.Sleep(2 * time.Millisecond)
	wantJSON(t, "viewer likes", e.asJSON(t, "viewer", "PUT", a+"/likes/me", ""), 200)
	time.Sleep(2 * time.Millisecond)
	wantJSON(t, "owner likes own", e.asJSON(t, "owner", "PUT", a+"/likes/me", ""), 200)
	e.comment(t, "owner", "my own")
	time.Sleep(2 * time.Millisecond)
	wantJSON(t, "mention on another photo", e.asJSON(t, "viewer", "POST", e.assetPath(byContributor)+"/comments",
		commentBody(t, e.mention("owner")+" 봤어?", e.ids["owner"])), 201)
	time.Sleep(2 * time.Millisecond)
	// A mention on the owner's own photo is one mention item.
	e.comment(t, "viewer", e.mention("owner")+" 그리고", e.ids["owner"])
	act := func(login, q string) map[string]any {
		return wantJSON(t, "activity "+login, e.as(t, login, "GET", "/api/v1/me/activity"+q, nil, nil), 200)
	}
	m := act("owner", "")
	wantKeys(t, "activity", m, "items", "next_cursor", "has_more")
	var got []string
	for _, it := range m["items"].([]any) {
		im := it.(map[string]any)
		wantKeys(t, "item", im, "id", "type", "folder_id", "folder_name", "asset_id", "actor", "created_at", "comment")
		got = append(got, im["type"].(string)+":"+im["actor"].(map[string]any)["display_name"].(string))
		if im["folder_name"] != "family" {
			t.Fatalf("item %v", im)
		}
		if im["type"] == "like" && im["comment"] != nil || im["type"] != "like" && im["comment"] == nil {
			t.Fatalf("item %v", im)
		}
	}
	if strings.Join(got, ",") != "mention:viewer,mention:viewer,like:viewer,comment:contributor" {
		t.Fatalf("owner activity %v", got)
	}
	first := m["items"].([]any)[0].(map[string]any)
	if c := first["comment"].(map[string]any); c["segments"].([]any)[0].(map[string]any)["text"] != "@owner" || c["can_delete"] != true {
		t.Fatalf("activity comment %v", c)
	}
	// Paging one at a time gives the same order.
	var paged []string
	cursor := ""
	for range 10 {
		p := act("owner", "?limit=1&cursor="+cursor)
		for _, it := range p["items"].([]any) {
			paged = append(paged, it.(map[string]any)["id"].(string))
		}
		if p["has_more"] != true {
			break
		}
		cursor = p["next_cursor"].(string)
	}
	if len(paged) != 4 || paged[0] != first["id"] {
		t.Fatalf("paged %v", paged)
	}
	// The contributor: a mention on its photo by the viewer is a comment on
	// its upload... and a mention of the owner, not of it.
	if items := act("contributor", "")["items"].([]any); len(items) != 1 || items[0].(map[string]any)["type"] != "comment" {
		t.Fatalf("contributor activity %v", items)
	}
	for _, login := range []string{"uploader", "outsider", "root"} {
		if n := len(act(login, "")["items"].([]any)); n != 0 {
			t.Fatalf("%s activity %d", login, n)
		}
	}
	wantError(t, "bad cursor", e.as(t, "owner", "GET", "/api/v1/me/activity?cursor=***", nil, nil), 400, "invalid_cursor")
	// The viewer leaves: its like disappears from the owner's activity,
	// its comments stay without actor.
	wantStatusReply(t, "remove viewer", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family+"/members/"+e.ids["viewer"], nil, nil), 204)
	items := act("owner", "")["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["actor"] != nil {
		t.Fatalf("after removal %v", items)
	}
	// A member who lost read access sees none of the album's activity.
	e.setMember(t, "contributor", "UPLOADER")
	if n := len(act("contributor", "")["items"].([]any)); n != 0 {
		t.Fatalf("uploader activity %d", n)
	}
}

// TestSocialRateLimit: likes and comments are limited per account.
func TestSocialRateLimit(t *testing.T) {
	e := newACLEnv(t)
	var mu sync.Mutex
	clock := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	httpapi.SetSocialClock(e.API, func() time.Time { mu.Lock(); defer mu.Unlock(); return clock })
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	a := e.assetPath(e.photo)
	for i := range 60 {
		method := []string{"PUT", "DELETE"}[i%2]
		wantJSON(t, "like", e.asJSON(t, "viewer", method, a+"/likes/me", ""), 200)
	}
	r := e.asJSON(t, "viewer", "PUT", a+"/likes/me", "")
	wantError(t, "61st like", r, 429, "rate_limited")
	if r.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After %q", r.Header.Get("Retry-After"))
	}
	// Another account has its own budget.
	wantJSON(t, "owner like", e.asJSON(t, "owner", "PUT", a+"/likes/me", ""), 200)
	advance(time.Second)
	wantJSON(t, "after a second", e.asJSON(t, "viewer", "PUT", a+"/likes/me", ""), 200)
	for i := range 20 {
		e.comment(t, "contributor", fmt.Sprint("c", i))
	}
	r = e.asJSON(t, "contributor", "POST", a+"/comments", `{"body":"too many"}`)
	wantError(t, "21st comment", r, 429, "rate_limited")
	if r.Header.Get("Retry-After") != "3" {
		t.Fatalf("Retry-After %q", r.Header.Get("Retry-After"))
	}
	// Likes and comments are separate budgets; reads are not limited.
	wantJSON(t, "like", e.asJSON(t, "contributor", "PUT", a+"/likes/me", ""), 200)
	wantJSON(t, "read", e.as(t, "contributor", "GET", a+"/comments", nil, nil), 200)
	advance(3 * time.Second)
	e.comment(t, "contributor", "later")
	if n := e.rows(t, "SELECT count(*) FROM asset_comments"); n != 21 {
		t.Fatalf("%d comments", n)
	}
}
