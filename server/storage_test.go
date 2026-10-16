package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/storage"
)

// newTestStore returns a store on a freshly initialized t.TempDir() volume.
func newTestStore(t *testing.T) *store {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := newStore(vol)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUploadFailsClosedWhenVolumeGone(t *testing.T) {
	s := newTestStore(t)
	h := s.mux()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/folders", strings.NewReader(`{"name":"family"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create folder: %d %s", w.Code, w.Body)
	}
	var folder Folder
	if err := json.Unmarshal(w.Body.Bytes(), &folder); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(s.root, ".system", "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(s.root, storage.MarkerName)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/folders/"+folder.ID+"/assets", bytes.NewReader([]byte("sample bytes")))
	req.Header.Set("X-Filename", "sample.txt")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"storage_unavailable"`) {
		t.Fatalf("upload: %d %s, want 503 storage_unavailable", w.Code, w.Body)
	}
	if n := countFiles(t, filepath.Join(s.root, ".system", "pool")); n != 0 {
		t.Fatalf("pool has %d files after refused upload", n)
	}
	if n := countFiles(t, filepath.Join(s.root, ".system", "incoming")); n != 0 {
		t.Fatalf("incoming has %d files after refused upload", n)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("state.json changed after refused upload")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/folders", strings.NewReader(`{"name":"other"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("create folder with volume gone: %d %s", w.Code, w.Body)
	}
	if after, _ := os.ReadFile(statePath); !bytes.Equal(before, after) {
		t.Fatal("state.json changed after refused folder creation")
	}
	s.mu.Lock()
	folders := len(s.state.Folders)
	s.mu.Unlock()
	if folders != 1 {
		t.Fatalf("in-memory folders = %d, want 1", folders)
	}
}

func TestHealthReportsStorage(t *testing.T) {
	s := newTestStore(t)
	health := func() map[string]string {
		t.Helper()
		w := httptest.NewRecorder()
		s.mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("health status %d", w.Code)
		}
		if strings.Contains(w.Body.String(), s.root) || strings.Contains(w.Body.String(), s.vol.ID()) {
			t.Fatalf("health leaks storage details: %s", w.Body)
		}
		var out map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := health()["storage"]; got != "ok" {
		t.Fatalf("storage = %q, want ok", got)
	}
	if err := os.Remove(filepath.Join(s.root, storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if got := health()["storage"]; got != "unavailable" {
		t.Fatalf("storage = %q, want unavailable", got)
	}
}
