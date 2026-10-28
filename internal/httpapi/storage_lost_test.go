package httpapi_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aaaicu/withalbum/internal/storage"
)

// TestStorageLostStaysUnavailable: after the storage disappeared once, the
// server keeps refusing writes and reports storage "unavailable" even when
// the disk is back (found by scripts/chaos-macos.sh: after a detach and a
// reattach with the same device number, health said "ok" although the open
// database and the instance lock belonged to the detached mount). A
// session that cannot be checked because the database is gone is 503
// storage_unavailable, not 500.
func TestStorageLostStaysUnavailable(t *testing.T) {
	e := newACLEnv(t)
	marker := filepath.Join(e.Root, storage.MarkerName)
	saved, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if m := wantJSON(t, "health", e.as(t, "owner", "GET", "/api/v1/health", nil, nil), 200); m["storage"] != "unavailable" {
		t.Fatalf("health %v", m)
	}
	// The disk is back with the same marker.
	if err := os.WriteFile(marker, saved, 0600); err != nil {
		t.Fatal(err)
	}
	if m := wantJSON(t, "health", e.as(t, "owner", "GET", "/api/v1/health", nil, nil), 200); m["storage"] != "unavailable" {
		t.Fatalf("health after the marker is back: %v, want unavailable until restart", m)
	}
	wantError(t, "upload", e.as(t, "owner", "POST", "/api/v1/folders/"+e.family+"/assets", []byte("after reattach"), nil), 503, "storage_unavailable")
	wantError(t, "create folder", e.as(t, "owner", "POST", "/api/v1/folders", []byte(`{"name":"x"}`), nil), 503, "storage_unavailable")
	// The database handle of the detached disk fails: still a storage
	// refusal, not an internal error.
	if err := e.Meta.DB().Close(); err != nil {
		t.Fatal(err)
	}
	wantError(t, "list folders", e.as(t, "owner", "GET", "/api/v1/folders", nil, nil), 503, "storage_unavailable")
	// A fresh open of the volume (what a restart does) works again.
	if _, err := storage.Open(e.Root, storage.OpenOptions{}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}
