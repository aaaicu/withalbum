package httpapi_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/httpapi"
	"github.com/aaaicu/withalbum/internal/integrity"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

const liveID = "8C1D5A0E-4B2F-4F7A-9E3D-1A2B3C4D5E6F"

// livePhoto is a still and its video as a client would send them.
type livePhoto struct{ still, video []byte }

func newLive(id string, seed int) livePhoto {
	return livePhoto{still: lpt.JPEG(id, seed), video: lpt.MOV(id, seed)}
}

func (lp livePhoto) body(t *testing.T) []byte {
	return []byte(fmt.Sprintf(`{"kind":"live_photo","components":[{"role":"still","bytes":%d,"sha256":%q,"filename":"IMG_0001.JPG"},`+
		`{"role":"paired_video","bytes":%d,"sha256":%q,"filename":"IMG_0001.MOV"}]}`, len(lp.still), shaHex(lp.still), len(lp.video), shaHex(lp.video)))
}

func (e *aclEnv) bundlePath(folder, id string) string {
	return "/api/v1/folders/" + folder + "/bundles/" + id
}

func (e *aclEnv) createBundle(t *testing.T, login, folder, key string, lp livePhoto) reply {
	t.Helper()
	return e.as(t, login, "POST", "/api/v1/folders/"+folder+"/bundles", lp.body(t),
		map[string]string{"Content-Type": "application/json", idemKey: key, "X-Device-ID": "phone"})
}

// openBundle creates a bundle and returns its id and the upload ids of
// the still and the video.
func (e *aclEnv) openBundle(t *testing.T, login, folder, key string, lp livePhoto) (string, string, string) {
	t.Helper()
	m := wantJSON(t, "create bundle", e.createBundle(t, login, folder, key, lp), 201)
	comps := m["components"].([]any)
	if len(comps) != 2 || m["status"] != "open" || m["kind"] != "live_photo" {
		t.Fatalf("bundle %v", m)
	}
	ids := map[string]string{}
	for _, c := range comps {
		c := c.(map[string]any)
		if c["offset"] != float64(0) || c["max_chunk_bytes"] == nil {
			t.Fatalf("component %v", c)
		}
		ids[c["role"].(string)] = c["upload_id"].(string)
	}
	return m["bundle_id"].(string), ids["still"], ids["paired_video"]
}

func (e *aclEnv) sendPart(t *testing.T, login, folder, uploadID string, body []byte) {
	t.Helper()
	r := e.as(t, login, "PATCH", "/api/v1/folders/"+folder+"/uploads/"+uploadID, body,
		map[string]string{"Upload-Offset": "0", "X-Chunk-SHA256": shaHex(body)})
	m := wantJSON(t, "chunk", r, 200)
	if m["offset"] != float64(len(body)) || m["role"] == nil || m["bundle_id"] == nil {
		t.Fatalf("after chunk %v", m)
	}
}

func (e *aclEnv) completeBundle(t *testing.T, login, folder, id string) reply {
	t.Helper()
	return e.as(t, login, "POST", e.bundlePath(folder, id)+"/complete", nil, nil)
}

// uploadLive runs a whole bundle upload and returns the completion reply.
func (e *aclEnv) uploadLive(t *testing.T, login, folder, key string, lp livePhoto) reply {
	t.Helper()
	bid, s, v := e.openBundle(t, login, folder, key, lp)
	e.sendPart(t, login, folder, s, lp.still)
	e.sendPart(t, login, folder, v, lp.video)
	return e.completeBundle(t, login, folder, bid)
}

// visible counts what a commit makes visible: assets, events and receipts.
func (e *aclEnv) visible(t *testing.T) string {
	t.Helper()
	return fmt.Sprint(e.count(t, "SELECT count(*) FROM assets"), e.count(t, "SELECT count(*) FROM folder_changes"),
		e.count(t, "SELECT count(*) FROM upload_receipts"), e.count(t, "SELECT count(*) FROM asset_components"))
}

