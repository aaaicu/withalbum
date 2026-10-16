package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestLocalDashboard(t *testing.T) {
	s := newTestStore(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.mux().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "WithAlbum Server") {
		t.Fatal("dashboard title missing")
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP")
	}
	req = httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	w = httptest.NewRecorder()
	s.mux().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown path returned %d, want 404", w.Code)
	}
}
