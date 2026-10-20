package httpapi_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aaaicu/withalbum/internal/storage"
	"github.com/aaaicu/withalbum/internal/users"
)

// withExif returns jpg (a JPEG from jpegBytes) with an EXIF APP1 segment
// holding DateTimeOriginal dt and, if set, OffsetTimeOriginal off. Built
// by hand: no real photo is part of the tests.
func withExif(t *testing.T, jpg []byte, dt, off string) []byte {
	t.Helper()
	if len(jpg) < 2 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		t.Fatal("not a JPEG")
	}
	type ent struct {
		tag uint16
		val string
	}
	ents := []ent{{0x9003, dt}}
	if off != "" {
		ents = append(ents, ent{0x9011, off})
	}
	var b bytes.Buffer
	bo := binary.BigEndian
	w16 := func(v uint16) { _ = binary.Write(&b, bo, v) }
	w32 := func(v uint32) { _ = binary.Write(&b, bo, v) }
	b.WriteString("MM")
	w16(42)
	w32(8)
	exifOff := uint32(8 + 2 + 12 + 4)
	w16(1)
	w16(0x8769)
	w16(4)
	w32(1)
	w32(exifOff)
	w32(0)
	dataOff := exifOff + 2 + uint32(len(ents))*12 + 4
	var data bytes.Buffer
	w16(uint16(len(ents)))
	for _, e := range ents {
		v := append([]byte(e.val), 0)
		w16(e.tag)
		w16(2)
		w32(uint32(len(v)))
		w32(dataOff + uint32(data.Len()))
		data.Write(v)
	}
	w32(0)
	b.Write(data.Bytes())
	payload := append([]byte("Exif\x00\x00"), b.Bytes()...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	out := append([]byte{0xFF, 0xD8}, seg...)
	out = append(out, payload...)
	return append(out, jpg[2:]...)
}

// snapshotAssets returns the folder's assets by id, as login sees them.
func (e *aclEnv) snapshotAssets(t *testing.T, login string) map[string]map[string]any {
	t.Helper()
	m := wantJSON(t, "snapshot", e.as(t, login, "GET", "/api/v1/folders/"+e.family+"/assets", nil, nil), 200)
	out := map[string]map[string]any{}
	for _, a := range m["assets"].([]any) {
		am := a.(map[string]any)
		out[am["id"].(string)] = am
	}
	return out
}

// listedFolder returns family as it appears in login's folder list, or nil.
func (e *aclEnv) listedFolder(t *testing.T, login string) map[string]any {
	t.Helper()
	for _, item := range wantJSON(t, "list", e.as(t, login, "GET", "/api/v1/folders", nil, nil), 200)["folders"].([]any) {
		if m := item.(map[string]any); m["id"] == e.family {
			return m
		}
	}
	return nil
}

func processCaptures(t *testing.T, e *aclEnv) int {
	t.Helper()
	total := 0
	for {
		n, err := e.API.ProcessCaptures(t.Context())
		if err != nil {
			t.Fatalf("ProcessCaptures: %v", err)
		}
		total += n
		if n == 0 {
			return total
		}
	}
}

