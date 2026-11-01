package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aaaicu/withalbum/internal/auth/oidc"
	"github.com/aaaicu/withalbum/internal/auth/oidc/oidctest"
)

func TestOIDCKeysOnce(t *testing.T) {
	ti := oidctest.NewTestIssuer()
	defer ti.Close()
	saved := map[oidc.Provider]string{}
	for p, u := range oidc.KeysURLs {
		saved[p] = u
		oidc.KeysURLs[p] = ti.Server.URL
	}
	defer func() {
		for p, u := range saved {
			oidc.KeysURLs[p] = u
		}
	}()
	dir := t.TempDir()
	if code := runOIDCKeys([]string{"-out", dir, "-once"}, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, p := range oidc.Providers {
		f := filepath.Join(dir, string(p)+".json")
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o444 {
			t.Fatalf("%s: %v %v", f, st, err)
		}
	}
	// A failing provider keeps the old file and the exit code says so.
	oidc.KeysURLs[oidc.Apple] = "http://127.0.0.1:1/unreachable"
	before, _ := os.ReadFile(filepath.Join(dir, "apple.json"))
	if code := runOIDCKeys([]string{"-out", dir, "-once"}, io.Discard); code != exitProblems {
		t.Fatalf("exit %d, want %d", code, exitProblems)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "apple.json"))
	if string(before) != string(after) {
		t.Fatal("old key file must be kept")
	}
	if code := runOIDCKeys([]string{"-once"}, io.Discard); code != exitUsage {
		t.Fatalf("no -out: exit %d", code)
	}
	if code := runOIDCKeys([]string{"-out", dir, "-every", "1m"}, io.Discard); code != exitUsage {
		t.Fatalf("too often: exit %d", code)
	}
}
