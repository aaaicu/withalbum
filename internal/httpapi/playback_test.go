package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	pbt "github.com/aaaicu/withalbum/internal/blobstore/playbacktest"
	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/storage"
)

// newPlaybackEnv is newACLEnv with a fake playback tool, a settable clock
// and no pause between conversions.
func newPlaybackEnv(t *testing.T) (*aclEnv, *pbt.Fake, *time.Time) {
	t.Helper()
	e := newACLEnv(t)
	fake := &pbt.Fake{}
	e.Blobs.SetTranscoder(fake)
	clock := time.Now()
	httpapi.SetPlaybackClock(e.API, func() time.Time { return clock })
	httpapi.SetPlaybackGap(e.API, 0)
	return e, fake, &clock
}

// processPlayback runs the playback worker until it has nothing to do and
// returns how many originals it settled.
func processPlayback(t *testing.T, e *aclEnv) int {
	t.Helper()
	total := 0
	for {
		n, err := e.API.ProcessPlayback(t.Context())
		if err != nil {
			t.Fatalf("ProcessPlayback: %v", err)
		}
		total += n
		if n == 0 {
			return total
		}
	}
}

// playbackOf returns the playback object of asset id as login sees it in
// the snapshot.
func (e *aclEnv) playbackOf(t *testing.T, login, id string) map[string]any {
	t.Helper()
	a, ok := e.snapshotAssets(t, login)[id]
	if !ok {
		t.Fatalf("asset %s not in the snapshot", id)
	}
	p, _ := a["playback"].(map[string]any)
	if _, present := a["playback"]; !present {
		t.Fatalf("asset %s has no playback key", id)
	}
	return p
}

func (e *aclEnv) uploadMovie(t *testing.T, login, folder string, body []byte, name string) string {
	t.Helper()
	return wantJSON(t, "upload "+name, e.as(t, login, "POST", "/api/v1/folders/"+folder+"/assets", body, map[string]string{"X-Filename": name}), 201)["asset_id"].(string)
}