// TestLivePhotoRoundTrip: create, both chunks, complete; the asset is a
// live photo whose original is the still and whose component downloads
// byte for byte (with ranges); retries replay the result.
func TestLivePhotoRoundTrip(t *testing.T) {
	e := newACLEnv(t)
	f := e.family
	lp := newLive(liveID, 1)
	before := e.visible(t)
	bid, s, v := e.openBundle(t, "contributor", f, "live-1", lp)
	// A retried creation returns the same bundle.
	again := wantJSON(t, "create again", e.createBundle(t, "contributor", f, "live-1", lp), 200)
	if again["bundle_id"] != bid {
		t.Fatalf("retried creation %v", again)
	}
	other := lp
	other.video = lpt.MOV(liveID, 2)
	wantError(t, "same key, other video", e.createBundle(t, "contributor", f, "live-1", other), 422, "idempotency_key_reused")
	wantError(t, "single upload with the bundle key", e.upload(t, "contributor", f, lp.still, "live-1"), 422, "idempotency_key_reused")

	// Only the still: nothing is visible, the bundle says what is missing.
	e.sendPart(t, "contributor", f, s, lp.still)
	r := e.completeBundle(t, "contributor", f, bid)
	m := wantJSON(t, "incomplete", r, 409)
	if m["error"] != "upload_incomplete" || len(m["components"].([]any)) != 2 {
		t.Fatalf("incomplete reply %v", m)
	}
	if got := e.visible(t); got != before {
		t.Fatalf("visible after a partial bundle: %s, before %s", got, before)
	}
	// A component session is completed and cancelled through its bundle.
	wantJSON(t, "complete component", e.as(t, "contributor", "POST", "/api/v1/folders/"+f+"/uploads/"+s+"/complete", nil, nil), 409)
	wantJSON(t, "cancel component", e.as(t, "contributor", "DELETE", "/api/v1/folders/"+f+"/uploads/"+s, nil, nil), 409)
	st := wantJSON(t, "state", e.as(t, "contributor", "GET", e.bundlePath(f, bid), nil, nil), 200)
	if c := st["components"].([]any); c[0].(map[string]any)["offset"] != float64(len(lp.still)) || c[1].(map[string]any)["offset"] != float64(0) {
		t.Fatalf("state %v", st)
	}

	e.sendPart(t, "contributor", f, v, lp.video)
	r = e.completeBundle(t, "contributor", f, bid)
	m = wantJSON(t, "complete", r, 201)
	comps := m["components"].([]any)
	if m["result"] != "created" || m["media_kind"] != "live_photo" || m["pairing"] != "verified" || m["sha256"] != shaHex(lp.still) ||
		len(comps) != 1 || comps[0].(map[string]any)["sha256"] != shaHex(lp.video) || comps[0].(map[string]any)["role"] != "paired_video" ||
		m["preview_status"] != "ready" {
		t.Fatalf("complete reply %v", m)
	}
	aid := m["asset_id"].(string)
	// Replays: completing again, and creating again with the key.
	r = e.completeBundle(t, "contributor", f, bid)
	if rm := wantJSON(t, "complete again", r, 201); rm["asset_id"] != aid || r.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replayed completion %v %v", rm, r.Header)
	}
	if _, err := e.Meta.DB().Exec("DELETE FROM upload_bundles"); err != nil {
		t.Fatal(err)
	}
	r = e.createBundle(t, "contributor", f, "live-1", lp)
	if rm := wantJSON(t, "create after commit", r, 201); rm["asset_id"] != aid || rm["result"] != "created" || r.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("create after commit %v", rm)
	}
	wantError(t, "other video after commit", e.createBundle(t, "contributor", f, "live-1", other), 422, "idempotency_key_reused")

	// The reader's view.
	as := e.snapshotAssets(t, "viewer")[aid]
	if as["media_kind"] != "live_photo" || as["pairing"] != "verified" || as["sha256"] != shaHex(lp.still) {
		t.Fatalf("snapshot asset %v", as)
	}
	c := as["components"].([]any)[0].(map[string]any)
	if c["sha256"] != shaHex(lp.video) || c["bytes"] != float64(len(lp.video)) || c["mime_type"] != "video/quicktime" || c["filename"] != "IMG_0001.MOV" {
		t.Fatalf("component %v", c)
	}
	if e.snapshotAssets(t, "viewer")[e.photo]["components"] != nil || e.snapshotAssets(t, "viewer")[e.photo]["pairing"] != nil {
		t.Fatal("a plain photo shows live photo fields")
	}
	page := syncPageAs(t, e, "viewer", f)
	if !strings.Contains(string(page), `"kind":"asset_added","asset_id":"`+aid) || !strings.Contains(string(page), `"paired_video"`) {
		t.Fatalf("sync page %s", page)
	}
	base := "/api/v1/folders/" + f + "/assets/" + aid
	if o := e.as(t, "viewer", "GET", base+"/original", nil, nil); o.Status != 200 || !bytes.Equal(o.Body, lp.still) {
		t.Fatalf("original %d", o.Status)
	}
	dl := e.as(t, "viewer", "GET", base+"/components/paired_video", nil, nil)
	if dl.Status != 200 || !bytes.Equal(dl.Body, lp.video) || dl.Header.Get("Content-Type") != "video/quicktime" ||
		!strings.HasPrefix(dl.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(dl.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("component download %d %v", dl.Status, dl.Header)
	}
	part := e.as(t, "viewer", "GET", base+"/components/paired_video", nil, map[string]string{"Range": "bytes=10-19"})
	if part.Status != 206 || !bytes.Equal(part.Body, lp.video[10:20]) || part.Header.Get("Content-Range") != "bytes 10-19/"+strconv.Itoa(len(lp.video)) {
		t.Fatalf("range %d %v", part.Status, part.Header)
	}
	wantStatusReply(t, "range past the end", e.as(t, "viewer", "GET", base+"/components/paired_video", nil, map[string]string{"Range": "bytes=99999-"}), 416)
	wantError(t, "unknown role", e.as(t, "viewer", "GET", base+"/components/still", nil, nil), 404, "component_not_found")
	wantError(t, "plain photo", e.as(t, "viewer", "GET", "/api/v1/folders/"+f+"/assets/"+e.photo+"/components/paired_video", nil, nil), 404, "component_not_found")
	wantError(t, "uploader", e.as(t, "uploader", "GET", base+"/components/paired_video", nil, nil), 403, "forbidden")
	wantError(t, "outsider", e.as(t, "outsider", "GET", base+"/components/paired_video", nil, nil), 404, "folder_not_found")
	wantError(t, "other folder", e.as(t, "root", "GET", "/api/v1/folders/"+e.adminF+"/assets/"+aid+"/components/paired_video", nil, nil), 404, "asset_not_found")

	// The duplicate check names the paired video.
	ck := wantJSON(t, "check", e.as(t, "viewer", "POST", "/api/v1/folders/"+f+"/assets/check",
		[]byte(`{"sha256":["`+shaHex(lp.still)+`","`+shaHex(e.pic)+`","`+shaHex(lp.video)+`"]}`), map[string]string{"Content-Type": "application/json"}), 200)
	present := ck["present"].([]any)
	if len(present) != 2 || present[0].(map[string]any)["paired_sha256"] != shaHex(lp.video) || present[1].(map[string]any)["paired_sha256"] != nil {
		t.Fatalf("check %v", ck)
	}

	// Verify reports the video of the live photo when it is damaged.
	vp := filepath.Join(e.Root, ".system", "pool", shaHex(lp.video)[:2], shaHex(lp.video))
	if err := os.Chmod(vp, 0600); err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte{}, lp.video...)
	damaged[len(damaged)-1] ^= 1
	if err := os.WriteFile(vp, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := integrity.Run(t.Context(), e.Volume, e.Meta.DB(), integrity.Options{Hash: true})
	if err != nil || rep.Healthy() || rep.HashMismatch != 1 || len(rep.Problems) != 1 {
		t.Fatalf("verify: %v %+v", err, rep)
	}
	if refs := rep.Problems[0].Assets; len(refs) != 1 || refs[0].AssetID != aid || refs[0].Role != "paired_video" || refs[0].FolderID != f {
		t.Fatalf("verify names %+v", refs)
	}
	if _, err := e.Meta.DB().Exec("DELETE FROM asset_components"); err != nil {
		t.Fatal(err)
	}
	if rep, err := integrity.Run(t.Context(), e.Volume, e.Meta.DB(), integrity.Options{}); err != nil || rep.LivePhotosWithoutVideo != 1 {
		t.Fatalf("live photo without video: %v %+v", err, rep)
	}

	// Removing the asset removes the live photo as a whole.
	wantStatusReply(t, "remove", e.as(t, "owner", "DELETE", base, nil, nil), 204)
	wantError(t, "component after removal", e.as(t, "viewer", "GET", base+"/components/paired_video", nil, nil), 404, "asset_not_found")
}