// TestAssetMetadataExposure: captured_at, uploaded_by_name and
// member_count by relation to the folder. Readers (OWNER, CONTRIBUTOR,
// VIEWER) get them; an UPLOADER gets none of them (no member_count, still
// 403 for every read, the same 202 receipt); an outsider and the server
// administrator (not a member) still get 404.
func TestAssetMetadataExposure(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	shot := withExif(t, jpegBytes(t, 20, 10), "2026:10:19 14:03:22", "+09:00")
	shotID := wantJSON(t, "contributor upload", e.as(t, "contributor", "POST", f+"/assets", shot, map[string]string{"X-Filename": "shot.jpg"}), 201)["asset_id"].(string)

	// Before the worker ran: unknown, never the upload time.
	before := e.snapshotAssets(t, "owner")
	if a := before[shotID]; a["captured_at"] != nil || a["uploaded_by_name"] != "contributor" {
		t.Fatalf("before the worker: %v", a)
	}
	eventsBefore := e.events(t, e.family)
	if n := processCaptures(t, e); n != 3 { // owner's photo, admin's photo, the shot
		t.Fatalf("settled %d originals, want 3", n)
	}
	// One asset_updated, for the only original with a capture time.
	if n := e.events(t, e.family); n != eventsBefore+1 {
		t.Fatalf("events %d -> %d, want one asset_updated", eventsBefore, n)
	}
	if n := e.events(t, e.adminF); n != 1 {
		t.Fatalf("admin folder got events: %d", n)
	}

	for _, login := range []string{"owner", "contributor", "viewer"} {
		t.Run("reader/"+login, func(t *testing.T) {
			m := wantJSON(t, "folder", e.as(t, login, "GET", f, nil, nil), 200)
			wantKeys(t, "folder", m, folderKeys...)
			if m["member_count"] != float64(4) {
				t.Fatalf("member_count %v, want 4 (owner and three members)", m["member_count"])
			}
			if l := e.listedFolder(t, login); l == nil || l["member_count"] != float64(4) {
				t.Fatalf("listed folder %v", l)
			}
			as := e.snapshotAssets(t, login)
			if a := as[shotID]; a["captured_at"] != "2026-10-19T14:03:22+09:00" || a["uploaded_by_name"] != "contributor" {
				t.Fatalf("shot %v", a)
			}
			if a := as[e.photo]; a["captured_at"] != nil || a["uploaded_by_name"] != "owner" {
				t.Fatalf("photo without EXIF %v", a)
			}
			page := syncPage(t, asUser(e.Raw, e.tokens[login]), e.family, "")
			evs := page["events"].([]any)
			last := evs[len(evs)-1].(map[string]any)
			if last["kind"] != "asset_updated" || last["asset_id"] != shotID {
				t.Fatalf("last event %v", last)
			}
			a := page["assets"].(map[string]any)[shotID].(map[string]any)
			if a["captured_at"] != "2026-10-19T14:03:22+09:00" || a["uploaded_by_name"] != "contributor" {
				t.Fatalf("feed asset %v", a)
			}
		})
	}

	t.Run("uploader", func(t *testing.T) {
		m := wantJSON(t, "folder", e.as(t, "uploader", "GET", f, nil, nil), 200)
		wantKeys(t, "uploader folder", m, "id", "name", "created_at", "role", "can_read", "can_upload")
		if l := e.listedFolder(t, "uploader"); l == nil {
			t.Fatal("family not listed")
		} else {
			wantKeys(t, "uploader listed folder", l, "id", "name", "created_at", "role", "can_read", "can_upload")
		}
		for _, p := range []string{f + "/assets", f + "/sync", f + "/members", f + "/invitations"} {
			wantError(t, p, e.as(t, "uploader", "GET", p, nil, nil), 403, "forbidden")
		}
		r := e.as(t, "uploader", "POST", f+"/assets", withExif(t, jpegBytes(t, 3, 3), "2025:01:01 00:00:00", ""), nil)
		wantKeys(t, "receipt", wantJSON(t, "upload", r, 202), "result", "folder_id", "sha256", "bytes")
	})

	for _, login := range []string{"outsider", "root"} {
		t.Run("hidden/"+login, func(t *testing.T) {
			for _, p := range []string{f, f + "/assets", f + "/sync", f + "/members"} {
				wantError(t, p, e.as(t, login, "GET", p, nil, nil), 404, "folder_not_found")
			}
			if l := e.listedFolder(t, login); l != nil {
				t.Fatalf("%s lists family: %v", login, l)
			}
		})
	}

	// A pending invitation is not a member; an accepted one is (and the
	// accept response counts the new member).
	inv := e.invite(t, "outsider", "VIEWER")
	if m := wantJSON(t, "folder", e.as(t, "owner", "GET", f, nil, nil), 200); m["member_count"] != float64(4) {
		t.Fatalf("pending invitation counted: %v", m["member_count"])
	}
	acc := wantJSON(t, "accept", e.as(t, "outsider", "POST", "/api/v1/invitations/"+inv+"/accept", nil, nil), 200)
	if acc["member_count"] != float64(5) {
		t.Fatalf("accept response member_count %v, want 5", acc["member_count"])
	}

	// A disabled account: not counted, and its uploads show no name.
	us := users.New(e.Meta.DB())
	if err := us.SetStatus(t.Context(), e.ids["contributor"], users.StatusDisabled, users.Console); err != nil {
		t.Fatal(err)
	}
	if m := wantJSON(t, "folder", e.as(t, "viewer", "GET", f, nil, nil), 200); m["member_count"] != float64(4) {
		t.Fatalf("disabled member counted: %v", m["member_count"])
	}
	if a := e.snapshotAssets(t, "viewer")[shotID]; a["uploaded_by_name"] != nil || a["captured_at"] != "2026-10-19T14:03:22+09:00" {
		t.Fatalf("asset of a disabled uploader %v", a)
	}
	// The name follows the account's current display name.
	if _, err := e.Meta.DB().Exec("UPDATE users SET display_name = 'Mom' WHERE user_id = ?", e.ids["owner"]); err != nil {
		t.Fatal(err)
	}
	if a := e.snapshotAssets(t, "viewer")[e.photo]; a["uploaded_by_name"] != "Mom" {
		t.Fatalf("renamed uploader %v", a)
	}
}