func (e *aclEnv) playbackStatus(t *testing.T, sha string) (status, reason string, attempts int) {
	t.Helper()
	var r *string
	if err := e.Meta.DB().QueryRow("SELECT status, reason, attempts FROM blob_playback WHERE sha256 = ?", sha).Scan(&status, &r, &attempts); err != nil {
		t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	return
}

// TestPlaybackLifecycle: a video asset is pending after the upload (null
// for photos and live photos), the worker makes the copy (one
// asset_updated per folder), readers stream it with ranges like the
// original, the UPLOADER and strangers cannot, and the original is
// unchanged.
func TestPlaybackLifecycle(t *testing.T) {
	e, fake, _ := newPlaybackEnv(t)
	f := "/api/v1/folders/" + e.family
	movie := pbt.Movie(3840, 2160, 40_000, 9)
	clip := e.uploadMovie(t, "contributor", e.family, movie, "IMG_0042.MOV")
	live := wantJSON(t, "live photo", e.uploadLive(t, "owner", e.family, "live-1", newLive(liveID, 5)), 201)["asset_id"].(string)

	if p := e.playbackOf(t, "viewer", clip); p["status"] != "pending" || p["bytes"] != nil || p["width"] != nil || p["mime_type"] != nil {
		t.Fatalf("before the worker %v", p)
	}
	wantKeys(t, "playback", e.playbackOf(t, "viewer", clip), "status", "bytes", "width", "height", "mime_type")
	if p := e.playbackOf(t, "viewer", e.photo); p != nil {
		t.Fatalf("photo %v", p)
	}
	if p := e.playbackOf(t, "viewer", live); p != nil {
		t.Fatalf("live photo %v", p)
	}
	wantError(t, "not ready", e.as(t, "viewer", "GET", f+"/assets/"+clip+"/playback", nil, nil), 404, "playback_not_ready")
	wantError(t, "photo", e.as(t, "viewer", "GET", f+"/assets/"+e.photo+"/playback", nil, nil), 404, "playback_not_ready")

	events := e.events(t, e.family)
	if n := processPlayback(t, e); n != 1 || fake.Calls() != 1 {
		t.Fatalf("settled %d, calls %d", n, fake.Calls())
	}
	if got := e.events(t, e.family); got != events+1 {
		t.Fatalf("events %d -> %d, want one asset_updated", events, got)
	}
	want := pbt.MP4(720, 1280, 1000, 1, true)
	p := e.playbackOf(t, "viewer", clip)
	if p["status"] != "ready" || p["bytes"] != float64(len(want)) || p["width"] != float64(720) || p["height"] != float64(1280) || p["mime_type"] != "video/mp4" {
		t.Fatalf("after the worker %v", p)
	}
	page := syncPage(t, asUser(e.Raw, e.tokens["viewer"]), e.family, "")
	if a := page["assets"].(map[string]any)[clip].(map[string]any); a["playback"].(map[string]any)["status"] != "ready" {
		t.Fatalf("feed asset %v", a)
	}

	for _, login := range []string{"owner", "contributor", "viewer"} {
		r := e.as(t, login, "GET", f+"/assets/"+clip+"/playback", nil, nil)
		if r.Status != 200 || !bytes.Equal(r.Body, want) || r.Header.Get("Content-Type") != "video/mp4" || r.Header.Get("Accept-Ranges") != "bytes" ||
			r.Header.Get("Content-Disposition") != `attachment; filename=IMG_0042.mp4` || r.Header.Get("Content-Security-Policy") == "" || r.Header.Get("Last-Modified") == "" {
			t.Fatalf("%s: %d %v", login, r.Status, r.Header)
		}
	}
	r := e.as(t, "viewer", "GET", f+"/assets/"+clip+"/playback", nil, map[string]string{"Range": "bytes=4-11"})
	if r.Status != 206 || !bytes.Equal(r.Body, want[4:12]) || r.Header.Get("Content-Range") != "bytes 4-11/"+itoa(len(want)) {
		t.Fatalf("range: %d %v %q", r.Status, r.Header, r.Body)
	}
	r = e.as(t, "viewer", "GET", f+"/assets/"+clip+"/playback", nil, map[string]string{"Range": "bytes=999999-"})
	if r.Status != 416 || r.Header.Get("Content-Range") != "bytes */"+itoa(len(want)) {
		t.Fatalf("past the end: %d %v", r.Status, r.Header)
	}
	r = e.as(t, "viewer", "GET", f+"/assets/"+clip+"/playback", nil, map[string]string{"Range": "bytes=0-3", "If-Range": "Mon, 02 Jan 2006 15:04:05 GMT"})
	if r.Status != 200 || !bytes.Equal(r.Body, want) {
		t.Fatalf("stale If-Range: %d", r.Status)
	}
	wantError(t, "uploader", e.as(t, "uploader", "GET", f+"/assets/"+clip+"/playback", nil, nil), 403, "forbidden")
	wantError(t, "outsider", e.as(t, "outsider", "GET", f+"/assets/"+clip+"/playback", nil, nil), 404, "folder_not_found")
	wantError(t, "root", e.as(t, "root", "GET", f+"/assets/"+clip+"/playback", nil, nil), 404, "folder_not_found")
	wantError(t, "other folder", e.as(t, "root", "GET", "/api/v1/folders/"+e.adminF+"/assets/"+clip+"/playback", nil, nil), 404, "asset_not_found")
	// The UPLOADER's receipt has nothing of it.
	rc := wantJSON(t, "uploader receipt", e.as(t, "uploader", "POST", f+"/assets", pbt.Movie(1920, 1080, 30_000, 3), nil), 202)
	wantKeys(t, "receipt", rc, "result", "folder_id", "sha256", "bytes")

	if got := shaHex(mustRead(t, e.Blobs.PoolPath(shaHex(movie)))); got != shaHex(movie) {
		t.Fatal("original changed")
	}
	if n := processPlayback(t, e); n != 1 { // the UPLOADER's movie
		t.Fatalf("second pass settled %d", n)
	}
	if n := processPlayback(t, e); n != 0 {
		t.Fatalf("third pass settled %d", n)
	}
	st := wantJSON(t, "admin storage", e.as(t, "root", "GET", "/api/v1/admin/server/storage", nil, nil), 200)["playback"].(map[string]any)
	if st["tool"] != "fake" || st["ready"] != float64(2) || st["pending"] != float64(0) || st["ready_bytes"] != float64(2*len(want)) {
		t.Fatalf("admin playback %v", st)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPlaybackRetriesThenFails: a failing tool is tried again after the
// backoff (nothing shown changes, no event), and after the third failure
// the copy is failed (one asset_updated). A timeout counts like a
// failure. Shutdown does not count.
func TestPlaybackRetriesThenFails(t *testing.T) {
	e, fake, clock := newPlaybackEnv(t)
	movie := pbt.Movie(3840, 2160, 40_000, 4)
	clip := e.uploadMovie(t, "owner", e.family, movie, "a.mov")
	sha := shaHex(movie)
	fake.Set(func(context.Context, string, string) error { return errors.New("exit status 1: bad input") })
	events := e.events(t, e.family)
	if n := processPlayback(t, e); n != 1 {
		t.Fatalf("first run settled %d", n)
	}
	if st, reason, att := e.playbackStatus(t, sha); st != "pending" || reason != "convert_error" || att != 1 {
		t.Fatalf("after one failure %s %s %d", st, reason, att)
	}
	if n := processPlayback(t, e); n != 0 || fake.Calls() != 1 {
		t.Fatalf("retried before its time: %d, %d calls", n, fake.Calls())
	}
	*clock = clock.Add(11 * time.Minute)
	// The second run times out.
	httpapi.SetPlaybackLimit(e.API, func(int64) time.Duration { return 30 * time.Millisecond })
	fake.Set(func(ctx context.Context, _, _ string) error { <-ctx.Done(); return ctx.Err() })
	if n := processPlayback(t, e); n != 1 {
		t.Fatalf("second run settled %d", n)
	}
	if st, reason, att := e.playbackStatus(t, sha); st != "pending" || reason != "timeout" || att != 2 {
		t.Fatalf("after the timeout %s %s %d", st, reason, att)
	}
	*clock = clock.Add(39 * time.Minute)
	if n := processPlayback(t, e); n != 0 {
		t.Fatal("the second retry is 40 minutes later")
	}
	// Shutdown in the middle of the third run: still pending, not counted.
	ctx, cancel := context.WithCancel(t.Context())
	fake.Set(func(c context.Context, _, _ string) error { cancel(); <-c.Done(); return c.Err() })
	*clock = clock.Add(2 * time.Minute)
	httpapi.SetPlaybackLimit(e.API, func(int64) time.Duration { return time.Minute })
	if _, err := e.API.ProcessPlayback(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	if st, _, att := e.playbackStatus(t, sha); st != "pending" || att != 2 {
		t.Fatalf("after shutdown %s %d", st, att)
	}
	if got := e.events(t, e.family); got != events {
		t.Fatalf("events while retrying %d -> %d", events, got)
	}
	fake.Set(func(context.Context, string, string) error { return errors.New("exit status 1") })
	if n := processPlayback(t, e); n != 1 {
		t.Fatalf("third run settled %d", n)
	}
	if st, reason, att := e.playbackStatus(t, sha); st != "failed" || reason != "convert_error" || att != 3 {
		t.Fatalf("after three failures %s %s %d", st, reason, att)
	}
	if got := e.events(t, e.family); got != events+1 {
		t.Fatalf("events %d -> %d, want one", events, got)
	}
	if p := e.playbackOf(t, "viewer", clip); p["status"] != "failed" || p["bytes"] != nil {
		t.Fatalf("failed %v", p)
	}
	*clock = clock.Add(24 * time.Hour)
	if n := processPlayback(t, e); n != 0 {
		t.Fatal("a failed copy was tried again")
	}
}

// TestPlaybackUnsupported: without a tool every video is unsupported
// (and queued again at the next start with one); a copy that is not
// smaller than the original is not kept (unsupported, the original is
// streamed); a very long video is not converted.
func TestPlaybackUnsupported(t *testing.T) {
	e, fake, _ := newPlaybackEnv(t)
	e.Blobs.SetTranscoder(nil)
	movie := pbt.Movie(3840, 2160, 40_000, 5)
	clip := e.uploadMovie(t, "owner", e.family, movie, "a.mov")
	sha := shaHex(movie)
	processPlayback(t, e)
	if st, reason, _ := e.playbackStatus(t, sha); st != "unsupported" || reason != "no_converter" {
		t.Fatalf("no tool: %s %s", st, reason)
	}
	if p := e.playbackOf(t, "viewer", clip); p["status"] != "unsupported" {
		t.Fatalf("no tool: %v", p)
	}
	// The next start has a tool.
	e.Blobs.SetTranscoder(fake)
	events := e.events(t, e.family)
	if err := e.API.PreparePlayback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.events(t, e.family); got != events+1 {
		t.Fatalf("requeue events %d -> %d", events, got)
	}
	if p := e.playbackOf(t, "viewer", clip); p["status"] != "pending" {
		t.Fatalf("requeued %v", p)
	}
	fake.Set(func(_ context.Context, _, out string) error {
		return os.WriteFile(out, pbt.MP4(720, 1280, 39_000, 1, true), 0600)
	})
	processPlayback(t, e)
	if st, reason, _ := e.playbackStatus(t, sha); st != "unsupported" || reason != "original_small" {
		t.Fatalf("not smaller: %s %s", st, reason)
	}
	if ok, _ := e.Blobs.PlaybackExists(sha); ok {
		t.Fatal("a copy that is not smaller was kept")
	}
	// Longer than three hours: no tool run.
	long := pbt.Movie(1920, 1080, 30_000, 6)
	e.uploadMovie(t, "owner", e.family, long, "long.mov")
	if _, err := e.Meta.DB().Exec("UPDATE blobs SET duration_state = 'done', duration_ms = ? WHERE sha256 = ?", int64(4*time.Hour/time.Millisecond), shaHex(long)); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	processPlayback(t, e)
	if st, reason, _ := e.playbackStatus(t, shaHex(long)); st != "unsupported" || reason != "too_long" || fake.Calls() != calls {
		t.Fatalf("too long: %s %s (%d calls)", st, reason, fake.Calls()-calls)
	}
}

// TestPlaybackCleanup: removing the last video asset of an original
// deletes its copy (the row idles); while another folder still holds it
// the copy stays; uploading it again makes a new copy. A copy that went
// missing is made again (GET says not ready, then asset_updated), and the
// startup sweep deletes copies the database does not report ready.
func TestPlaybackCleanup(t *testing.T) {
	e, _, _ := newPlaybackEnv(t)
	f := "/api/v1/folders/" + e.family
	movie := pbt.Movie(3840, 2160, 40_000, 2)
	sha := shaHex(movie)
	clip := e.uploadMovie(t, "owner", e.family, movie, "a.mov")
	copyID := e.uploadMovie(t, "root", e.adminF, movie, "same.mov") // the same bytes in another album
	processPlayback(t, e)
	path := e.Blobs.PlaybackPath(sha)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if r := e.as(t, "owner", "DELETE", f+"/assets/"+clip, nil, nil); r.Status != 204 {
		t.Fatalf("remove: %d %s", r.Status, r.Body)
	}
	if n, err := e.API.ReleasePlayback(t.Context()); err != nil || n != 0 {
		t.Fatalf("released %d, %v while another album holds it", n, err)
	}
	if r := e.as(t, "root", "DELETE", "/api/v1/folders/"+e.adminF+"/assets/"+copyID, nil, nil); r.Status != 204 {
		t.Fatalf("remove copy: %d %s", r.Status, r.Body)
	}
	if n, err := e.API.ReleasePlayback(t.Context()); err != nil || n != 1 {
		t.Fatalf("released %d, %v", n, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy left: %v", err)
	}
	if st, _, _ := e.playbackStatus(t, sha); st != "idle" {
		t.Fatalf("after release %s", st)
	}
	if got := shaHex(mustRead(t, e.Blobs.PoolPath(sha))); got != sha {
		t.Fatal("original changed")
	}
	// Back again: pending, a new copy.
	again := e.uploadMovie(t, "owner", e.family, movie, "a.mov")
	if p := e.playbackOf(t, "viewer", again); p["status"] != "pending" {
		t.Fatalf("again %v", p)
	}
	processPlayback(t, e)
	if p := e.playbackOf(t, "viewer", again); p["status"] != "ready" {
		t.Fatalf("again after the worker %v", p)
	}

	// Missing file: not ready, made again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	events := e.events(t, e.family)
	wantError(t, "missing", e.as(t, "viewer", "GET", f+"/assets/"+again+"/playback", nil, nil), 404, "playback_not_ready")
	if p := e.playbackOf(t, "viewer", again); p["status"] != "pending" {
		t.Fatalf("missing copy %v", p)
	}
	if got := e.events(t, e.family); got != events+1 {
		t.Fatalf("events %d -> %d", events, got)
	}
	processPlayback(t, e)
	if r := e.as(t, "viewer", "GET", f+"/assets/"+again+"/playback", nil, nil); r.Status != 200 {
		t.Fatalf("made again: %d", r.Status)
	}

	// Startup: a ready file of another size (a restored database) is
	// deleted and requeued.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("another copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.API.PreparePlayback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.playbackStatus(t, sha); st != "pending" {
		t.Fatalf("another copy at startup: %s", st)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("another copy left: %v", err)
	}
	processPlayback(t, e)

	// Startup: a missing ready file is requeued, a copy the database
	// does not know is deleted.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stray := strings64("ab")
	strayPath := e.Blobs.PlaybackPath(stray)
	if err := os.MkdirAll(filepath.Dir(strayPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strayPath, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.API.PreparePlayback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strayPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stray copy left: %v", err)
	}
	if st, reason, _ := e.playbackStatus(t, sha); st != "pending" || reason != "file_missing" {
		t.Fatalf("missing at startup: %s %s", st, reason)
	}
}

func strings64(prefix string) string {
	b := []byte(prefix)
	for len(b) < 64 {
		b = append(b, 'c')
	}
	return string(b)
}

// TestPlaybackPausesWithoutStorage: with the volume gone the worker stops
// and records nothing; the copy stays pending, its attempts untouched.
func TestPlaybackPausesWithoutStorage(t *testing.T) {
	e, fake, _ := newPlaybackEnv(t)
	movie := pbt.Movie(3840, 2160, 40_000, 1)
	e.uploadMovie(t, "owner", e.family, movie, "a.mov")
	if err := os.Remove(filepath.Join(e.Root, storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if n, err := e.API.ProcessPlayback(t.Context()); err == nil || n != 0 {
		t.Fatalf("without storage: %d, %v", n, err)
	}
	if err := e.API.PreparePlayback(t.Context()); err == nil {
		t.Fatal("prepared without storage")
	}
	if st, _, att := e.playbackStatus(t, shaHex(movie)); st != "pending" || att != 0 || fake.Calls() != 0 {
		t.Fatalf("without storage %s %d (%d calls)", st, att, fake.Calls())
	}
}

func itoa(n int) string {
	return string(appendInt(nil, n))
}

func appendInt(b []byte, n int) []byte {
	if n >= 10 {
		b = appendInt(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

// TestRunPlayback: the running worker picks up an upload when woken and
// stops with its context.
func TestRunPlayback(t *testing.T) {
	e, _, _ := newPlaybackEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		e.API.RunPlayback(ctx)
		close(done)
	}()
	movie := pbt.Movie(3840, 2160, 40_000, 3)
	e.uploadMovie(t, "owner", e.family, movie, "a.mov")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, _, _ := e.playbackStatus(t, shaHex(movie)); st == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker did not make the copy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}
}
