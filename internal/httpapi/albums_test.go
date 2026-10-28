package httpapi_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pbt "github.com/aaaicu/withalbum/internal/blobstore/playbacktest"
	geo "github.com/aaaicu/withalbum/internal/capturetime/geotest"
	"github.com/aaaicu/withalbum/internal/integrity"
	"github.com/aaaicu/withalbum/internal/meta"
)

// Album management (M51, albums.go): hidden assets on every read path, the
// album trash, deleting / restoring / purging albums, the member list and
// the "my likes" filter.

// local is a client's index of one folder: asset id -> the asset as last
// received.
type local map[string]map[string]any

// apply reads the feed of folder as login from cursor to the end, like a
// client that knows every kind: asset_removed drops the asset, every other
// kind stores the asset if the page carries it. It checks that seq numbers
// follow each other and returns the next cursor and every raw page.
func (e *aclEnv) apply(t *testing.T, login, folder, cursor string, idx local) (string, []string) {
	t.Helper()
	var pages []string
	var last float64 = -1
	for {
		path := fmt.Sprintf("/api/v1/folders/%s/sync?limit=7", folder)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := e.as(t, login, "GET", path, nil, nil)
		m := wantJSON(t, "sync as "+login, r, 200)
		pages = append(pages, string(r.Body))
		assets := m["assets"].(map[string]any)
		for _, ev := range m["events"].([]any) {
			ev := ev.(map[string]any)
			seq := ev["seq"].(float64)
			if last >= 0 && seq != last+1 {
				t.Fatalf("%s: seq %v after %v", login, seq, last)
			}
			last = seq
			id := ev["asset_id"].(string)
			if ev["kind"] == "asset_removed" {
				delete(idx, id)
				continue
			}
			if a, ok := assets[id].(map[string]any); ok {
				idx[id] = a
			}
		}
		cursor = m["next_cursor"].(string)
		if m["has_more"] != true {
			return cursor, pages
		}
	}
}

