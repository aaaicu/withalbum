package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aaaicu/withalbum/internal/db"
	"github.com/aaaicu/withalbum/internal/meta/jsonstore"
	"github.com/aaaicu/withalbum/internal/meta/sqlitestore"
	"github.com/aaaicu/withalbum/internal/storage"
)

func TestLocalOnly(t *testing.T) {
	for _, tt := range []struct {
		addr    string
		allowed bool
	}{
		{"127.0.0.1:8787", true},
		{"localhost:8787", true},
		{"[::1]:8787", true},
		{"0.0.0.0:8787", false},
		{":8787", false},
		{"192.168.0.21:8787", false},
		{"[::]:8787", false},
		{"badaddr", false},
	} {
		if got := loopbackOnly(tt.addr); got != tt.allowed {
			t.Errorf("loopbackOnly(%q) = %v, want %v", tt.addr, got, tt.allowed)
		}
	}
}

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