// TestCaptureWorkerAfterUploads: the running worker is woken by a
// single-request upload and by a completed upload session, and the
// capture time reaches the snapshot without any other request.
func TestCaptureWorkerAfterUploads(t *testing.T) {
	e := newACLEnv(t)
	processCaptures(t, e)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.API.RunCaptures(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	f := "/api/v1/folders/" + e.family
	single := withExif(t, jpegBytes(t, 8, 8), "2024:12:31 23:59:59", "-05:00")
	singleID := wantJSON(t, "single", e.as(t, "owner", "POST", f+"/assets", single, nil), 201)["asset_id"].(string)

	c := sessionClient{t: t, h: e.Raw, folder: e.family, hdr: map[string]string{"Authorization": "Bearer " + e.tokens["contributor"]}}
	body := withExif(t, jpegBytes(t, 64, 64), "2023:06:01 08:00:00", "")
	id := wantJSON(t, "create session", c.create("session-1", body, ""), 201)["upload_id"].(string)
	sendAll(t, c, id, body, 0, 100)
	sessionID := wantJSON(t, "complete", c.complete(id), 201)["asset_id"].(string)

	want := map[string]string{singleID: "2024-12-31T23:59:59-05:00", sessionID: "2023-06-01T08:00:00"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		as := e.snapshotAssets(t, "viewer")
		got := map[string]any{singleID: as[singleID]["captured_at"], sessionID: as[sessionID]["captured_at"]}
		if fmt.Sprint(got) == fmt.Sprint(map[string]any{singleID: want[singleID], sessionID: want[sessionID]}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured_at %v, want %v", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCaptureBackfill: originals stored before this feature (pending after
// the migration) are read in the background, with one asset_updated per
// folder holding them; the state survives a restart (it is in the
// database), a missing volume leaves everything pending, and an
// unreadable original is settled as unknown instead of blocking the rest.
func TestCaptureBackfill(t *testing.T) {
	e := newACLEnv(t)
	f := "/api/v1/folders/" + e.family
	shot := withExif(t, jpegBytes(t, 12, 12), "2019:07:04 12:00:00", "+02:00")
	shotID := wantJSON(t, "upload", e.as(t, "owner", "POST", f+"/assets", shot, nil), 201)["asset_id"].(string)
	// The same bytes in a second folder of the same owner.
	other := wantJSON(t, "folder", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"other"}`), nil), 201)["id"].(string)
	wantJSON(t, "upload", e.as(t, "owner", "POST", "/api/v1/folders/"+other+"/assets", shot, nil), 201)
	broken := withExif(t, jpegBytes(t, 5, 5), "2018:01:01 00:00:00", "")
	wantJSON(t, "upload", e.as(t, "owner", "POST", f+"/assets", broken, nil), 201)
	processCaptures(t, e)

	// As a database from before migration 0010 looks after the upgrade.
	db := e.Meta.DB()
	if _, err := db.Exec("UPDATE blobs SET capture_state = 'pending', captured_at = NULL, captured_utc = NULL"); err != nil {
		t.Fatal(err)
	}
	pending := func() int {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM blobs WHERE capture_state = 'pending'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	all := pending()

	// An original that cannot be read (here: its pool file is gone).
	if err := os.Remove(e.Blobs.PoolPath(shaHex(broken))); err != nil {
		t.Fatal(err)
	}
	famBefore, otherBefore := e.events(t, e.family), e.events(t, other)
	if n := processCaptures(t, e); n != all {
		t.Fatalf("settled %d, want %d", n, all)
	}
	if pending() != 0 {
		t.Fatal("originals left pending")
	}
	if famBefore+1 != e.events(t, e.family) || otherBefore+1 != e.events(t, other) {
		t.Fatalf("events family %d->%d, other %d->%d; want one asset_updated each", famBefore, e.events(t, e.family), otherBefore, e.events(t, other))
	}
	if a := e.snapshotAssets(t, "viewer")[shotID]; a["captured_at"] != "2019-07-04T12:00:00+02:00" {
		t.Fatalf("backfilled %v", a)
	}
	var utc string
	if err := db.QueryRow("SELECT captured_utc FROM blobs WHERE sha256 = ?", shaHex(shot)).Scan(&utc); err != nil || utc != "2019-07-04T10:00:00Z" {
		t.Fatalf("captured_utc %q, %v", utc, err)
	}
	// Running again changes nothing.
	if n := processCaptures(t, e); n != 0 || famBefore+1 != e.events(t, e.family) {
		t.Fatalf("second run settled %d", n)
	}
}

// TestCaptureBackfillWithoutStorage: with the volume gone nothing is
// settled and nothing is lost, and the worker keeps refusing after the
// marker is back (the volume stays unavailable until a restart).
func TestCaptureBackfillWithoutStorage(t *testing.T) {
	e := newACLEnv(t)
	shot := withExif(t, jpegBytes(t, 12, 12), "2019:07:04 12:00:00", "+02:00")
	wantJSON(t, "upload", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/assets", shot, nil), 201)
	processCaptures(t, e)
	db := e.Meta.DB()
	if _, err := db.Exec("UPDATE blobs SET capture_state = 'pending', captured_at = NULL, captured_utc = NULL"); err != nil {
		t.Fatal(err)
	}
	pending := func() int {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM blobs WHERE capture_state = 'pending'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	all := pending()
	marker := filepath.Join(e.Root, storage.MarkerName)
	saved, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if n, err := e.API.ProcessCaptures(t.Context()); err == nil || n != 0 {
		t.Fatalf("ProcessCaptures without storage = %d, %v", n, err)
	}
	if err := os.WriteFile(marker, saved, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := e.API.ProcessCaptures(t.Context()); !errors.Is(err, storage.ErrVolumeChanged) || n != 0 {
		t.Fatalf("ProcessCaptures after the marker is back = %d, %v; want the volume to stay unavailable", n, err)
	}
	if pending() != all || all == 0 {
		t.Fatalf("pending %d, want %d: originals settled without storage", pending(), all)
	}
}