// ids returns the sorted keys of a snapshot or an index.
func ids[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// snap is the snapshot as login sees it, by asset id.
func (e *aclEnv) snap(t *testing.T, login, folder string) map[string]map[string]any {
	t.Helper()
	m := wantJSON(t, "snapshot", e.as(t, login, "GET", "/api/v1/folders/"+folder+"/assets?limit=500", nil, nil), 200)
	out := map[string]map[string]any{}
	for _, a := range m["assets"].([]any) {
		am := a.(map[string]any)
		out[am["id"].(string)] = am
	}
	return out
}

func (e *aclEnv) hide(t *testing.T, login, asset string, hidden bool) map[string]any {
	t.Helper()
	m := wantJSON(t, "hide", e.asJSON(t, login, "PATCH", e.assetPath(asset), fmt.Sprintf(`{"hidden":%v}`, hidden)), 200)
	if m["hidden"] != hidden || m["id"] != asset {
		t.Fatalf("hide reply %v", m)
	}
	return m
}

// TestHiddenAssetsVisibility: a VIEWER gets nothing of a hidden asset on
// any read path (snapshot, my likes, check, feed, original, preview,
// component, playback copy, likes, comments, activity, location), and
// cannot write to it; the OWNER and CONTRIBUTORs see it marked hidden.
// Showing it again brings it back to the VIEWER through the feed, and a
// role change between the two views invalidates the stored cursor.
func TestHiddenAssetsVisibility(t *testing.T) {
	e, _, _ := newPlaybackEnv(t)
	f := "/api/v1/folders/" + e.family
	movie := e.uploadMovie(t, "contributor", e.family, pbt.Movie(1920, 1080, 30_000, 3), "clip.mov")
	live := wantJSON(t, "live", e.uploadLive(t, "owner", e.family, "live-h", newLive(liveID, 21)), 201)["asset_id"].(string)
	geoPic := wantJSON(t, "geo", e.as(t, "owner", "POST", f+"/assets", geo.JPEG(geo.SampleLat, geo.SampleLon, 4.5, 31), map[string]string{"X-Filename": "geo.jpg"}), 201)["asset_id"].(string)
	processCaptures(t, e)
	processPlayback(t, e)
	hidden := []string{movie, live, geoPic}

	// Social on geoPic (uploaded by the owner) before it is hidden.
	wantJSON(t, "viewer likes", e.asJSON(t, "viewer", "PUT", e.assetPath(geoPic)+"/likes/me", ""), 200)
	viewerComment := wantJSON(t, "viewer comments", e.asJSON(t, "viewer", "POST", e.assetPath(geoPic)+"/comments", commentBody(t, "nice "+e.mention("contributor"), e.ids["contributor"])), 201)["id"].(string)
	wantJSON(t, "contributor mentions viewer", e.asJSON(t, "contributor", "POST", e.assetPath(geoPic)+"/comments", commentBody(t, "look "+e.mention("viewer"), e.ids["viewer"])), 201)
	if n := len(wantJSON(t, "activity", e.as(t, "viewer", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any)); n != 1 {
		t.Fatalf("viewer activity before hiding: %d items", n)
	}

	// Everything an asset is known by: its id, the hashes of its originals.
	all := e.snap(t, "owner", e.family)
	var secrets []string
	for _, id := range hidden {
		a := all[id]
		secrets = append(secrets, id, a["sha256"].(string))
		for _, c := range asSlice(a["components"]) {
			secrets = append(secrets, c.(map[string]any)["sha256"].(string))
		}
	}
	if all[movie]["playback"].(map[string]any)["status"] != "ready" || all[geoPic]["location"] == nil {
		t.Fatalf("setup: movie %v geo %v", all[movie]["playback"], all[geoPic]["location"])
	}

	viewerCursor, _ := e.apply(t, "viewer", e.family, "", local{})
	ownerCursor, _ := e.apply(t, "owner", e.family, "", local{})
	if strings.SplitN(viewerCursor, ".", 2)[0] == strings.SplitN(ownerCursor, ".", 2)[0] {
		t.Fatal("the VIEWER and the OWNER share a feed epoch")
	}

	// Only the OWNER and CONTRIBUTORs hide.
	for login, want := range map[string]int{"viewer": 403, "uploader": 403, "outsider": 404, "root": 404} {
		if r := e.asJSON(t, login, "PATCH", e.assetPath(geoPic), `{"hidden":true}`); r.Status != want {
			t.Fatalf("hide as %s: %d %s", login, r.Status, r.Body)
		}
	}
	wantError(t, "no field", e.asJSON(t, "owner", "PATCH", e.assetPath(geoPic), `{}`), 400, "invalid_request")
	wantError(t, "unknown asset", e.asJSON(t, "owner", "PATCH", e.assetPath(strings.Repeat("0", 32)), `{"hidden":true}`), 404, "asset_not_found")
	before := e.events(t, e.family)
	e.hide(t, "contributor", movie, true)
	e.hide(t, "contributor", live, true)
	e.hide(t, "owner", geoPic, true)
	e.hide(t, "owner", geoPic, true) // unchanged: no event
	if got := e.events(t, e.family); got != before+3 {
		t.Fatalf("events %d -> %d, want 3", before, got)
	}
	if e.audits(t, "asset.hide") != 3 {
		t.Fatal("hiding not audited")
	}

	// The VIEWER: every read path, raw bodies collected for the leak check.
	var bodies []string
	get := func(what, method, path, body string, status int, code string) {
		t.Helper()
		r := e.asJSON(t, "viewer", method, path, body)
		if method == "GET" {
			r = e.as(t, "viewer", method, path, nil, nil)
		}
		if r.Status != status {
			t.Fatalf("viewer %s: %d %s, want %d", what, r.Status, r.Body, status)
		}
		if code != "" {
			wantError(t, "viewer "+what, r, status, code)
		}
		bodies = append(bodies, string(r.Body))
	}
	get("snapshot", "GET", f+"/assets", "", 200, "")
	get("my likes", "GET", f+"/assets?liked_by_me=true", "", 200, "")
	var shas []string
	for _, id := range hidden {
		shas = append(shas, all[id]["sha256"].(string))
	}
	get("check", "POST", f+"/assets/check", mustJSON(t, map[string]any{"sha256": shas}), 200, "")
	get("folder", "GET", f, "", 200, "")
	get("folders", "GET", "/api/v1/folders", "", 200, "")
	get("activity", "GET", "/api/v1/me/activity", "", 200, "")
	get("mentionable", "GET", f+"/mentionable", "", 200, "")
	get("members", "GET", f+"/members", "", 200, "")
	for _, id := range hidden {
		p := e.assetPath(id)
		get("original", "GET", p+"/original", "", 404, "asset_not_found")
		get("preview", "GET", p+"/preview", "", 404, "asset_not_found")
		get("component", "GET", p+"/components/paired_video", "", 404, "asset_not_found")
		get("playback", "GET", p+"/playback", "", 404, "asset_not_found")
		get("likes", "GET", p+"/likes", "", 404, "asset_not_found")
		get("comments", "GET", p+"/comments", "", 404, "asset_not_found")
		get("like", "PUT", p+"/likes/me", "", 404, "asset_not_found")
		get("unlike", "DELETE", p+"/likes/me", "", 404, "asset_not_found")
		get("comment", "POST", p+"/comments", commentBody(t, "hi"), 404, "asset_not_found")
		get("delete", "DELETE", p, "", 403, "forbidden")
	}
	get("edit own comment", "PATCH", e.assetPath(geoPic)+"/comments/"+viewerComment, commentBody(t, "edited"), 404, "asset_not_found")
	get("delete own comment", "DELETE", e.assetPath(geoPic)+"/comments/"+viewerComment, "", 404, "asset_not_found")
	for _, b := range bodies {
		for _, s := range secrets {
			if strings.Contains(b, s) {
				t.Fatalf("a VIEWER read names hidden %s: %s", s, b)
			}
		}
	}
	if s := e.snap(t, "viewer", e.family); len(s) != 1 || s[e.photo] == nil {
		t.Fatalf("viewer snapshot %v", ids(s))
	}
	// The feed: from the start and from the cursor before hiding, the
	// hidden assets are only ever asset_removed, without data.
	for _, from := range []string{"", viewerCursor} {
		idx := local{}
		if from != "" {
			idx = local{e.photo: {}, movie: {}, live: {}, geoPic: {}}
		}
		next, pages := e.apply(t, "viewer", e.family, from, idx)
		if got := ids(idx); !slices.Equal(got, []string{e.photo}) {
			t.Fatalf("viewer feed from %q ends with %v", from, got)
		}
		for _, p := range pages {
			var page struct {
				Events []meta.Event `json:"events"`
			}
			if err := json.Unmarshal([]byte(p), &page); err != nil {
				t.Fatal(err)
			}
			for _, ev := range page.Events {
				if slices.Contains(hidden, ev.AssetID) && (ev.Kind != "asset_removed" || ev.CommentID != "" || ev.UploaderDeviceID != "") {
					t.Fatalf("viewer event of a hidden asset: %+v", ev)
				}
			}
			for _, s := range secrets[1:] {
				if len(s) == 64 && strings.Contains(p, s) {
					t.Fatalf("viewer feed carries a hidden hash")
				}
			}
		}
		viewerCursor = next
	}
	if act := wantJSON(t, "activity", e.as(t, "viewer", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any); len(act) != 0 {
		t.Fatalf("viewer activity on hidden photos: %v", act)
	}

	// The OWNER and the CONTRIBUTOR see everything, marked hidden.
	for _, login := range []string{"owner", "contributor"} {
		s := e.snap(t, login, e.family)
		for _, id := range hidden {
			if s[id] == nil || s[id]["hidden"] != true {
				t.Fatalf("%s snapshot of %s: %v", login, id, s[id])
			}
		}
		if s[e.photo]["hidden"] != false {
			t.Fatalf("visible photo %v", s[e.photo])
		}
		for _, p := range []string{e.assetPath(geoPic) + "/original", e.assetPath(movie) + "/playback", e.assetPath(live) + "/components/paired_video", e.assetPath(geoPic) + "/preview"} {
			if r := e.as(t, login, "GET", p, nil, nil); r.Status != 200 {
				t.Fatalf("%s %s: %d", login, p, r.Status)
			}
		}
		if l := wantJSON(t, "likes", e.as(t, login, "GET", e.assetPath(geoPic)+"/likes", nil, nil), 200); l["like_count"] != 1.0 {
			t.Fatalf("likes %v", l)
		}
		if s[geoPic]["location"] == nil {
			t.Fatalf("%s lost the location", login)
		}
	}
	idx := local{}
	_, pages := e.apply(t, "owner", e.family, ownerCursor, idx)
	for _, id := range hidden {
		if idx[id] == nil || idx[id]["hidden"] != true {
			t.Fatalf("owner feed of %s: %v (%v)", id, idx[id], pages)
		}
	}
	if act := wantJSON(t, "owner activity", e.as(t, "owner", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any); len(act) != 3 {
		t.Fatalf("owner activity on its hidden photo: %d items", len(act))
	}
	// Cursors of one view are refused in the other.
	wantError(t, "owner cursor as viewer", e.as(t, "viewer", "GET", f+"/sync?cursor="+url.QueryEscape(ownerCursor), nil, nil), 409, "cursor_invalid")
	wantError(t, "viewer cursor as owner", e.as(t, "owner", "GET", f+"/sync?cursor="+url.QueryEscape(viewerCursor), nil, nil), 409, "cursor_invalid")

	// Shown again: the VIEWER gets it back through its feed, with its
	// likes, comments, location and activity.
	e.hide(t, "contributor", geoPic, false)
	vidx := local{e.photo: {}}
	viewerCursor, _ = e.apply(t, "viewer", e.family, viewerCursor, vidx)
	if a := vidx[geoPic]; a == nil || a["liked_by_me"] != true || a["comment_count"] != 2.0 || a["location"] == nil || a["hidden"] != false {
		t.Fatalf("viewer after showing: %v", vidx[geoPic])
	}
	if s := e.snap(t, "viewer", e.family); !slices.Equal(ids(s), ids(vidx)) {
		t.Fatalf("snapshot %v, feed %v", ids(s), ids(vidx))
	}
	if act := wantJSON(t, "activity", e.as(t, "viewer", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any); len(act) != 1 {
		t.Fatalf("viewer activity after showing: %v", act)
	}
	// A VIEWER who becomes a CONTRIBUTOR reloads (409) and then sees the
	// hidden ones; back to VIEWER, the same.
	e.setMember(t, "viewer", "CONTRIBUTOR")
	wantError(t, "promoted", e.as(t, "viewer", "GET", f+"/sync?cursor="+url.QueryEscape(viewerCursor), nil, nil), 409, "cursor_invalid")
	if s := e.snap(t, "viewer", e.family); len(s) != 4 || s[movie]["hidden"] != true {
		t.Fatalf("promoted snapshot %v", ids(s))
	}
	promoted, _ := e.apply(t, "viewer", e.family, "", local{})
	e.setMember(t, "viewer", "VIEWER")
	wantError(t, "demoted", e.as(t, "viewer", "GET", f+"/sync?cursor="+url.QueryEscape(promoted), nil, nil), 409, "cursor_invalid")
	if s := e.snap(t, "viewer", e.family); len(s) != 2 {
		t.Fatalf("demoted snapshot %v", ids(s))
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// TestHiddenFeedReplay: after any sequence of hiding, showing, removing,
// restoring, uploading and social writes, a client that applies the feed
// (from the start, or step by step from its stored cursor) ends with
// exactly the snapshot of its view, for the VIEWER and the OWNER alike.
func TestHiddenFeedReplay(t *testing.T) {
	e := newACLEnv(t)
	rng := rand.New(rand.NewSource(51))
	assets := []string{e.photo}
	for i := range 4 {
		assets = append(assets, wantJSON(t, "upload", e.upload(t, "contributor", e.family, jpegBytes(t, 10+i, 10), fmt.Sprint("r", i)), 201)["asset_id"].(string))
	}
	cursors := map[string]string{}
	stepIdx := map[string]local{"viewer": {}, "owner": {}}
	for _, login := range []string{"viewer", "owner"} {
		cursors[login], _ = e.apply(t, login, e.family, "", stepIdx[login])
	}
	for step := range 60 {
		a := assets[rng.Intn(len(assets))]
		switch rng.Intn(6) {
		case 0, 1:
			e.asJSON(t, "owner", "PATCH", e.assetPath(a), fmt.Sprintf(`{"hidden":%v}`, rng.Intn(2) == 0))
		case 2:
			e.as(t, "contributor", "DELETE", e.assetPath(a), nil, nil)
		case 3:
			e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/trash/"+a+"/restore", nil, nil)
		case 4:
			e.asJSON(t, "contributor", "PUT", e.assetPath(a)+"/likes/me", "")
		case 5:
			e.asJSON(t, "owner", "POST", e.assetPath(a)+"/comments", commentBody(t, fmt.Sprint("step ", step)))
		}
		for _, login := range []string{"viewer", "owner"} {
			cursors[login], _ = e.apply(t, login, e.family, cursors[login], stepIdx[login])
			fresh := local{}
			e.apply(t, login, e.family, "", fresh)
			s := e.snap(t, login, e.family)
			if !slices.Equal(ids(s), ids(fresh)) || !slices.Equal(ids(s), ids(stepIdx[login])) {
				t.Fatalf("step %d %s: snapshot %v, feed from start %v, step by step %v", step, login, ids(s), ids(fresh), ids(stepIdx[login]))
			}
			for id, a := range s {
				if stepIdx[login][id]["hidden"] != a["hidden"] || stepIdx[login][id]["comment_count"] != a["comment_count"] {
					t.Fatalf("step %d %s: %s differs: feed %v, snapshot %v", step, login, id, stepIdx[login][id], a)
				}
				if login == "viewer" && a["hidden"] == true {
					t.Fatalf("viewer got hidden %s", id)
				}
			}
		}
	}
}

// TestHiddenConcurrency: hiding and showing from two managers at once while
// a VIEWER reads its feed: every change is one event, and the VIEWER's feed
// ends with its snapshot.
func TestHiddenConcurrency(t *testing.T) {
	e := newACLEnv(t)
	before := e.events(t, e.family)
	var wg sync.WaitGroup
	var mu sync.Mutex
	changes := 0
	for _, login := range []string{"owner", "contributor"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				r := do(t, e.Raw, "PATCH", e.assetPath(e.photo), []byte(fmt.Sprintf(`{"hidden":%v}`, i%2 == 0)),
					map[string]string{"Authorization": "Bearer " + e.tokens[login], "Content-Type": "application/json"})
				if r.Status != 200 && r.Status != 503 {
					t.Errorf("hide as %s: %d %s", login, r.Status, r.Body)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			r := do(t, e.Raw, "GET", "/api/v1/folders/"+e.family+"/sync", nil, map[string]string{"Authorization": "Bearer " + e.tokens["viewer"]})
			if r.Status != 200 {
				t.Errorf("viewer sync: %d", r.Status)
			}
		}
	}()
	wg.Wait()
	mu.Lock()
	changes = e.audits(t, "asset.hide") + e.audits(t, "asset.unhide")
	mu.Unlock()
	if got := e.events(t, e.family); got != before+changes {
		t.Fatalf("events %d -> %d for %d changes", before, got, changes)
	}
	idx := local{}
	e.apply(t, "viewer", e.family, "", idx)
	if s := e.snap(t, "viewer", e.family); !slices.Equal(ids(s), ids(idx)) {
		t.Fatalf("snapshot %v, feed %v", ids(s), ids(idx))
	}
}

// TestLikedByMe: the album's "my likes" view lists only the caller's likes,
// without a sync cursor, and never a hidden asset for a VIEWER.
func TestLikedByMe(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	second := wantJSON(t, "second", e.upload(t, "owner", e.family, jpegBytes(t, 12, 12), "l2"), 201)["asset_id"].(string)
	third := wantJSON(t, "third", e.upload(t, "owner", e.family, jpegBytes(t, 13, 12), "l3"), 201)["asset_id"].(string)
	for _, l := range [][2]string{{"viewer", e.photo}, {"viewer", third}, {"contributor", second}, {"contributor", third}} {
		wantJSON(t, "like", e.asJSON(t, l[0], "PUT", e.assetPath(l[1])+"/likes/me", ""), 200)
	}
	mine := func(login string) []string {
		t.Helper()
		m := wantJSON(t, "my likes", e.as(t, login, "GET", f+"/assets?liked_by_me=true&limit=1", nil, nil), 200)
		if _, ok := m["cursor"]; ok {
			t.Fatalf("my likes carries a sync cursor: %v", m)
		}
		var out []string
		for {
			for _, a := range m["assets"].([]any) {
				a := a.(map[string]any)
				if a["liked_by_me"] != true {
					t.Fatalf("not liked %v", a)
				}
				out = append(out, a["id"].(string))
			}
			if m["has_more"] != true {
				break
			}
			m = wantJSON(t, "my likes", e.as(t, login, "GET", f+"/assets?liked_by_me=true&limit=1&after="+m["next_after"].(string), nil, nil), 200)
		}
		slices.Sort(out)
		return out
	}
	want := func(v ...string) []string { slices.Sort(v); return v }
	if got := mine("viewer"); !slices.Equal(got, want(e.photo, third)) {
		t.Fatalf("viewer likes %v", got)
	}
	if got := mine("contributor"); !slices.Equal(got, want(second, third)) {
		t.Fatalf("contributor likes %v", got)
	}
	if got := mine("owner"); len(got) != 0 {
		t.Fatalf("owner likes %v", got)
	}
	e.hide(t, "contributor", third, true)
	if got := mine("viewer"); !slices.Equal(got, want(e.photo)) {
		t.Fatalf("viewer likes with a hidden one %v", got)
	}
	if got := mine("contributor"); !slices.Equal(got, want(second, third)) {
		t.Fatalf("contributor likes with a hidden one %v", got)
	}
	for _, v := range []string{"yes", "1", "TRUE"} {
		wantError(t, "filter "+v, e.as(t, "viewer", "GET", f+"/assets?liked_by_me="+v, nil, nil), 400, "invalid_filter")
	}
	wantError(t, "filter twice", e.as(t, "viewer", "GET", f+"/assets?liked_by_me=true&liked_by_me=true", nil, nil), 400, "invalid_filter")
	if m := wantJSON(t, "false", e.as(t, "viewer", "GET", f+"/assets?liked_by_me=false", nil, nil), 200); m["cursor"] == nil {
		t.Fatal("liked_by_me=false is the snapshot")
	}
	wantError(t, "uploader", e.as(t, "uploader", "GET", f+"/assets?liked_by_me=true", nil, nil), 403, "forbidden")
}

// TestAlbumTrash: the OWNER and CONTRIBUTORs remove any asset to the
// album's trash (a VIEWER or UPLOADER cannot), list it with its preview
// and restore it with its likes and comments; a re-uploaded original
// blocks the restore; after 30 days the removal is final.
func TestAlbumTrash(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	wantJSON(t, "like", e.asJSON(t, "viewer", "PUT", e.assetPath(e.photo)+"/likes/me", ""), 200)
	e.comment(t, "contributor", "hello "+e.mention("viewer"), e.ids["viewer"])
	viewerCursor, _ := e.apply(t, "viewer", e.family, "", local{})

	for login, want := range map[string]int{"viewer": 403, "uploader": 403, "outsider": 404, "root": 404} {
		if r := e.as(t, login, "DELETE", e.assetPath(e.photo), nil, nil); r.Status != want {
			t.Fatalf("remove as %s: %d", login, r.Status)
		}
		if r := e.as(t, login, "GET", f+"/trash", nil, nil); r.Status != want {
			t.Fatalf("trash as %s: %d", login, r.Status)
		}
	}
	wantStatusReply(t, "contributor removes the owner's photo", e.as(t, "contributor", "DELETE", e.assetPath(e.photo), nil, nil), 204)
	idx := local{e.photo: {}}
	viewerCursor, _ = e.apply(t, "viewer", e.family, viewerCursor, idx)
	if len(idx) != 0 {
		t.Fatalf("viewer still has %v", ids(idx))
	}
	wantError(t, "removed original", e.as(t, "owner", "GET", e.assetPath(e.photo)+"/original", nil, nil), 404, "asset_not_found")
	wantError(t, "removed likes", e.as(t, "owner", "GET", e.assetPath(e.photo)+"/likes", nil, nil), 404, "asset_not_found")
	wantError(t, "remove again", e.as(t, "owner", "DELETE", e.assetPath(e.photo), nil, nil), 404, "asset_not_found")

	for _, login := range []string{"owner", "contributor"} {
		m := wantJSON(t, "trash", e.as(t, login, "GET", f+"/trash", nil, nil), 200)
		wantKeys(t, "trash page", m, "folder_id", "assets", "next_cursor", "has_more")
		list := m["assets"].([]any)
		if len(list) != 1 {
			t.Fatalf("%s trash %v", login, list)
		}
		a := list[0].(map[string]any)
		if a["id"] != e.photo || a["removed_by_name"] != "contributor" || a["like_count"] != 1.0 || a["comment_count"] != 1.0 || a["sha256"] != shaHex(e.pic) {
			t.Fatalf("trash item %v", a)
		}
		removed, _ := time.Parse(time.RFC3339Nano, a["removed_at"].(string))
		purge, _ := time.Parse(time.RFC3339Nano, a["purge_after"].(string))
		if purge.Sub(removed) != meta.TrashRetention {
			t.Fatalf("purge_after %v removed_at %v", a["purge_after"], a["removed_at"])
		}
		if r := e.as(t, login, "GET", f+"/trash/"+e.photo+"/preview", nil, nil); r.Status != 200 || r.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("trash preview %d %v", r.Status, r.Header)
		}
	}
	wantError(t, "viewer trash preview", e.as(t, "viewer", "GET", f+"/trash/"+e.photo+"/preview", nil, nil), 403, "forbidden")
	wantError(t, "preview of an asset not in the trash", e.as(t, "owner", "GET", f+"/trash/"+strings.Repeat("a", 32)+"/preview", nil, nil), 404, "asset_not_found")
	wantError(t, "viewer restores", e.as(t, "viewer", "POST", f+"/trash/"+e.photo+"/restore", nil, nil), 403, "forbidden")

	// Restored: same id, likes and comments back, asset_added in the feed.
	a := wantJSON(t, "restore", e.as(t, "owner", "POST", f+"/trash/"+e.photo+"/restore", nil, nil), 200)
	if a["id"] != e.photo || a["like_count"] != 1.0 || a["comment_count"] != 1.0 {
		t.Fatalf("restored %v", a)
	}
	e.apply(t, "viewer", e.family, viewerCursor, idx)
	if v := idx[e.photo]; v == nil || v["liked_by_me"] != true || v["comment_count"] != 1.0 {
		t.Fatalf("viewer after restore %v", idx)
	}
	if n := len(wantJSON(t, "activity", e.as(t, "viewer", "GET", "/api/v1/me/activity", nil, nil), 200)["items"].([]any)); n != 1 {
		t.Fatalf("viewer activity after restore: %d", n)
	}
	wantError(t, "restore again", e.as(t, "owner", "POST", f+"/trash/"+e.photo+"/restore", nil, nil), 404, "asset_not_found")
	if e.audits(t, "asset.restore") != 1 {
		t.Fatal("restore not audited")
	}

	// The same original uploaded again blocks the restore.
	wantStatusReply(t, "remove", e.as(t, "contributor", "DELETE", e.assetPath(e.photo), nil, nil), 204)
	again := wantJSON(t, "upload again", e.upload(t, "owner", e.family, e.pic, "again"), 201)["asset_id"].(string)
	if again == e.photo {
		t.Fatal("re-upload reused the removed id")
	}
	wantError(t, "conflict", e.as(t, "contributor", "POST", f+"/trash/"+e.photo+"/restore", nil, nil), 409, "asset_conflict")

	// A hidden asset in the trash stays hidden when restored.
	e.hide(t, "contributor", again, true)
	wantStatusReply(t, "remove hidden", e.as(t, "contributor", "DELETE", e.assetPath(again), nil, nil), 204)
	wantJSON(t, "restore hidden", e.as(t, "owner", "POST", f+"/trash/"+again+"/restore", nil, nil), 200)
	if s := e.snap(t, "viewer", e.family); s[again] != nil {
		t.Fatal("restored hidden asset shown to the viewer")
	}

	// Older than 30 days: not restorable, then purged (likes and comments
	// deleted, removal final, the feed still reads).
	if _, err := e.Meta.DB().Exec("UPDATE folder_assets SET removed_at = ? WHERE asset_id = ?",
		time.Now().Add(-meta.TrashRetention-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z"), e.photo); err != nil {
		t.Fatal(err)
	}
	if l := wantJSON(t, "trash", e.as(t, "owner", "GET", f+"/trash", nil, nil), 200)["assets"].([]any); len(l) != 0 {
		t.Fatalf("expired trash listed %v", l)
	}
	wantError(t, "restore expired", e.as(t, "owner", "POST", f+"/trash/"+e.photo+"/restore", nil, nil), 404, "asset_not_found")
	p, err := e.API.PurgeExpired(t.Context(), time.Now())
	if err != nil || p.TrashPurged != 1 || p.FoldersPurged != 0 {
		t.Fatalf("purge %+v %v", p, err)
	}
	if n := e.rows(t, "SELECT count(*) FROM asset_likes WHERE asset_id = ?", e.photo) + e.rows(t, "SELECT count(*) FROM asset_comments WHERE asset_id = ?", e.photo); n != 0 {
		t.Fatalf("%d social rows after the purge", n)
	}
	if e.rows(t, "SELECT count(*) FROM folder_assets WHERE asset_id = ? AND purged_at IS NOT NULL", e.photo) != 1 || e.audits(t, "trash.purge") != 1 {
		t.Fatal("purge not recorded")
	}
	if p, err := e.API.PurgeExpired(t.Context(), time.Now()); err != nil || p.TrashPurged != 0 {
		t.Fatalf("second purge %+v %v", p, err)
	}
	for _, login := range []string{"viewer", "owner"} {
		e.apply(t, login, e.family, "", local{})
	}
	if _, err := e.Meta.DB().Exec("UPDATE folder_assets SET removed_at = NULL WHERE asset_id = ?", e.photo); err == nil || !strings.Contains(err.Error(), "final") {
		t.Fatalf("a purged removal was undone: %v", err)
	}
}

// TestAlbumDeleteRestorePurge: deleting hides the album from everyone at
// once; only its owner lists and restores it (members keep their cursors);
// after 30 days the purge removes its rows and the originals no other
// album holds, keeps the shared ones, and leaves the library consistent.
func TestAlbumDeleteRestorePurge(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	shared := wantJSON(t, "shared", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"shared"}`), nil), 201)["id"].(string)
	sharedPhoto := wantJSON(t, "same bytes", e.upload(t, "owner", shared, e.pic, "s1"), 201)["asset_id"].(string)
	unique := jpegBytes(t, 21, 22)
	wantJSON(t, "unique", e.upload(t, "contributor", e.family, unique, "u1"), 201)
	lp := newLive(liveID, 44)
	wantJSON(t, "live", e.uploadLive(t, "owner", e.family, "live-d", lp), 201)
	wantJSON(t, "like", e.asJSON(t, "viewer", "PUT", e.assetPath(e.photo)+"/likes/me", ""), 200)
	contributorCursor, _ := e.apply(t, "contributor", e.family, "", local{})
	familyAssets := e.rows(t, "SELECT count(*) FROM folder_assets WHERE folder_id = ?", e.family)

	for login, want := range map[string]int{"contributor": 403, "viewer": 403, "uploader": 403, "outsider": 404, "root": 404} {
		if r := e.as(t, login, "DELETE", f, nil, nil); r.Status != want {
			t.Fatalf("delete as %s: %d", login, r.Status)
		}
	}
	wantStatusReply(t, "delete", e.as(t, "owner", "DELETE", f, nil, nil), 204)
	if e.audits(t, "folder.delete") != 1 {
		t.Fatal("delete not audited")
	}
	for _, login := range []string{"owner", "contributor", "viewer", "uploader"} {
		for _, fo := range wantJSON(t, "folders", e.as(t, login, "GET", "/api/v1/folders", nil, nil), 200)["folders"].([]any) {
			if fo.(map[string]any)["id"] == e.family {
				t.Fatalf("%s still lists the deleted album", login)
			}
		}
		for _, p := range []string{f, f + "/assets", f + "/sync", f + "/members", e.assetPath(e.photo) + "/original"} {
			wantError(t, login+" "+p, e.as(t, login, "GET", p, nil, nil), 404, "folder_not_found")
		}
		wantError(t, login+" upload", e.upload(t, login, e.family, jpegBytes(t, 5, 5), "late"), 404, "folder_not_found")
	}
	deleted := func(login string) []any {
		t.Helper()
		return wantJSON(t, "deleted", e.as(t, login, "GET", "/api/v1/folders/deleted", nil, nil), 200)["folders"].([]any)
	}
	if d := deleted("owner"); len(d) != 1 || d[0].(map[string]any)["id"] != e.family || d[0].(map[string]any)["name"] != "family" {
		t.Fatalf("owner deleted list %v", d)
	} else {
		wantKeys(t, "deleted folder", d[0].(map[string]any), "id", "name", "created_at", "deleted_at", "purge_after")
	}
	for _, login := range []string{"contributor", "viewer", "root"} {
		if d := deleted(login); len(d) != 0 {
			t.Fatalf("%s sees deleted folders %v", login, d)
		}
		wantError(t, login+" restores", e.as(t, login, "POST", f+"/restore", nil, nil), 404, "folder_not_found")
	}
	wantError(t, "restore a folder in use", e.as(t, "owner", "POST", "/api/v1/folders/"+shared+"/restore", nil, nil), 404, "folder_not_found")
	storage := wantJSON(t, "storage", e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil), 200)
	owned := wantJSON(t, "owned", e.as(t, "root", "GET", "/api/v1/admin/users/"+e.ids["owner"]+"/folders", nil, nil), 200)
	if storage["deleted_albums"] != 1.0 || owned["deleted_owned"] != 1.0 || owned["active_owned"] != 1.0 {
		t.Fatalf("admin view: storage %v owned %v", storage["deleted_albums"], owned)
	}

	// Restored: everything as it was, members' cursors still valid.
	m := wantJSON(t, "restore", e.as(t, "owner", "POST", f+"/restore", nil, nil), 200)
	if m["id"] != e.family || m["role"] != "OWNER" || m["member_count"] != 4.0 {
		t.Fatalf("restored %v", m)
	}
	idx := local{}
	if _, pages := e.apply(t, "contributor", e.family, contributorCursor, idx); len(idx) != 0 || len(pages) != 1 {
		t.Fatalf("contributor after restore: %v", pages)
	}
	if n := e.rows(t, "SELECT count(*) FROM folder_assets WHERE folder_id = ?", e.family); n != familyAssets {
		t.Fatalf("assets %d, want %d", n, familyAssets)
	}
	wantError(t, "restore twice", e.as(t, "owner", "POST", f+"/restore", nil, nil), 404, "folder_not_found")

	// Deleted again and purged after 30 days.
	wantStatusReply(t, "delete again", e.as(t, "owner", "DELETE", f, nil, nil), 204)
	if p, err := e.API.PurgeExpired(t.Context(), time.Now()); err != nil || p.FoldersPurged != 0 {
		t.Fatalf("early purge %+v %v", p, err)
	}
	gone := []string{shaHex(unique), shaHex(lp.still), shaHex(lp.video)}
	for _, sha := range gone {
		if _, err := os.Stat(e.Blobs.PoolPath(sha)); err != nil {
			t.Fatalf("pool file before the purge: %v", err)
		}
	}
	p, err := e.API.PurgeExpired(t.Context(), time.Now().Add(meta.TrashRetention+time.Minute))
	if err != nil || p.FoldersPurged != 1 || p.OriginalsPurged != 3 {
		t.Fatalf("purge %+v %v", p, err)
	}
	for _, sha := range gone {
		if e.rows(t, "SELECT count(*) FROM blobs WHERE sha256 = ?", sha) != 0 {
			t.Fatalf("blob row %s kept", sha)
		}
		for _, path := range []string{e.Blobs.PoolPath(sha), e.Blobs.PreviewPath(sha)} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("%s still there: %v", filepath.Base(path), err)
			}
		}
	}
	if left, _ := os.ReadDir(filepath.Join(e.Root, ".system", "purge")); len(left) != 0 {
		t.Fatalf("purge directory not empty: %d", len(left))
	}
	for _, q := range []string{"SELECT count(*) FROM folders WHERE folder_id = ?", "SELECT count(*) FROM folder_members WHERE folder_id = ?",
		"SELECT count(*) FROM folder_changes WHERE folder_id = ?", "SELECT count(*) FROM folder_assets WHERE folder_id = ?", "SELECT count(*) FROM asset_likes WHERE folder_id = ?"} {
		if n := e.rows(t, q, e.family); n != 0 {
			t.Fatalf("%s: %d", q, n)
		}
	}
	if e.rows(t, "SELECT count(*) FROM assets WHERE asset_id = ?", e.photo) != 0 || e.audits(t, "folder.purge") != 1 {
		t.Fatal("assets or audit after the purge")
	}
	// The shared original stays and still downloads byte for byte.
	r := e.as(t, "owner", "GET", "/api/v1/folders/"+shared+"/assets/"+sharedPhoto+"/original", nil, nil)
	if r.Status != 200 || shaHex(r.Body) != shaHex(e.pic) {
		t.Fatalf("shared original after the purge: %d", r.Status)
	}
	wantError(t, "restore purged", e.as(t, "owner", "POST", f+"/restore", nil, nil), 404, "folder_not_found")
	if d := deleted("owner"); len(d) != 0 {
		t.Fatalf("purged folder listed %v", d)
	}
	if s := wantJSON(t, "storage", e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil), 200); s["deleted_albums"] != 0.0 {
		t.Fatalf("deleted_albums %v", s["deleted_albums"])
	}
	rep, err := integrity.Run(t.Context(), e.Volume, e.Meta.DB(), integrity.Options{Hash: true})
	if err != nil || !rep.Healthy() || rep.Unreferenced != 0 {
		t.Fatalf("integrity after the purge: %+v %v", rep, err)
	}
	rec, err := e.Blobs.Reconcile(t.Context(), e.Meta)
	if err != nil || !rec.Healthy() || rec.Orphans != 0 {
		t.Fatalf("reconcile after the purge: %s %v", rec.Summary(), err)
	}
}

// TestAlbumPurgeKeepsWhatOthersHold: an asset another folder still links
// (PoC imports linked one asset to several folders) and its original
// survive the purge of one of its folders.
func TestAlbumPurgeKeepsWhatOthersHold(t *testing.T) {
	e := newACLEnv(t)
	if _, err := e.Meta.DB().Exec("INSERT INTO folder_assets (folder_id, asset_id, added_at) VALUES (?, ?, 't')", e.adminF, e.photo); err != nil {
		t.Fatal(err)
	}
	wantStatusReply(t, "delete", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family, nil, nil), 204)
	p, err := e.API.PurgeExpired(t.Context(), time.Now().Add(meta.TrashRetention+time.Minute))
	if err != nil || p.FoldersPurged != 1 || p.OriginalsPurged != 0 {
		t.Fatalf("purge %+v %v", p, err)
	}
	r := e.as(t, "root", "GET", "/api/v1/folders/"+e.adminF+"/assets/"+e.photo+"/original", nil, nil)
	if r.Status != 200 || shaHex(r.Body) != shaHex(e.pic) {
		t.Fatalf("other folder's original: %d", r.Status)
	}
}

// TestAlbumPurgeDetachFailure: when the originals cannot be moved out of
// the pool the purge commits nothing; the album stays restorable and its
// files stay where they were.
func TestAlbumPurgeDetachFailure(t *testing.T) {
	e := newACLEnv(t)
	unique := jpegBytes(t, 31, 9)
	wantJSON(t, "unique", e.upload(t, "owner", e.family, unique, "u"), 201)
	wantStatusReply(t, "delete", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family, nil, nil), 204)
	later := time.Now().Add(meta.TrashRetention + time.Minute)
	ids, err := e.Meta.DeletedFoldersDue(t.Context(), later, 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("due %v %v", ids, err)
	}
	failing := func(shas []string) (func(), error) {
		undo, err := e.Blobs.DetachOriginals(shas[:1])
		if err != nil {
			return nil, err
		}
		undo() // as DetachOriginals does on its own failure
		return nil, fmt.Errorf("injected detach failure")
	}
	if _, err := e.Meta.PurgeDeletedFolder(t.Context(), e.family, later, failing); err == nil {
		t.Fatal("purge with a failing detach succeeded")
	}
	if _, err := os.Stat(e.Blobs.PoolPath(shaHex(unique))); err != nil {
		t.Fatalf("pool file after a failed purge: %v", err)
	}
	wantJSON(t, "restore", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/restore", nil, nil), 200)
	if r := e.as(t, "owner", "GET", "/api/v1/folders/"+e.family+"/assets", nil, nil); r.Status != 200 {
		t.Fatalf("after restore %d", r.Status)
	}
}

// TestAlbumPurgeCrashRecovery: a purge that moved the originals out of the
// pool but never committed (the process died) leaves rows whose files are
// in .system/purge; the startup recovery puts them back, and a committed
// purge's leftovers are deleted.
func TestAlbumPurgeCrashRecovery(t *testing.T) {
	e := newACLEnv(t)
	sha := shaHex(e.pic)
	if _, err := e.Blobs.DetachOriginals([]string{sha}); err != nil { // "crash" before COMMIT
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Blobs.PoolPath(sha)); !os.IsNotExist(err) {
		t.Fatalf("detached file still in the pool: %v", err)
	}
	rec, err := e.Blobs.RecoverPurged(t.Context(), func(s string) (bool, error) { return e.Meta.HasBlob(t.Context(), s) })
	if err != nil || rec.Restored != 1 || rec.Removed != 0 {
		t.Fatalf("recover %+v %v", rec, err)
	}
	if r := e.as(t, "owner", "GET", e.assetPath(e.photo)+"/original", nil, nil); r.Status != 200 || shaHex(r.Body) != sha {
		t.Fatalf("original after recovery: %d", r.Status)
	}
	// A committed purge's file (no row) is deleted.
	orphan := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(e.Root, ".system", "purge", orphan), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Root, ".system", "purge", "not-a-hash"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err = e.Blobs.RecoverPurged(t.Context(), func(s string) (bool, error) { return e.Meta.HasBlob(t.Context(), s) })
	if err != nil || rec.Restored != 0 || rec.Removed != 1 || rec.Unexpected != 1 {
		t.Fatalf("recover %+v %v", rec, err)
	}
	if r, err := e.Blobs.Reconcile(t.Context(), e.Meta); err != nil || !r.Healthy() {
		t.Fatalf("reconcile %v %v", r, err)
	}
}

// TestAlbumPurgeRacesUpload: an upload of the same bytes into another
// album while a purge removes them never leaves a committed asset without
// its pool file.
func TestAlbumPurgeRacesUpload(t *testing.T) {
	e := newACLEnv(t)
	for i := range 8 {
		doomed := wantJSON(t, "doomed", e.as(t, "owner", "POST", "/api/v1/folders", []byte(fmt.Sprintf(`{"name":"doomed %d"}`, i)), nil), 201)["id"].(string)
		body := jpegBytes(t, 50+i, 7)
		wantJSON(t, "upload", e.upload(t, "owner", doomed, body, fmt.Sprint("d", i)), 201)
		wantStatusReply(t, "delete", e.as(t, "owner", "DELETE", "/api/v1/folders/"+doomed, nil, nil), 204)
		var wg sync.WaitGroup
		var status int
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := e.API.PurgeExpired(t.Context(), time.Now().Add(meta.TrashRetention+time.Minute)); err != nil {
				t.Errorf("purge: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			r := do(t, e.Raw, "POST", "/api/v1/folders/"+e.family+"/assets", body,
				map[string]string{"Authorization": "Bearer " + e.tokens["contributor"], idemKey: fmt.Sprint("race", i)})
			status = r.Status
		}()
		wg.Wait()
		if status != 201 {
			// Refused because the file left under it: a retry stores it.
			wantJSON(t, "retry", e.upload(t, "contributor", e.family, body, fmt.Sprint("race", i)), 201)
		}
		if _, err := os.Stat(e.Blobs.PoolPath(shaHex(body))); err != nil {
			t.Fatalf("round %d (upload %d): committed asset without its pool file: %v", i, status, err)
		}
	}
	rep, err := integrity.Run(t.Context(), e.Volume, e.Meta.DB(), integrity.Options{Hash: true})
	if err != nil || !rep.Healthy() {
		t.Fatalf("integrity: %+v %v", rep, err)
	}
}

// TestDisableOwnerOfDeletedAlbum: an account whose albums are all deleted
// can be disabled; once enabled again its owner can still restore them.
func TestDisableOwnerOfDeletedAlbum(t *testing.T) {
	e := newACLEnv(t)
	wantStatusReply(t, "delete", e.as(t, "owner", "DELETE", "/api/v1/folders/"+e.family, nil, nil), 204)
	wantJSON(t, "disable", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["owner"], `{"status":"disabled"}`), 200)
	wantJSON(t, "enable", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["owner"], `{"status":"active"}`), 200)
	e.tokens["owner"] = loginToken(t, e.Raw, "owner", userPassword)
	wantJSON(t, "restore", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/restore", nil, nil), 200)
	wantError(t, "disable an owner in use", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["owner"], `{"status":"disabled"}`), 409, "owns_albums")
}

// TestMembersForReaders: every reader lists the members (is_me, is_owner);
// only the owner sees accounts that cannot log in and the pending
// invitations.
func TestMembersForReaders(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	wantJSON(t, "invite", e.asJSON(t, "owner", "POST", f+"/invitations", `{"invited_user_id":"`+e.ids["outsider"]+`","role":"VIEWER"}`), 201)
	wantJSON(t, "disable uploader", e.asJSON(t, "root", "PATCH", "/api/v1/admin/users/"+e.ids["uploader"], `{"status":"disabled"}`), 200)
	list := func(login string) (map[string]map[string]any, map[string]any) {
		t.Helper()
		m := wantJSON(t, "members", e.as(t, login, "GET", f+"/members", nil, nil), 200)
		out := map[string]map[string]any{}
		for i, it := range m["members"].([]any) {
			mm := it.(map[string]any)
			wantKeys(t, "member", mm, "user_id", "display_name", "role", "active", "joined_at", "is_me", "is_owner")
			if (i == 0) != (mm["role"] == "OWNER") || mm["is_owner"] != (mm["role"] == "OWNER") || mm["is_me"] != (mm["user_id"] == e.ids[login]) {
				t.Fatalf("%s: member %d %v", login, i, mm)
			}
			out[mm["display_name"].(string)] = mm
		}
		return out, m
	}
	for _, login := range []string{"contributor", "viewer"} {
		ms, m := list(login)
		if _, ok := m["invitations"]; ok || len(ms) != 3 || ms["uploader"] != nil || ms[login]["is_me"] != true {
			t.Fatalf("%s: %v", login, m)
		}
	}
	ms, m := list("owner")
	if inv := m["invitations"].([]any); len(inv) != 1 || len(ms) != 4 || ms["uploader"]["active"] != false {
		t.Fatalf("owner: %v", m)
	}
	if r := e.as(t, "uploader", "GET", f+"/members", nil, nil); r.Status != 401 { // disabled above
		t.Fatalf("disabled uploader: %d", r.Status)
	}
}
