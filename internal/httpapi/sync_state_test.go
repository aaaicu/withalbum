package httpapi_test

import (
	"sort"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/meta"
)

// GET /api/v1/sync/state (M53): every readable folder's cursor in one
// request, 304 while nothing changed, a long poll and the content digest.

func (e *aclEnv) syncState(t *testing.T, login, query, tag string) reply {
	t.Helper()
	hdr := map[string]string{}
	if tag != "" {
		hdr["If-None-Match"] = `"` + tag + `"`
	}
	return e.as(t, login, "GET", "/api/v1/sync/state"+query, nil, hdr)
}

func stateFolders(t *testing.T, m map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, f := range m["folders"].([]any) {
		f := f.(map[string]any)
		out[f["folder_id"].(string)] = f
	}
	return out
}

// snapshotDigest computes the digest a client would from the snapshot.
func (e *aclEnv) snapshotDigest(t *testing.T, login, folder string) (string, int) {
	t.Helper()
	m := wantJSON(t, "snapshot", e.as(t, login, "GET", "/api/v1/folders/"+folder+"/assets?limit=500", nil, nil), 200)
	list := m["assets"].([]any)
	sort.Slice(list, func(i, j int) bool {
		return list[i].(map[string]any)["id"].(string) < list[j].(map[string]any)["id"].(string)
	})
	b := meta.NewDigestBuilder()
	for _, a := range list {
		a := a.(map[string]any)
		b.Add(a["id"].(string), a["sha256"].(string), int(a["like_count"].(float64)), a["liked_by_me"] == true, int(a["comment_count"].(float64)))
	}
	return b.Sum(), b.Count()
}

func TestSyncStateListsReadableFoldersWithFeedCursor(t *testing.T) {
	e := newACLEnv(t)
	for _, login := range []string{"owner", "contributor", "viewer"} {
		r := e.syncState(t, login, "", "")
		m := wantJSON(t, login, r, 200)
		wantKeys(t, login, m, "tag", "albums_tag", "folders")
		if r.Header.Get("ETag") != `"`+m["tag"].(string)+`"` || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s headers %v", login, r.Header)
		}
		fs := stateFolders(t, m)
		if len(fs) != 1 || fs[e.family] == nil {
			t.Fatalf("%s folders %v", login, m["folders"])
		}
		wantKeys(t, login+" folder", fs[e.family], "folder_id", "cursor")
		// The cursor is where the feed ends for this caller.
		end := e.pull(t, login, e.family, "", feed{}, 100)
		if fs[e.family]["cursor"] != end {
			t.Fatalf("%s cursor %v, feed end %v", login, fs[e.family]["cursor"], end)
		}
	}
	// An UPLOADER's album is in albums_tag but has no cursor; others see nothing.
	for _, login := range []string{"uploader", "outsider"} {
		m := wantJSON(t, login, e.syncState(t, login, "", ""), 200)
		if len(m["folders"].([]any)) != 0 {
			t.Fatalf("%s folders %v", login, m["folders"])
		}
	}
	up := wantJSON(t, "uploader", e.syncState(t, "uploader", "", ""), 200)
	out := wantJSON(t, "outsider", e.syncState(t, "outsider", "", ""), 200)
	if up["albums_tag"] == out["albums_tag"] {
		t.Fatal("uploader's album missing from albums_tag")
	}
}

func TestSyncStateNotModifiedUntilAChange(t *testing.T) {
	e := newACLEnv(t)
	m := wantJSON(t, "first", e.syncState(t, "viewer", "", ""), 200)
	tag := m["tag"].(string)
	if r := e.syncState(t, "viewer", "", tag); r.Status != 304 || r.Header.Get("ETag") != `"`+tag+`"` || len(r.Body) != 0 {
		t.Fatalf("unchanged: %d %v %q", r.Status, r.Header, r.Body)
	}
	// A like moves the cursor (comment and like events are in the feed).
	wantJSON(t, "like", e.as(t, "contributor", "PUT", "/api/v1/folders/"+e.family+"/assets/"+e.photo+"/likes/me", nil, nil), 200)
	m2 := wantJSON(t, "after like", e.syncState(t, "viewer", "", tag), 200)
	if m2["tag"] == tag || stateFolders(t, m2)[e.family]["cursor"] == stateFolders(t, m)[e.family]["cursor"] {
		t.Fatalf("like not seen: %v -> %v", m, m2)
	}
	if m2["albums_tag"] != m["albums_tag"] {
		t.Fatal("albums_tag moved without an album change")
	}
	// An album setting changes albums_tag.
	wantJSON(t, "setting", e.asJSON(t, "owner", "PATCH", "/api/v1/folders/"+e.family, `{"hide_location":true}`), 200)
	m3 := wantJSON(t, "after setting", e.syncState(t, "viewer", "", m2["tag"].(string)), 200)
	if m3["albums_tag"] == m2["albums_tag"] {
		t.Fatal("setting not in albums_tag")
	}
}

