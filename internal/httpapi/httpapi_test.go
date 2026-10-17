package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/legacyimport"
	"github.com/aaaicu/withalbum/internal/meta"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

func openSQLiteStore(t *testing.T, vol *storage.Volume) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContractSQLiteStore(t *testing.T) {
	runContract(t, openSQLiteStore)
}

// openImportedSQLiteStore opens a database produced by migrate-legacy from an
// empty PoC state.json, so the contract also covers imported databases.
func openImportedSQLiteStore(t *testing.T, vol *storage.Volume) *sqlitestore.Store {
	t.Helper()
	state := []byte(`{"folders":{},"assets":{},"links":{},"events":{},"next_seq":{}}`)
	if err := os.WriteFile(jsonstore.StatePath(vol.Root()), state, 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := legacyimport.Run(t.Context(), vol, legacyimport.Options{Apply: true}); err != nil {
		t.Fatalf("import: %v (%v)", err, r.Problems)
	}
	return openSQLiteStore(t, vol)
}

func TestContractImportedSQLiteStore(t *testing.T) {
	runContract(t, openImportedSQLiteStore)
}

func TestHealthDoesNotLeakStorageDetails(t *testing.T) {
	env := newContractEnv(t, openSQLiteStore)
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
	env := newContractEnv(t, openSQLiteStore)
	server := httptest.NewServer(env.Handler)
	defer server.Close()
	authed := func(method, path string, body io.Reader) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, body)
		req.Header.Set("Authorization", "Bearer "+env.Token)
		req.Header.Set("X-Filename", "pic.jpg")
		req.Header.Set("X-Device-ID", "iphone-A")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := authed("POST", "/api/v1/folders", strings.NewReader(`{"name":"family"}`))
	var folder struct{ ID string }
	if err := json.NewDecoder(res.Body).Decode(&folder); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	pic := jpegBytes(t, 4, 4)
	var assetID string
	for i, want := range []int{201, 200} {
		res := authed("POST", "/api/v1/folders/"+folder.ID+"/assets", bytes.NewReader(pic))
		var up struct {
			AssetID string `json:"asset_id"`
		}
		_ = json.NewDecoder(res.Body).Decode(&up)
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("upload %d: status %d, want %d", i, res.StatusCode, want)
		}
		assetID = up.AssetID
	}
	res = authed("GET", "/api/v1/folders/"+folder.ID+"/sync?cursor=0", nil)
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
	prev := authed("GET", "/api/v1/folders/"+folder.ID+"/assets/"+assetID+"/preview", nil)
	prev.Body.Close()
	if prev.StatusCode != 200 {
		t.Fatal("preview missing", prev.Status)
	}
}