func wantStatusReply(t *testing.T, what string, r reply, status int) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s: status %d, want %d: %s", what, r.Status, status, r.Body)
	}
}

func syncPageAs(t *testing.T, e *aclEnv, login, folder string) []byte {
	t.Helper()
	r := e.as(t, login, "GET", "/api/v1/folders/"+folder+"/sync", nil, nil)
	wantStatusReply(t, "sync", r, 200)
	return r.Body
}

// TestLivePhotoVerification: a damaged component restarts only itself,
// mismatched identifiers and wrong types commit nothing and keep the
// bundle open, missing identifiers commit as unverified.
func TestLivePhotoVerification(t *testing.T) {
	e := newACLEnv(t)
	f := e.family
	before := e.visible(t)

	// The video arrives damaged (no chunk hash): only it starts over.
	lp := newLive(liveID, 3)
	bid, s, v := e.openBundle(t, "owner", f, "damaged", lp)
	e.sendPart(t, "owner", f, s, lp.still)
	bad := append([]byte{}, lp.video...)
	bad[3] ^= 0xFF
	wantJSON(t, "damaged chunk", e.as(t, "owner", "PATCH", "/api/v1/folders/"+f+"/uploads/"+v, bad, map[string]string{"Upload-Offset": "0"}), 200)
	m := wantJSON(t, "complete damaged", e.completeBundle(t, "owner", f, bid), 422)
	if m["error"] != "hash_mismatch" || m["role"] != "paired_video" || m["upload_id"] != v {
		t.Fatalf("damaged reply %v", m)
	}
	st := wantJSON(t, "state", e.as(t, "owner", "GET", e.bundlePath(f, bid), nil, nil), 200)
	if c := st["components"].([]any); c[0].(map[string]any)["offset"] != float64(len(lp.still)) || c[1].(map[string]any)["offset"] != float64(0) {
		t.Fatalf("after damaged video %v", st)
	}
	if got := e.visible(t); got != before {
		t.Fatalf("visible after a damaged bundle: %s", got)
	}
	e.sendPart(t, "owner", f, v, lp.video)
	wantJSON(t, "complete repaired", e.completeBundle(t, "owner", f, bid), 201)
	before = e.visible(t)

	// Identifiers of two different Live Photos.
	mixed := livePhoto{still: lpt.JPEG(liveID, 4), video: lpt.MOV("OTHER-ID", 4)}
	bid, s, v = e.openBundle(t, "owner", f, "mixed", mixed)
	e.sendPart(t, "owner", f, s, mixed.still)
	e.sendPart(t, "owner", f, v, mixed.video)
	wantError(t, "mismatch", e.completeBundle(t, "owner", f, bid), 422, "pairing_mismatch")
	wantJSON(t, "still open", e.as(t, "owner", "GET", e.bundlePath(f, bid), nil, nil), 200)
	if got := e.visible(t); got != before {
		t.Fatalf("visible after a mismatch: %s, before %s", got, before)
	}
	parts := countFilesUnder(t, filepath.Join(e.Root, ".system", "uploads"))
	wantStatusReply(t, "cancel", e.as(t, "owner", "DELETE", e.bundlePath(f, bid), nil, nil), 204)
	if n := countFilesUnder(t, filepath.Join(e.Root, ".system", "uploads")); n != parts-2 {
		t.Fatalf("partial files after cancel: %d, before %d", n, parts)
	}
	wantError(t, "cancelled", e.as(t, "owner", "GET", e.bundlePath(f, bid), nil, nil), 404, "bundle_not_found")

	// Wrong types: a JPEG as the video.
	wrong := livePhoto{still: lpt.JPEG(liveID, 5), video: lpt.JPEG(liveID, 6)}
	bid, s, v = e.openBundle(t, "owner", f, "wrong", wrong)
	e.sendPart(t, "owner", f, s, wrong.still)
	e.sendPart(t, "owner", f, v, wrong.video)
	m = wantJSON(t, "wrong type", e.completeBundle(t, "owner", f, bid), 415)
	if m["error"] != "unsupported_component_type" || m["role"] != "paired_video" {
		t.Fatalf("wrong type %v", m)
	}
	if got := e.visible(t); got != before {
		t.Fatalf("visible after a wrong type: %s", got)
	}

	// No identifier in the video: kept, as unverified. A HEIC still.
	plain := livePhoto{still: lpt.HEIC(liveID, 7), video: lpt.MOV("", 7)}
	m = wantJSON(t, "unverified", e.uploadLive(t, "owner", f, "plain", plain), 201)
	if m["pairing"] != "unverified" || m["media_kind"] != "live_photo" {
		t.Fatalf("unverified reply %v", m)
	}
	var pid any
	if err := e.Meta.DB().QueryRow("SELECT pairing_id FROM asset_components WHERE sha256 = ?", shaHex(plain.video)).Scan(&pid); err != nil || pid != nil {
		t.Fatalf("pairing id of an unverified pair %v, %v", pid, err)
	}

	// Request validation.
	hdr := map[string]string{"Content-Type": "application/json", idemKey: "v"}
	for body, code := range map[string]string{
		`{"kind":"burst","components":[]}`: "invalid_kind",
		`{"kind":"live_photo","components":[{"role":"still","bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"}]}`:                                                                              "invalid_components",
		`{"kind":"live_photo","components":[{"role":"still","bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"},{"role":"still","bytes":1,"sha256":"` + strings.Repeat("b", 64) + `"}]}`:        "invalid_components",
		`{"kind":"live_photo","components":[{"role":"still","bytes":0,"sha256":"` + strings.Repeat("a", 64) + `"},{"role":"paired_video","bytes":1,"sha256":"` + strings.Repeat("b", 64) + `"}]}`: "invalid_bytes",
		`{"kind":"live_photo","components":[{"role":"still","bytes":1,"sha256":"xyz"},{"role":"paired_video","bytes":1,"sha256":"` + strings.Repeat("b", 64) + `"}]}`:                             "invalid_sha256",
		`{"kind":"live_photo","components":[{"role":"thumb","bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"},{"role":"paired_video","bytes":1,"sha256":"` + strings.Repeat("b", 64) + `"}]}`: "invalid_components",
	} {
		wantError(t, body, e.as(t, "owner", "POST", "/api/v1/folders/"+f+"/bundles", []byte(body), hdr), 400, code)
	}
	big := `{"kind":"live_photo","components":[{"role":"still","bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"},{"role":"paired_video","bytes":` +
		strconv.FormatInt(512<<20+1, 10) + `,"sha256":"` + strings.Repeat("b", 64) + `"}]}`
	wantError(t, "video too large", e.as(t, "owner", "POST", "/api/v1/folders/"+f+"/bundles", []byte(big), hdr), 413, "file_too_large")
	wantError(t, "no key", e.as(t, "owner", "POST", "/api/v1/folders/"+f+"/bundles", lp.body(t), map[string]string{"Content-Type": "application/json"}), 400, "idempotency_key_required")
}

