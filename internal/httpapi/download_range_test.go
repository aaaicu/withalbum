package httpapi_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestOriginalRange pins the byte range behaviour of the original download
// that docs/API_v2.yaml documents (RFC 9110 ranges, served by
// http.ServeFile): 206 with Content-Range for one satisfiable range, 416
// with "Content-Range: bytes */<size>" otherwise, and the same access
// checks and safety headers as a full download.
func TestOriginalRange(t *testing.T) {
	e := newACLEnv(t)
	orig := "/api/v1/folders/" + e.family + "/assets/" + e.photo + "/original"
	size := len(e.pic)
	if size < 32 {
		t.Fatalf("test picture too small: %d bytes", size)
	}
	safety := func(t *testing.T, what string, r reply) {
		t.Helper()
		if !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("%s: missing attachment/sandbox headers: %v", what, r.Header)
		}
	}

	full := e.as(t, "viewer", "GET", orig, nil, nil)
	if full.Status != 200 || !bytes.Equal(full.Body, e.pic) || full.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("full download: %d accept-ranges=%q", full.Status, full.Header.Get("Accept-Ranges"))
	}
	if full.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("full download Cache-Control %q", full.Header.Get("Cache-Control"))
	}
	safety(t, "full", full)

	for _, c := range []struct {
		rng        string
		start, end int // inclusive
	}{
		{"bytes=0-9", 0, 9},
		{"bytes=10-", 10, size - 1},
		{"bytes=-5", size - 5, size - 1},
		{fmt.Sprintf("bytes=%d-%d", size-3, size+100), size - 3, size - 1}, // end past the size is clamped
	} {
		r := e.as(t, "viewer", "GET", orig, nil, map[string]string{"Range": c.rng})
		want := fmt.Sprintf("bytes %d-%d/%d", c.start, c.end, size)
		if r.Status != 206 || r.Header.Get("Content-Range") != want || !bytes.Equal(r.Body, e.pic[c.start:c.end+1]) {
			t.Fatalf("%s: %d Content-Range %q (want %q), %d bytes", c.rng, r.Status, r.Header.Get("Content-Range"), want, len(r.Body))
		}
		if r.Header.Get("Content-Length") != fmt.Sprint(c.end-c.start+1) || r.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("%s: Content-Length %q Content-Type %q", c.rng, r.Header.Get("Content-Length"), r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: Cache-Control %q", c.rng, r.Header.Get("Cache-Control"))
		}
		safety(t, c.rng, r)
	}

	// Unsatisfiable: starts at or past the end.
	for _, rng := range []string{fmt.Sprintf("bytes=%d-", size), fmt.Sprintf("bytes=%d-%d", size+10, size+20)} {
		r := e.as(t, "viewer", "GET", orig, nil, map[string]string{"Range": rng})
		if r.Status != 416 || r.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
			t.Fatalf("%s: %d Content-Range %q", rng, r.Status, r.Header.Get("Content-Range"))
		}
		if bytes.Contains(r.Body, e.pic[:8]) {
			t.Fatalf("%s: 416 carries file bytes", rng)
		}
	}

	// A malformed Range header, or a unit other than bytes, is 416 too
	// (not ignored).
	for _, rng := range []string{"bytes=abc", "items=0-1"} {
		if r := e.as(t, "viewer", "GET", orig, nil, map[string]string{"Range": rng}); r.Status != 416 {
			t.Fatalf("%s: %d", rng, r.Status)
		}
	}
	// Several ranges: one 206 multipart/byteranges response.
	if r := e.as(t, "viewer", "GET", orig, nil, map[string]string{"Range": "bytes=0-1,5-6"}); r.Status != 206 ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/byteranges") {
		t.Fatalf("multiple ranges: %d %q", r.Status, r.Header.Get("Content-Type"))
	}
	// If-Range with a validator that does not match: the full original.
	if r := e.as(t, "viewer", "GET", orig, nil, map[string]string{"Range": "bytes=0-9", "If-Range": `"other-etag"`}); r.Status != 200 || !bytes.Equal(r.Body, e.pic) {
		t.Fatalf("non-matching If-Range: %d", r.Status)
	}

	// Access is decided before any range: same answers as a full download.
	if r := e.as(t, "uploader", "GET", orig, nil, map[string]string{"Range": "bytes=0-9"}); r.Status != 403 {
		t.Fatalf("uploader range: %d", r.Status)
	}
	if r := e.as(t, "outsider", "GET", orig, nil, map[string]string{"Range": "bytes=0-9"}); r.Status != 404 {
		t.Fatalf("outsider range: %d", r.Status)
	}
}