func TestSyncStateDigestMatchesSnapshot(t *testing.T) {
	e := newACLEnv(t)
	e.upload(t, "contributor", e.family, jpegBytes(t, 11, 9), "k-digest-1")
	wantJSON(t, "like", e.as(t, "owner", "PUT", "/api/v1/folders/"+e.family+"/assets/"+e.photo+"/likes/me", nil, nil), 200)
	wantJSON(t, "comment", e.as(t, "viewer", "POST", "/api/v1/folders/"+e.family+"/assets/"+e.photo+"/comments",
		[]byte(`{"body":"좋다"}`), map[string]string{idemKey: "c-digest-1", "Content-Type": "application/json"}), 201)
	digests := map[string]string{}
	for _, login := range []string{"owner", "viewer"} {
		m := wantJSON(t, login, e.syncState(t, login, "?digests=1", ""), 200)
		f := stateFolders(t, m)[e.family]
		wantKeys(t, login+" folder", f, "folder_id", "cursor", "digest", "count")
		want, n := e.snapshotDigest(t, login, e.family)
		if f["digest"] != want || int(f["count"].(float64)) != n || n != 2 {
			t.Fatalf("%s digest %v/%v, snapshot %v/%d", login, f["digest"], f["count"], want, n)
		}
		digests[login] = want
		// Without digests=1 the same state, without the fields.
		plain := stateFolders(t, wantJSON(t, login, e.syncState(t, login, "", ""), 200))[e.family]
		if plain["digest"] != nil || plain["cursor"] != f["cursor"] || m["tag"] == nil {
			t.Fatalf("%s plain %v", login, plain)
		}
	}
	// liked_by_me is the caller's own: the owner's digest differs.
	if digests["owner"] == digests["viewer"] {
		t.Fatal("digest does not depend on the caller's like")
	}
}

func TestSyncStateLongPoll(t *testing.T) {
	e := newACLEnv(t)
	tag := wantJSON(t, "first", e.syncState(t, "viewer", "", ""), 200)["tag"].(string)

	start := time.Now()
	if r := e.syncState(t, "viewer", "?wait=1", tag); r.Status != 304 {
		t.Fatalf("quiet wait: %d", r.Status)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("returned after %v without waiting", d)
	}

	done := make(chan reply, 1)
	go func() { done <- e.syncState(t, "viewer", "?wait=10", tag) }()
	time.Sleep(300 * time.Millisecond)
	e.upload(t, "contributor", e.family, jpegBytes(t, 13, 5), "k-poll-1")
	select {
	case r := <-done:
		m := wantJSON(t, "woken", r, 200)
		if m["tag"] == tag {
			t.Fatal("same tag after a change")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll not woken by the upload")
	}
}

func TestSyncStateBadParameters(t *testing.T) {
	e := newACLEnv(t)
	for q, code := range map[string]string{
		"?wait=26": "invalid_wait", "?wait=-1": "invalid_wait", "?wait=x": "invalid_wait",
		"?digests=2": "invalid_digests",
	} {
		if m := wantJSON(t, q, e.syncState(t, "viewer", q, ""), 400); m["error"] != code {
			t.Fatalf("%s: %v", q, m)
		}
	}
}

func TestSyncStateDigestFollowsTheCallersView(t *testing.T) {
	e := newACLEnv(t)
	e.upload(t, "contributor", e.family, jpegBytes(t, 17, 3), "k-view-1")
	wantJSON(t, "hide", e.asJSON(t, "owner", "PATCH", "/api/v1/folders/"+e.family+"/assets/"+e.photo, `{"hidden":true}`), 200)
	for login, n := range map[string]int{"owner": 2, "contributor": 2, "viewer": 1} {
		f := stateFolders(t, wantJSON(t, login, e.syncState(t, login, "?digests=1", ""), 200))[e.family]
		want, got := e.snapshotDigest(t, login, e.family)
		if int(f["count"].(float64)) != n || got != n || f["digest"] != want {
			t.Fatalf("%s: %v/%v, snapshot %v/%d", login, f["digest"], f["count"], want, got)
		}
		if _, end := e.snapshot(t, login, e.family, 100); f["cursor"] != end {
			t.Fatalf("%s cursor %v, snapshot cursor %v", login, f["cursor"], end)
		}
	}
}