// TestLivePhotoDuplicates: a still-only asset is upgraded in place, the
// same Live Photo again is already present, the same still with another
// video is a conflict for readers and a new asset (with the same 202) for
// an UPLOADER.
func TestLivePhotoDuplicates(t *testing.T) {
	e := newACLEnv(t)
	f := e.family
	lp := newLive(liveID, 8)
	single := wantJSON(t, "still alone", e.upload(t, "contributor", f, lp.still, "still-only"), 201)
	aid := single["asset_id"].(string)
	events := e.events(t, f)
	m := wantJSON(t, "upgrade", e.uploadLive(t, "contributor", f, "upgrade", lp), 200)
	if m["result"] != "upgraded" || m["asset_id"] != aid || m["media_kind"] != "live_photo" || len(m["components"].([]any)) != 1 {
		t.Fatalf("upgrade %v", m)
	}
	page, err := e.Meta.ListEvents(t.Context(), f, int64(events), 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != "asset_updated" || page.Events[0].AssetID != aid ||
		page.Assets[aid].PairedVideo.SHA256 != shaHex(lp.video) || page.Events[0].UploaderDeviceID != "" {
		t.Fatalf("upgrade event %+v %v", page, err)
	}
	if n := e.count(t, "SELECT count(*) FROM audit_log WHERE action = 'asset.attach_component' AND target_id = ?", aid); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	// The same Live Photo under another key.
	m = wantJSON(t, "again", e.uploadLive(t, "contributor", f, "again", lp), 200)
	if m["result"] != "already_present" || m["asset_id"] != aid {
		t.Fatalf("already present %v", m)
	}
	// The same still with another video.
	other := livePhoto{still: lp.still, video: lpt.MOV(liveID, 9)}
	before := e.visible(t)
	wantError(t, "conflict", e.uploadLive(t, "contributor", f, "conflict", other), 409, "component_conflict")
	if got := e.visible(t); got != before {
		t.Fatalf("visible after a conflict: %s, before %s", got, before)
	}
	if b, _ := os.ReadFile(filepath.Join(e.Root, ".system", "pool", shaHex(lp.video)[:2], shaHex(lp.video))); !bytes.Equal(b, lp.video) {
		t.Fatal("the existing video changed")
	}

	// UPLOADER: the same receipt for new, upgraded, present and conflicting.
	plainStill := lpt.JPEG("UP-1", 10)
	wantJSON(t, "owner still", e.upload(t, "owner", f, plainStill, "owner-still"), 201)
	cases := []struct {
		name string
		lp   livePhoto
	}{
		{"new", newLive("UP-2", 11)},
		{"upgrade", livePhoto{still: plainStill, video: lpt.MOV("UP-1", 10)}},
		{"present", lp},
		{"conflict", other},
	}
	for i, c := range cases {
		assets := e.count(t, "SELECT count(*) FROM assets")
		r := e.uploadLive(t, "uploader", f, "up-"+strconv.Itoa(i), c.lp)
		m := wantJSON(t, c.name, r, 202)
		want := fmt.Sprint(map[string]any{"result": "accepted", "folder_id": f, "sha256": shaHex(c.lp.still), "bytes": float64(len(c.lp.still)),
			"components": []any{map[string]any{"role": "paired_video", "sha256": shaHex(c.lp.video), "bytes": float64(len(c.lp.video))}}})
		if fmt.Sprint(m) != want {
			t.Fatalf("%s: receipt %v, want %v", c.name, m, want)
		}
		hdr := slices.Sorted(func(yield func(string) bool) {
			for k := range r.Header {
				if !yield(k) {
					return
				}
			}
		})
		if strings.Join(hdr, ",") != "Cache-Control,Content-Security-Policy,Content-Type,Cross-Origin-Resource-Policy,Pragma,Referrer-Policy,X-Content-Type-Options,X-Frame-Options" {
			t.Fatalf("%s: headers %v", c.name, hdr)
		}
		// The conflicting one is kept as an asset of its own.
		grew := e.count(t, "SELECT count(*) FROM assets") - assets
		if want := map[string]int{"new": 1, "upgrade": 0, "present": 0, "conflict": 1}[c.name]; grew != want {
			t.Fatalf("%s: %d new assets, want %d", c.name, grew, want)
		}
	}
	// The kept conflicting Live Photo is a second asset with the same still.
	if n := e.count(t, "SELECT count(*) FROM assets WHERE original_sha256 = ?", shaHex(lp.still)); n != 2 {
		t.Fatalf("assets with the still: %d", n)
	}
}

// TestLivePhotoAccess: who may create and see bundles.
func TestLivePhotoAccess(t *testing.T) {
	e := newACLEnv(t)
	f := e.family
	lp := newLive(liveID, 12)
	wantError(t, "viewer", e.createBundle(t, "viewer", f, "k", lp), 403, "forbidden")
	wantError(t, "outsider", e.createBundle(t, "outsider", f, "k", lp), 404, "folder_not_found")
	wantError(t, "admin", e.createBundle(t, "root", f, "k", lp), 404, "folder_not_found")
	bid, _, _ := e.openBundle(t, "uploader", f, "k", lp)
	// Another device of the same account, another account: not found.
	e.tokens["uploader2"] = loginToken(t, e.Raw, "uploader", userPassword)
	for _, login := range []string{"uploader2", "contributor"} {
		wantError(t, login, e.as(t, login, "GET", e.bundlePath(f, bid), nil, nil), 404, "bundle_not_found")
		wantError(t, login, e.completeBundle(t, login, f, bid), 404, "bundle_not_found")
		wantError(t, login, e.as(t, login, "DELETE", e.bundlePath(f, bid), nil, nil), 404, "bundle_not_found")
	}
	wantError(t, "bad id", e.as(t, "uploader", "GET", e.bundlePath(f, "nothex"), nil, nil), 404, "bundle_not_found")
	// Losing the role during the upload: refused at the commit.
	e.setMember(t, "uploader", "VIEWER")
	wantError(t, "demoted", e.completeBundle(t, "uploader", f, bid), 403, "forbidden")
	e.setMember(t, "uploader", "UPLOADER")
	me := wantJSON(t, "me", e.as(t, "uploader", "GET", "/api/v1/me", nil, nil), 200)
	if fmt.Sprint(me["server_features"]) != "[live_photo_bundles folder_create_idempotency asset_duration]" {
		t.Fatalf("server_features %v", me["server_features"])
	}
}

// TestLivePhotoCommitFaults: a failure at any point before COMMIT leaves
// nothing visible and the bundle open; completing again commits once.
func TestLivePhotoCommitFaults(t *testing.T) {
	e := newACLEnv(t)
	f := e.family
	for i, point := range []string{httpapi.FaultAfterPromote, httpapi.FaultAfterPreview, httpapi.FaultBeforeCommit, httpapi.FaultAfterCommit} {
		lp := newLive(fmt.Sprint("FAULT-", i), 20+i)
		bid, s, v := e.openBundle(t, "owner", f, "fault-"+point, lp)
		e.sendPart(t, "owner", f, s, lp.still)
		e.sendPart(t, "owner", f, v, lp.video)
		before := e.visible(t)
		e.faults.set(point, func() error { return errors.New("injected") })
		r := e.completeBundle(t, "owner", f, bid)
		e.faults.set(point, nil)
		if r.Status != 500 {
			t.Fatalf("%s: status %d %s", point, r.Status, r.Body)
		}
		if point != httpapi.FaultAfterCommit {
			if got := e.visible(t); got != before {
				t.Fatalf("%s: visible %s, before %s", point, got, before)
			}
		}
		// Retry: committed once (after_commit: replayed).
		m := wantJSON(t, point+" retry", e.completeBundle(t, "owner", f, bid), 201)
		if n := e.count(t, "SELECT count(*) FROM assets WHERE asset_id = ?", m["asset_id"]); n != 1 {
			t.Fatalf("%s: asset rows %d", point, n)
		}
		if n := e.count(t, "SELECT count(*) FROM folder_changes WHERE asset_id = ?", m["asset_id"]); n != 1 {
			t.Fatalf("%s: events %d", point, n)
		}
	}
}

// TestLivePhotoExpiryAndRecovery: an abandoned bundle is pruned with both
// sessions and partial files; a chunk on one component keeps the other
// alive; a component whose bytes are lost at startup starts over alone.
func TestLivePhotoExpiryAndRecovery(t *testing.T) {
	e := newACLEnv(t)
	f := e.family
	lp := newLive(liveID, 30)
	bid, s, v := e.openBundle(t, "owner", f, "abandoned", lp)
	e.sendPart(t, "owner", f, s, lp.still)
	uploads := filepath.Join(e.Root, ".system", "uploads")
	// Recovery: the still's partial file is lost.
	if err := os.Remove(filepath.Join(uploads, s+".part")); err != nil {
		t.Fatal(err)
	}
	half := lp.video[:len(lp.video)/2]
	wantJSON(t, "half video", e.as(t, "owner", "PATCH", "/api/v1/folders/"+f+"/uploads/"+v, half, map[string]string{"Upload-Offset": "0"}), 200)
	rec, err := e.API.RecoverUploads(t.Context())
	if err != nil || rec.Restarted != 1 || rec.Dropped != 0 {
		t.Fatalf("recover %+v %v", rec, err)
	}
	st := wantJSON(t, "state", e.as(t, "owner", "GET", e.bundlePath(f, bid), nil, nil), 200)
	if c := st["components"].([]any); c[0].(map[string]any)["offset"] != float64(0) || c[1].(map[string]any)["offset"] != float64(len(half)) {
		t.Fatalf("after recovery %v", st)
	}
	// Not expired yet 23 hours on; gone after 25.
	if n, err := e.API.PruneUploads(t.Context(), time.Now().Add(23*time.Hour)); err != nil || n != 0 {
		t.Fatalf("early prune %d %v", n, err)
	}
	n, err := e.API.PruneUploads(t.Context(), time.Now().Add(25*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("prune %d %v", n, err)
	}
	if e.count(t, "SELECT count(*) FROM upload_bundles")+e.count(t, "SELECT count(*) FROM upload_sessions") != 0 {
		t.Fatal("rows left after prune")
	}
	if _, err := os.Stat(filepath.Join(uploads, v+".part")); !os.IsNotExist(err) {
		t.Fatalf("partial file left: %v", err)
	}
	// Expiry moves with the components.
	bid, s, _ = e.openBundle(t, "owner", f, "slow", lp)
	if _, err := e.Meta.DB().Exec("UPDATE upload_bundles SET expires_at = '2000-01-01T00:00:00.000000000Z' WHERE bundle_id = ?", bid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Meta.DB().Exec("UPDATE upload_sessions SET expires_at = '2000-01-01T00:00:00.000000000Z' WHERE bundle_id = ? AND upload_id <> ?", bid, s); err != nil {
		t.Fatal(err)
	}
	e.sendPart(t, "owner", f, s, lp.still)
	if n := e.count(t, "SELECT count(*) FROM upload_sessions WHERE bundle_id = ? AND expires_at > '2001'", bid); n != 2 {
		t.Fatalf("component sessions extended: %d", n)
	}
	wantJSON(t, "bundle extended", e.as(t, "owner", "GET", e.bundlePath(f, bid), nil, nil), 200)
}
