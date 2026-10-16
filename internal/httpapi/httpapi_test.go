package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/storage"
)

func openJSONStore(t *testing.T, vol *storage.Volume) meta.MetadataStore {
	t.Helper()
	s, err := jsonstore.Open(vol)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContractJSONStore(t *testing.T) {
	runContract(t, openJSONStore)
}

// TestJSONStoreFailClosedKeepsStateFile checks the jsonstore-specific part of
// the volume guard: refused writes leave state.json byte-for-byte unchanged.
func TestJSONStoreFailClosedKeepsStateFile(t *testing.T) {
	env := newContractEnv(t, openJSONStore)
	h := env.Handler
	fid := createFolder(t, h, "family")["id"].(string)
	statePath := jsonstore.StatePath(env.Root)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(env.Root, storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	wantError(t, "upload", uploadTo(t, h, fid, []byte("sample bytes"), map[string]string{"X-Filename": "sample.txt"}), 503, "storage_unavailable")
	wantError(t, "create folder", do(t, h, "POST", "/api/v1/folders", []byte(`{"name":"other"}`), nil), 503, "storage_unavailable")
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("state.json changed after refused writes")
	}
	folders, err := env.Meta.ListFolders(context.Background())
	if err != nil || len(folders) != 1 {
		t.Fatalf("in-memory folders = %d %v, want 1", len(folders), err)
	}
}

func TestHealthDoesNotLeakStorageDetails(t *testing.T) {
	env := newContractEnv(t, openJSONStore)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		env.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("health status %d", w.Code)
		}
		if strings.Contains(w.Body.String(), env.Root) || strings.Contains(w.Body.String(), env.Volume.ID()) {
			t.Fatalf("health leaks storage details: %s", w.Body)
		}
		var out map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		want := []string{"ok", "unavailable"}[i]
		if out["storage"] != want || out["version"] != "test" {
			t.Fatalf("health %v, want storage %q", out, want)
		}
		if i == 0 {
			if err := os.Remove(filepath.Join(env.Root, storage.MarkerName)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestUploadAndDeltaSyncOverRealHTTP runs one upload/sync/preview round trip
// through a real listener rather than a ResponseRecorder.
func TestUploadAndDeltaSyncOverRealHTTP(t *testing.T) {
	env := newContractEnv(t, openJSONStore)
	server := httptest.NewServer(env.Handler)
	defer server.Close()
	res, err := http.Post(server.URL+"/api/v1/folders", "application/json", strings.NewReader(`{"name":"family"}`))
	if err != nil {
		t.Fatal(err)
	}
	var folder meta.Folder
	if err := json.NewDecoder(res.Body).Decode(&folder); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	pic := jpegBytes(t, 4, 4)
	for i, want := range []int{201, 200} {
		req, _ := http.NewRequest("POST", server.URL+"/api/v1/folders/"+folder.ID+"/assets", bytes.NewReader(pic))
		req.Header.Set("X-Filename", "pic.jpg")
		req.Header.Set("X-Device-ID", "iphone-A")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("upload %d: status %d, want %d", i, res.StatusCode, want)
		}
	}
	res, err = http.Get(server.URL + "/api/v1/folders/" + folder.ID + "/sync?cursor=0")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Events     []meta.Event `json:"events"`
		NextCursor int          `json:"next_cursor"`
	}
	if err := json.NewDecoder(res.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(data.Events) != 1 || data.NextCursor != 1 {
		t.Fatalf("unexpected delta %+v", data)
	}
	prev, err := http.Get(server.URL + "/api/v1/assets/" + shaHex(pic) + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	prev.Body.Close()
	if prev.StatusCode != 200 {
		t.Fatal("preview missing", prev.Status)
	}
}
