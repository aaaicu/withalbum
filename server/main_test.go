package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/legacyimport"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

func newTestVolume(t *testing.T) *storage.Volume {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

// writeLegacyState plants a PoC state.json and returns its bytes.
func writeLegacyState(t *testing.T, root string) []byte {
	t.Helper()
	state := []byte(`{"folders":{"0123456789abcdef0123456789abcdef":{"id":"0123456789abcdef0123456789abcdef","name":"family","created_at":"2026-01-01T00:00:00Z"}},"assets":{},"links":{},"events":{},"next_seq":{}}` + "\n")
	if err := os.Mkdir(filepath.Join(root, ".system"), 0700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonstore.StatePath(root), state, 0600); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestStartupCreatesSQLiteOnNewInstall(t *testing.T) {
	vol := newTestVolume(t)
	store, err := openMetadata(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, ok := store.(*sqlitestore.Store); !ok {
		t.Fatalf("store is %T, want *sqlitestore.Store", store)
	}
	if _, err := os.Stat(sqlitestore.DBPath(vol.Root())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jsonstore.StatePath(vol.Root())); !os.IsNotExist(err) {
		t.Fatalf("state.json created on a new install: %v", err)
	}
}

// TestStartupRefusesUnmigratedState: a root with only state.json is refused,
// state.json keeps its exact bytes and no database is created.
func TestStartupRefusesUnmigratedState(t *testing.T) {
	vol := newTestVolume(t)
	before := writeLegacyState(t, vol.Root())
	for i := 0; i < 2; i++ {
		store, err := openMetadata(t.Context(), vol)
		if !errors.Is(err, errUnmigratedState) {
			if store != nil {
				store.Close()
			}
			t.Fatalf("openMetadata = %v, want errUnmigratedState", err)
		}
	}
	after, err := os.ReadFile(jsonstore.StatePath(vol.Root()))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("state.json changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(vol.Root(), ".system"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("unexpected %s in .system after refused start", e.Name())
		}
	}
}

// TestStartupRefusesStateNextToUnrelatedDB: a database plus a state.json that
// was never imported is refused too, and state.json is left unchanged.
func TestStartupRefusesStateNextToUnrelatedDB(t *testing.T) {
	vol := newTestVolume(t)
	s, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := writeLegacyState(t, vol.Root())
	if store, err := openMetadata(t.Context(), vol); !errors.Is(err, errUnmigratedState) {
		if store != nil {
			store.Close()
		}
		t.Fatalf("openMetadata = %v, want errUnmigratedState", err)
	}
	if after, _ := os.ReadFile(jsonstore.StatePath(vol.Root())); !bytes.Equal(before, after) {
		t.Fatal("state.json changed")
	}
}

// TestStartupAcceptsImportedState: a state.json whose hash is recorded in
// legacy_imports (an import that stopped before renaming it) does not block.
func TestStartupAcceptsImportedState(t *testing.T) {
	vol := newTestVolume(t)
	s, err := sqlitestore.Open(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	state := writeLegacyState(t, vol.Root())
	sqlDB, err := db.Open(sqlitestore.DBPath(vol.Root()))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(state)
	if _, err := sqlDB.Exec("INSERT INTO legacy_imports (import_id, source_sha256, report_json) VALUES ('test', ?, '{}')", hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()
	store, err := openMetadata(t.Context(), vol)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
}

// TestMigrateLegacyCommand drives "migrate-legacy" end to end on a root that
// only the previous version could start: dry-run writes nothing, -apply
// imports, the server then starts on the database, and a second -apply is
// refused.
func TestMigrateLegacyCommand(t *testing.T) {
	vol := newTestVolume(t)
	state := writeLegacyState(t, vol.Root())
	t.Setenv("WITHALBUM_ROOT", vol.Root())
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runMigrateLegacy(args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	if code, _, _ := run("-bogus"); code != 2 {
		t.Fatalf("bad flag: exit %d, want 2", code)
	}
	if code, _, _ := run("extra"); code != 2 {
		t.Fatalf("extra argument: exit %d, want 2", code)
	}
	code, out, errOut := run()
	if code != 0 || !strings.Contains(out, "(dry-run): ok") || !strings.Contains(out, "folders 1") {
		t.Fatalf("dry-run: exit %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(sqlitestore.DBPath(vol.Root())); !os.IsNotExist(err) {
		t.Fatalf("dry-run created the database: %v", err)
	}
	code, out, errOut = run("-apply", "-json")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s\n%s", code, out, errOut)
	}
	var report legacyimport.Report
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.Result != "ok" || report.Folders != 1 {
		t.Fatalf("apply -json report %q: %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(vol.Root(), report.MigratedPath)); err != nil || !bytes.Equal(b, state) {
		t.Fatalf("migrated state.json: %v", err)
	}
	store, err := openMetadata(t.Context(), vol)
	if err != nil {
		t.Fatalf("server start after import: %v", err)
	}
	folders, err := store.ListFolders(t.Context())
	store.Close()
	if err != nil || len(folders) != 1 || folders[0].Name != "family" || folders[0].ID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("folders after import: %+v %v", folders, err)
	}
	if code, _, errOut := run("-apply"); code != 1 || !strings.Contains(errOut, "no .system/state.json") {
		t.Fatalf("second apply: exit %d, %s", code, errOut)
	}
}

func TestMigrateLegacyCommandRefusesBrokenState(t *testing.T) {
	vol := newTestVolume(t)
	if err := os.Mkdir(filepath.Join(vol.Root(), ".system"), 0700); err != nil {
		t.Fatal(err)
	}
	broken := []byte(`{"folders":{"a":{"id":"a","name":"x","created_at":"t"}},"assets":`)
	if err := os.WriteFile(jsonstore.StatePath(vol.Root()), broken, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITHALBUM_ROOT", vol.Root())
	t.Setenv("WITHALBUM_VOLUME_ID", "")
	var stdout, stderr bytes.Buffer
	if code := runMigrateLegacy([]string{"-apply"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "FAILED") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if b, _ := os.ReadFile(jsonstore.StatePath(vol.Root())); !bytes.Equal(b, broken) {
		t.Fatal("state.json changed")
	}
	entries, _ := os.ReadDir(filepath.Join(vol.Root(), ".system"))
	for _, e := range entries {
		if e.Name() != "state.json" && e.Name() != storage.LockName {
			t.Fatalf(".system holds %s, want only state.json and the instance lock", e.Name())
		}
	}
}
