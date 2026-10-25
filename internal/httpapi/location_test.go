package httpapi_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	geo "github.com/aaaicu/withalbum/internal/capturetime/geotest"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

// All positions are synthetic (geotest.Sample*).

func wantLocation(t *testing.T, what string, a map[string]any, lat, lon float64, acc any) {
	t.Helper()
	l, ok := a["location"].(map[string]any)
	if !ok {
		t.Fatalf("%s: location %v", what, a["location"])
	}
	wantKeys(t, what+" location", l, "lat", "lon", "accuracy_m")
	if math.Abs(l["lat"].(float64)-lat) > 1e-7 || math.Abs(l["lon"].(float64)-lon) > 1e-7 || l["accuracy_m"] != acc {
		t.Fatalf("%s: location %v, want %v %v ±%v", what, l, lat, lon, acc)
	}
}

// TestAssetLocation: the capture worker reads the location after the
// commit (null before, one asset_updated after); readers see it in the
// snapshot and the feed; an UPLOADER sees nothing and gets the same
// receipt; a Live Photo whose still has no GPS shows its video's; the
// backfill reads it again.
func TestAssetLocation(t *testing.T) {
	e := newACLEnv(t)
	processCaptures(t, e)
	f := "/api/v1/folders/" + e.family
	lat, lon := geo.SampleLat, geo.SampleLon

	pic := geo.JPEG(lat, lon, 4.5, 11)
	picID := wantJSON(t, "upload", e.as(t, "contributor", "POST", f+"/assets", pic, map[string]string{"X-Filename": "geo.jpg"}), 201)["asset_id"].(string)
	clip := geo.MP4XYZ(geo.ISO6709(-lat, -lon), 12)
	clipID := wantJSON(t, "clip", e.as(t, "owner", "POST", f+"/assets", clip, map[string]string{"X-Filename": "clip.mp4"}), 201)["asset_id"].(string)
	lp := livePhoto{still: lpt.JPEGWithTIFF(geo.LiveTIFF(liveID, 0, 0, true), 13),
		video: geo.MOV(13, geo.Item{Key: "com.apple.quicktime.content.identifier", Value: liveID},
			geo.Item{Key: "com.apple.quicktime.location.ISO6709", Value: "+12.5000+123.2500/"})}
	liveAsset := wantJSON(t, "live", e.uploadLive(t, "owner", e.family, "geo-live", lp), 201)["asset_id"].(string)

	// The uploader's receipt has no location (nor anything else).
	r := e.as(t, "uploader", "POST", f+"/assets", geo.JPEG(lat, lon, 0, 14), map[string]string{"X-Filename": "u.jpg"})
	wantKeys(t, "receipt", wantJSON(t, "uploader upload", r, 202), "result", "folder_id", "sha256", "bytes")

	if a := e.snapshotAssets(t, "viewer")[picID]; a["location"] != nil {
		t.Fatalf("before the worker: %v", a)
	}
	events := e.events(t, e.family)
	processCaptures(t, e)
	// pic, clip (capture time from mvhd too), live (video), uploader's photo.
	if got := e.events(t, e.family); got != events+4 {
		t.Fatalf("events %d -> %d, want 4", events, got)
	}
	check := func(what string) {
		t.Helper()
		for _, login := range []string{"owner", "contributor", "viewer"} {
			as := e.snapshotAssets(t, login)
			wantLocation(t, what+"/"+login+"/pic", as[picID], lat, lon, 4.5)
			wantLocation(t, what+"/"+login+"/clip", as[clipID], -12.3457, -123.4568, nil)
			wantLocation(t, what+"/"+login+"/live", as[liveAsset], 12.5, 123.25, nil)
			if as[e.photo]["location"] != nil {
				t.Fatalf("%s: photo without GPS %v", what, as[e.photo])
			}
		}
	}
	check("after the worker")
	page := syncPage(t, asUser(e.Raw, e.tokens["viewer"]), e.family, "")
	wantLocation(t, "feed", page["assets"].(map[string]any)[picID].(map[string]any), lat, lon, 4.5)
	wantError(t, "uploader snapshot", e.as(t, "uploader", "GET", f+"/assets", nil, nil), 403, "forbidden")
	wantError(t, "uploader feed", e.as(t, "uploader", "GET", f+"/sync", nil, nil), 403, "forbidden")
	wantError(t, "outsider", e.as(t, "outsider", "GET", f+"/assets", nil, nil), 404, "folder_not_found")

	// The original keeps its GPS tags byte for byte; the preview has none.
	orig := e.as(t, "viewer", "GET", f+"/assets/"+picID+"/original", nil, nil)
	if orig.Status != 200 || !bytes.Equal(orig.Body, pic) {
		t.Fatalf("original changed: %d", orig.Status)
	}
	prev := e.as(t, "viewer", "GET", f+"/assets/"+picID+"/preview", nil, nil)
	if prev.Status != 200 || len(prev.Body) < 4 {
		t.Fatalf("preview %d", prev.Status)
	}
	if bytes.Contains(prev.Body, []byte("Exif")) || bytes.Contains(prev.Body, []byte{0xFF, 0xE1}) {
		t.Fatal("preview carries an EXIF/APP1 segment")
	}

	// Backfill: locations pending again, nothing else.
	if _, err := e.Meta.DB().Exec("UPDATE blobs SET location_state = 'pending', location_lat = NULL, location_lon = NULL, location_accuracy_m = NULL"); err != nil {
		t.Fatal(err)
	}
	events = e.events(t, e.family)
	processCaptures(t, e)
	if got := e.events(t, e.family); got != events+4 {
		t.Fatalf("backfill events %d -> %d, want 4", events, got)
	}
	check("after the backfill")
	if n := processCaptures(t, e); n != 0 {
		t.Fatalf("second run settled %d", n)
	}
}

