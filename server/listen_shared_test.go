package main

import (
	"encoding/json"
	"os"
	"testing"
)

// TestListenScopeSharedCases keeps the macOS menu bar app's address rules
// (macos/Sources/WithAlbumCore/ListenAddress.swift, tested against the same
// file) identical to scopeOf.
func TestListenScopeSharedCases(t *testing.T) {
	b, err := os.ReadFile("../macos/Tests/Fixtures/listen_scope_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct{ Addr, Scope string }
	}
	if err := json.Unmarshal(b, &f); err != nil || len(f.Cases) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	names := map[listenScope]string{scopeLoopback: "loopback", scopePrivate: "private", scopeWildcard: "wildcard"}
	for _, c := range f.Cases {
		got := "refused"
		if s, err := scopeOf(c.Addr); err == nil {
			got = names[s]
		}
		if got != c.Scope {
			t.Errorf("scopeOf(%q) = %s, fixture says %s", c.Addr, got, c.Scope)
		}
	}
}
