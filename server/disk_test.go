//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/storage"
)

func readMarkerFile(t *testing.T, root string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, storage.MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeMarkerFile(t *testing.T, root string, m map[string]any) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, storage.MarkerName), b, 0600); err != nil {
		t.Fatal(err)
	}
}

// A marker of an earlier version (no disk_uuid) gets the disk UUID on the
// first server start; the volume id stays.
func TestOpenAppAdoptsDiskUUIDOfLegacyMarker(t *testing.T) {
	root := newRoot(t)
	m := readMarkerFile(t, root)
	delete(m, "disk_uuid")
	writeMarkerFile(t, root, m)
	a := mustOpenApp(t, root, nil)
	cur, rec := a.vol.DiskUUID()
	a.Close()
	after := readMarkerFile(t, root)
	if after["volume_id"] != m["volume_id"] {
		t.Fatalf("volume id changed: %v -> %v", m["volume_id"], after["volume_id"])
	}
	got, _ := after["disk_uuid"].(string)
	if got != cur || rec != cur {
		t.Fatalf("marker disk_uuid %q, volume current %q recorded %q", got, cur, rec)
	}
	// Restarting is unchanged.
	mustOpenApp(t, root, nil).Close()
	if again := readMarkerFile(t, root); again["disk_uuid"] != after["disk_uuid"] {
		t.Fatalf("disk uuid changed on restart: %v", again["disk_uuid"])
	}
}

// A marker copied from another disk (another disk_uuid) is refused with a
// SECURITY hint until -adopt-disk records this disk.
func TestOpenAppRefusesMarkerOfAnotherDisk(t *testing.T) {
	root := newRoot(t)
	m := readMarkerFile(t, root)
	m["disk_uuid"] = "00000000-0000-4000-8000-000000000001"
	writeMarkerFile(t, root, m)
	_, err := openApp(t.Context(), root, appOptions{Auth: fastAuth})
	if !errors.Is(err, storage.ErrDiskMismatch) && !errors.Is(err, storage.ErrDiskUnknown) {
		t.Fatalf("openApp = %v, want a disk identity error", err)
	}
	if !strings.Contains(err.Error(), "SECURITY") || !strings.Contains(err.Error(), "-adopt-disk") {
		t.Fatalf("hint does not explain the refusal: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(root, ".system")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("refused start created .system (%v)", serr)
	}
	var out, errOut bytes.Buffer
	if code := runAdoptDisk(root, "", &out, &errOut); code != 0 {
		t.Fatalf("adopt-disk = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "00000000-0000-4000-8000-000000000001 -> ") {
		t.Fatalf("adopt-disk output %q", out.String())
	}
	if after := readMarkerFile(t, root); after["volume_id"] != m["volume_id"] {
		t.Fatalf("adopt-disk changed the volume id")
	}
	mustOpenApp(t, root, nil).Close()
	// adopt-disk takes the instance lock: refused while a server runs.
	a := mustOpenApp(t, root, nil)
	defer a.Close()
	if code := runAdoptDisk(root, "", &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "LOCKED") {
		t.Fatalf("adopt-disk while running = %d: %s", code, errOut.String())
	}
}