// TestHideLocation: only the owner may set hide_location; while set,
// members get location null and the owner still sees it; each change
// sends asset_updated for the assets with a location; the same value
// writes nothing; the UPLOADER never sees the setting.
func TestHideLocation(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	lat, lon := geo.SampleLat, geo.SampleLon
	picID := wantJSON(t, "upload", e.as(t, "owner", "POST", f+"/assets", geo.JPEG(lat, lon, 0, 21), map[string]string{"X-Filename": "geo.jpg"}), 201)["asset_id"].(string)
	processCaptures(t, e)

	if m := e.listedFolder(t, "viewer"); m["hide_location"] != false {
		t.Fatalf("default %v", m)
	}
	wantKeys(t, "uploader folder", wantJSON(t, "uploader folder", e.as(t, "uploader", "GET", f, nil, nil), 200),
		"id", "name", "created_at", "role", "can_read", "can_upload")

	on, off := `{"hide_location":true}`, `{"hide_location":false}`
	for _, login := range []string{"contributor", "viewer", "uploader"} {
		wantError(t, login, e.asJSON(t, login, "PATCH", f, on), 403, "forbidden")
	}
	for _, login := range []string{"outsider", "root"} {
		wantError(t, login, e.asJSON(t, login, "PATCH", f, on), 404, "folder_not_found")
	}
	wantError(t, "missing", e.asJSON(t, "owner", "PATCH", f, `{}`), 400, "invalid_settings")
	wantError(t, "not a boolean", e.asJSON(t, "owner", "PATCH", f, `{"hide_location":"yes"}`), 400, "invalid_json")
	wantError(t, "not json", e.as(t, "owner", "PATCH", f, []byte(on), nil), 415, "json_required")

	events := e.events(t, e.family)
	m := wantJSON(t, "hide", e.asJSON(t, "owner", "PATCH", f, on), 200)
	wantKeys(t, "folder", m, folderKeys...)
	if m["hide_location"] != true || m["role"] != "OWNER" {
		t.Fatalf("hide reply %v", m)
	}
	if got := e.events(t, e.family); got != events+1 { // only the photo with GPS
		t.Fatalf("events %d -> %d", events, got)
	}
	if e.audits(t, "folder.set_hide_location") != 1 {
		t.Fatal("no audit row")
	}
	for _, login := range []string{"contributor", "viewer"} {
		if a := e.snapshotAssets(t, login)[picID]; a["location"] != nil {
			t.Fatalf("%s sees a hidden location: %v", login, a)
		}
		if m := e.listedFolder(t, login); m["hide_location"] != true {
			t.Fatalf("%s folder %v", login, m)
		}
		page := syncPage(t, asUser(e.Raw, e.tokens[login]), e.family, "")
		if a := page["assets"].(map[string]any)[picID].(map[string]any); a["location"] != nil {
			t.Fatalf("%s feed shows a hidden location: %v", login, a)
		}
	}
	wantLocation(t, "owner while hidden", e.snapshotAssets(t, "owner")[picID], lat, lon, nil)

	// Same value: nothing written.
	events = e.events(t, e.family)
	wantJSON(t, "hide again", e.asJSON(t, "owner", "PATCH", f, on), 200)
	if got := e.events(t, e.family); got != events || e.audits(t, "folder.set_hide_location") != 1 {
		t.Fatalf("unchanged value wrote events %d -> %d", events, got)
	}
	// Shown again.
	wantJSON(t, "show", e.asJSON(t, "owner", "PATCH", f, off), 200)
	if got := e.events(t, e.family); got != events+1 {
		t.Fatalf("show events %d -> %d", events, got)
	}
	wantLocation(t, "viewer again", e.snapshotAssets(t, "viewer")[picID], lat, lon, nil)
}

