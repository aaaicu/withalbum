package httpapi_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	geo "github.com/aaaicu/withalbum/internal/capturetime/geotest"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

// Camera names are invented ("Testmaker", "TM-1 Pro").

// rotatedMovie is a QuickTime movie with an invented make/model and one
// video track of w×h whose matrix turns it by 90°.
func rotatedMovie(w, h uint32, seed int) []byte {
	be := binary.BigEndian
	keys := [][]byte{{0, 0, 0, 0}, be.AppendUint32(nil, 2)}
	var ilst [][]byte
	for i, kv := range [][2]string{{"com.apple.quicktime.make", "Testmaker"}, {"com.apple.quicktime.model", "TM-1 Pro"}} {
		keys = append(keys, be.AppendUint32(nil, uint32(8+len(kv[0]))), []byte("mdta"), []byte(kv[0]))
		ilst = append(ilst, lpt.Box(string(be.AppendUint32(nil, uint32(i+1))), lpt.Box("data", be.AppendUint32(nil, 1), make([]byte, 4), []byte(kv[1]))))
	}
	meta := lpt.Box("meta", lpt.Box("hdlr", make([]byte, 8), []byte("mdta"), make([]byte, 13)), lpt.Box("keys", keys...), lpt.Box("ilst", ilst...))
	tk := []byte{0, 0, 0, 7}
	tk = append(tk, make([]byte, 36)...)
	for _, v := range []uint32{0, 0x10000, 0, 0xFFFF0000, 0, 0, 0, 0, 0x40000000} {
		tk = be.AppendUint32(tk, v)
	}
	tk = be.AppendUint32(tk, w<<16)
	tk = be.AppendUint32(tk, h<<16)
	mvhd := lpt.Box("mvhd", []byte{0, 0, 0, 0}, make([]byte, 8), be.AppendUint32(nil, 600), be.AppendUint32(nil, 1800), make([]byte, 80))
	return bytes.Join([][]byte{
		lpt.Box("ftyp", []byte("qt  "), make([]byte, 4), []byte("qt  ")),
		lpt.Box("mdat", bytes.Repeat([]byte{byte(seed)}, 32)),
		lpt.Box("moov", mvhd, meta, lpt.Box("trak", lpt.Box("tkhd", tk))),
	}, nil)
}

// TestAssetCamera: the capture worker reads the camera and pixel size
// after the commit (null before, asset_updated after); readers see them in
// the snapshot and the feed; the UPLOADER's receipt is unchanged; the
// backfill reads them again.
func TestAssetCamera(t *testing.T) {
	e := newACLEnv(t)
	processCaptures(t, e)
	f := "/api/v1/folders/" + e.family

	pic := geo.CameraJPEG("Testmaker", "TM-1 Pro", "TM-1 Pro back camera 6.8mm f/1.8", 24, 4032, 3024, 31)
	picID := wantJSON(t, "upload", e.as(t, "contributor", "POST", f+"/assets", pic, map[string]string{"X-Filename": "cam.jpg"}), 201)["asset_id"].(string)
	clipID := wantJSON(t, "clip", e.as(t, "owner", "POST", f+"/assets", rotatedMovie(1920, 1080, 32), map[string]string{"X-Filename": "clip.mov"}), 201)["asset_id"].(string)
	r := e.as(t, "uploader", "POST", f+"/assets", geo.CameraJPEG("Testmaker", "TM-2", "", 26, 10, 10, 33), map[string]string{"X-Filename": "u.jpg"})
	wantKeys(t, "receipt", wantJSON(t, "uploader upload", r, 202), "result", "folder_id", "sha256", "bytes")

	if a := e.snapshotAssets(t, "viewer")[picID]; a["camera"] != nil || a["width"] != nil || a["height"] != nil {
		t.Fatalf("before the worker: %v", a)
	}
	events := e.events(t, e.family)
	processCaptures(t, e)
	if got := e.events(t, e.family); got < events+3 {
		t.Fatalf("events %d -> %d, want at least 3", events, got)
	}
	check := func(what string) {
		t.Helper()
		for _, login := range []string{"owner", "contributor", "viewer"} {
			as := e.snapshotAssets(t, login)
			p := as[picID]
			cam, ok := p["camera"].(map[string]any)
			if !ok {
				t.Fatalf("%s/%s: camera %v", what, login, p["camera"])
			}
			wantKeys(t, "camera", cam, "make", "model", "lens", "focal_35mm")
			if cam["make"] != "Testmaker" || cam["model"] != "TM-1 Pro" || cam["lens"] != "TM-1 Pro back camera 6.8mm f/1.8" || cam["focal_35mm"] != 24.0 {
				t.Fatalf("%s/%s: camera %v", what, login, cam)
			}
			// The encoded frame (8×8) wins over the EXIF pixel size.
			if p["width"] != 8.0 || p["height"] != 8.0 {
				t.Fatalf("%s/%s: size %v×%v", what, login, p["width"], p["height"])
			}
			c := as[clipID]
			cc, _ := c["camera"].(map[string]any)
			if cc == nil || cc["model"] != "TM-1 Pro" || cc["lens"] != nil || cc["focal_35mm"] != nil {
				t.Fatalf("%s/%s: clip camera %v", what, login, c["camera"])
			}
			if c["width"] != 1080.0 || c["height"] != 1920.0 {
				t.Fatalf("%s/%s: rotated clip %v×%v", what, login, c["width"], c["height"])
			}
		}
	}
	check("after the worker")
	page := syncPage(t, asUser(e.Raw, e.tokens["viewer"]), e.family, "")
	if a := page["assets"].(map[string]any)[picID].(map[string]any); a["width"] != 8.0 || a["camera"] == nil {
		t.Fatalf("feed %v", a)
	}
	wantError(t, "uploader snapshot", e.as(t, "uploader", "GET", f+"/assets", nil, nil), 403, "forbidden")

	// Backfill: media info pending again.
	if _, err := e.Meta.DB().Exec(`UPDATE blobs SET media_state = 'pending', camera_make = NULL, camera_model = NULL, camera_lens = NULL,
		camera_focal_35mm = NULL, pixel_width = NULL, pixel_height = NULL`); err != nil {
		t.Fatal(err)
	}
	events = e.events(t, e.family)
	processCaptures(t, e)
	if got := e.events(t, e.family); got < events+3 {
		t.Fatalf("backfill events %d -> %d", events, got)
	}
	check("after the backfill")
	if n := processCaptures(t, e); n != 0 {
		t.Fatalf("second run settled %d", n)
	}
}
