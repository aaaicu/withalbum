package httpapi_test

import (
	"testing"

	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

// TestAssetDuration: the capture worker reads a movie's playing time
// (lpt.MOV: timescale 600, duration 1800 = 3 s) after the commit; until
// then duration_ms is null. A video asset carries it itself; a Live Photo
// does not (its own original is the still) but its paired_video
// component does. Each change is one asset_updated, visible in the feed.
// The backfill (duration pending after the migration) reads it again
// without touching settled capture times; photos get no event.
func TestAssetDuration(t *testing.T) {
	e := newACLEnv(t)
	processCaptures(t, e)
	f := "/api/v1/folders/" + e.family

	clip := lpt.MOV("", 3)
	clipID := wantJSON(t, "upload clip", e.as(t, "contributor", "POST", f+"/assets", clip, map[string]string{"X-Filename": "clip.mov"}), 201)["asset_id"].(string)
	liveAsset := wantJSON(t, "live photo", e.uploadLive(t, "owner", e.family, "live-1", newLive(liveID, 5)), 201)["asset_id"].(string)

	before := e.snapshotAssets(t, "viewer")
	if a := before[clipID]; a["media_kind"] != "video" || a["duration_ms"] != nil {
		t.Fatalf("clip before the worker %v", a)
	}
	events := e.events(t, e.family)
	processCaptures(t, e)
	// One event for the clip (capture time and duration together), one
	// for the live photo's video (its duration) and one for its still (its
	// pixel size, M48).
	if got := e.events(t, e.family); got != events+3 {
		t.Fatalf("events %d -> %d, want three asset_updated", events, got)
	}
	check := func(what string) {
		t.Helper()
		as := e.snapshotAssets(t, "viewer")
		if a := as[clipID]; a["duration_ms"] != float64(3000) {
			t.Fatalf("%s: clip %v", what, a)
		}
		lp := as[liveAsset]
		comps := lp["components"].([]any)
		c := comps[0].(map[string]any)
		wantKeys(t, "component", c, "role", "sha256", "bytes", "mime_type", "filename", "duration_ms")
		if lp["media_kind"] != "live_photo" || lp["duration_ms"] != nil || c["duration_ms"] != float64(3000) {
			t.Fatalf("%s: live photo %v", what, lp)
		}
		if a := as[e.photo]; a["duration_ms"] != nil {
			t.Fatalf("%s: photo %v", what, a)
		}
	}
	check("after the worker")
	page := syncPage(t, asUser(e.Raw, e.tokens["viewer"]), e.family, "")
	if a := page["assets"].(map[string]any)[clipID].(map[string]any); a["duration_ms"] != float64(3000) {
		t.Fatalf("feed asset %v", a)
	}
	captured := e.snapshotAssets(t, "viewer")[clipID]["captured_at"]
	if captured == nil {
		t.Fatal("clip has no capture time (lpt.MOV records one in mvhd)")
	}

	// The backfill: durations pending again, capture times settled.
	if _, err := e.Meta.DB().Exec("UPDATE blobs SET duration_state = 'pending', duration_ms = NULL"); err != nil {
		t.Fatal(err)
	}
	events = e.events(t, e.family)
	if n := processCaptures(t, e); n != e.count(t, "SELECT count(*) FROM blobs") {
		t.Fatalf("backfill settled %d", n)
	}
	if got := e.events(t, e.family); got != events+2 {
		t.Fatalf("backfill events %d -> %d, want two", events, got)
	}
	check("after the backfill")
	if a := e.snapshotAssets(t, "viewer")[clipID]; a["captured_at"] != captured {
		t.Fatalf("capture time changed by the backfill: %v", a)
	}
	if n := processCaptures(t, e); n != 0 {
		t.Fatalf("second run settled %d", n)
	}

	// An upload-only member still reads nothing.
	wantError(t, "uploader snapshot", e.as(t, "uploader", "GET", f+"/assets", nil, nil), 403, "forbidden")
}