// TestHideMyLocation: an account's own hide_my_location hides the location
// of the photos it uploaded from every other reader (the album owner too),
// in every album; the uploader keeps seeing them; each change sends
// asset_updated; GET /me reports the setting.
func TestHideMyLocation(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	lat, lon := geo.SampleLat, geo.SampleLon
	mine := wantJSON(t, "contributor upload", e.as(t, "contributor", "POST", f+"/assets", geo.JPEG(lat, lon, 0, 31), map[string]string{"X-Filename": "c.jpg"}), 201)["asset_id"].(string)
	owners := wantJSON(t, "owner upload", e.as(t, "owner", "POST", f+"/assets", geo.JPEG(-lat, lon, 0, 32), map[string]string{"X-Filename": "o.jpg"}), 201)["asset_id"].(string)
	processCaptures(t, e)

	me := wantJSON(t, "me", e.as(t, "contributor", "GET", "/api/v1/me", nil, nil), 200)
	wantKeys(t, "settings", me["settings"].(map[string]any), "hide_my_location")
	if me["settings"].(map[string]any)["hide_my_location"] != false {
		t.Fatalf("default settings %v", me["settings"])
	}
	wantError(t, "missing", e.asJSON(t, "contributor", "PATCH", "/api/v1/me/settings", `{}`), 400, "invalid_settings")
	wantError(t, "no session", do(t, e.Raw, "PATCH", "/api/v1/me/settings", []byte(`{"hide_my_location":true}`),
		map[string]string{"Content-Type": "application/json"}), 401, "unauthorized")

	events := e.events(t, e.family)
	r := wantJSON(t, "hide mine", e.asJSON(t, "contributor", "PATCH", "/api/v1/me/settings", `{"hide_my_location":true}`), 200)
	if r["hide_my_location"] != true {
		t.Fatalf("reply %v", r)
	}
	if got := e.events(t, e.family); got != events+1 { // only the contributor's photo
		t.Fatalf("events %d -> %d", events, got)
	}
	if e.audits(t, "user.set_hide_my_location") != 1 {
		t.Fatal("no audit row")
	}
	for _, login := range []string{"owner", "viewer"} {
		as := e.snapshotAssets(t, login)
		if as[mine]["location"] != nil {
			t.Fatalf("%s sees a location its uploader hid: %v", login, as[mine])
		}
		wantLocation(t, login+" sees the owner's photo", as[owners], -lat, lon, nil)
		page := syncPage(t, asUser(e.Raw, e.tokens[login]), e.family, "")
		if a := page["assets"].(map[string]any)[mine].(map[string]any); a["location"] != nil {
			t.Fatalf("%s feed: %v", login, a)
		}
	}
	wantLocation(t, "uploader sees its own", e.snapshotAssets(t, "contributor")[mine], lat, lon, nil)

	// Same value: nothing written. Back: shown again.
	events = e.events(t, e.family)
	wantJSON(t, "again", e.asJSON(t, "contributor", "PATCH", "/api/v1/me/settings", `{"hide_my_location":true}`), 200)
	if e.events(t, e.family) != events {
		t.Fatal("unchanged value wrote events")
	}
	wantJSON(t, "show", e.asJSON(t, "contributor", "PATCH", "/api/v1/me/settings", `{"hide_my_location":false}`), 200)
	wantLocation(t, "viewer again", e.snapshotAssets(t, "viewer")[mine], lat, lon, nil)

	// Both rules: the owner hides the album; the uploader's photo stays
	// visible to the uploader only if the uploader is the owner.
	wantJSON(t, "album hide", e.asJSON(t, "owner", "PATCH", f, `{"hide_location":true}`), 200)
	if a := e.snapshotAssets(t, "contributor")[mine]; a["location"] != nil {
		t.Fatalf("album hide applies to members' own photos too: %v", a)
	}
}

// TestLocationWithSocial: the assets of the social events (likes_updated,
// comment_added) follow the same location rules as the rest of the feed:
// with hide_location set a member gets null next to its like and comment
// counts, the owner the position.
func TestLocationWithSocial(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	lat, lon := geo.SampleLat, geo.SampleLon
	picID := wantJSON(t, "upload", e.as(t, "owner", "POST", f+"/assets", geo.JPEG(lat, lon, 0, 31), map[string]string{"X-Filename": "geo.jpg"}), 201)["asset_id"].(string)
	processCaptures(t, e)
	wantJSON(t, "hide", e.asJSON(t, "owner", "PATCH", f, `{"hide_location":true}`), 200)
	before := e.events(t, e.family)
	wantJSON(t, "like", e.asJSON(t, "viewer", "PUT", f+"/assets/"+picID+"/likes/me", ""), 200)
	wantJSON(t, "comment", e.asJSON(t, "viewer", "POST", f+"/assets/"+picID+"/comments", commentBody(t, "hi")), 201)
	if got := e.events(t, e.family); got != before+2 {
		t.Fatalf("events %d -> %d", before, got)
	}
	for _, login := range []string{"owner", "viewer"} {
		events, assets := e.feed(t, login)
		if got := kinds(events[len(events)-2:]); strings.Join(got, ",") != "likes_updated,comment_added" {
			t.Fatalf("%s kinds %v", login, got)
		}
		a := assets[picID].(map[string]any)
		if a["like_count"] != 1.0 || a["comment_count"] != 1.0 || a["liked_by_me"] != (login == "viewer") {
			t.Fatalf("%s social fields %v", login, a)
		}
		if login == "owner" {
			wantLocation(t, "owner", a, lat, lon, nil)
		} else if a["location"] != nil {
			t.Fatalf("viewer sees a hidden location next to the social fields: %v", a)
		}
	}
}
