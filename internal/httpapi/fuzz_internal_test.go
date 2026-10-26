package httpapi

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/aaaicu/withalbum/internal/meta"
)

// Fuzzers of the request parsers that see bytes from any client that can
// reach the server, including the relay and the funnel (before or right
// after authentication). Run one longer with, e.g.:
//
//	go test ./internal/httpapi -run '^$' -fuzz '^FuzzParseCursor$' -fuzztime 60s

// FuzzParseCursor: a sync cursor is accepted only in its canonical form
// "<16 hex>.<decimal seq>" (or empty), and an accepted one encodes back to
// a cursor that parses to the same position.
func FuzzParseCursor(f *testing.F) {
	for _, s := range []string{"", "0123456789abcdef.0", "0123456789abcdef.42", "0123456789abcdef.-1", "0123456789ABCDEF.1",
		"0123456789abcdef.", ".1", "0123456789abcdef.1.2", "0123456789abcdef.999999999999999999", "0123456789abcdef.9999999999999999999",
		"0123456789abcdef.+1", "0123456789abcdef. 1", "0123456789abcdef.0x1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		c, ok := parseCursor(v)
		if !ok {
			return
		}
		if v == "" {
			if c != (syncCursor{}) {
				t.Fatalf("empty cursor parsed to %+v", c)
			}
			return
		}
		if c.seq < 0 || !epochRE.MatchString(c.epoch) {
			t.Fatalf("parseCursor(%q) = %+v", v, c)
		}
		again, ok := parseCursor(encodeCursor(c.epoch, c.seq))
		if !ok || again != c {
			t.Fatalf("round trip of %q: %+v -> %+v (%v)", v, c, again, ok)
		}
	})
}

// FuzzParseSocialCursor: the opaque cursor of comment and activity lists
// is bounded, valid UTF-8 in its ID, and round-trips.
func FuzzParseSocialCursor(f *testing.F) {
	f.Add("")
	f.Add(encodeSocialCursor(meta.SocialCursor{At: "2026-10-26T00:00:00.000000000Z", ID: "c0123"}))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("a\nb\nc")))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte("\n")))
	f.Add(strings.Repeat("A", 500))
	f.Fuzz(func(t *testing.T, v string) {
		c, ok := parseSocialCursor(v)
		if !ok || v == "" {
			return
		}
		if c.At == "" || c.ID == "" || strings.ContainsRune(c.ID, '\n') || !utf8.ValidString(c.ID) || len(c.At)+len(c.ID) > 400 {
			t.Fatalf("parseSocialCursor(%q) = %+v", v, c)
		}
		again, ok := parseSocialCursor(encodeSocialCursor(c))
		if !ok || again != c {
			t.Fatalf("round trip of %q: %+v -> %+v", v, c, again)
		}
	})
}

// FuzzParseComment: an accepted comment is trimmed valid UTF-8 without
// control characters (other than newline and tab) or bidirectional
// overrides, within the byte bound, and its mention list is distinct,
// sorted and exactly the accounts named by its tokens.
func FuzzParseComment(f *testing.F) {
	id := strings.Repeat("ab", 16)
	f.Add("hello", "")
	f.Add("hi <@"+id+">", id)
	f.Add("hi <@"+id+"> <@"+id+">", id+","+id)
	f.Add("x\u202Ey", "")
	f.Add("\r\n a \r", "")
	f.Add("\x00", "")
	f.Add("<@"+strings.ToUpper(id)+">", strings.ToUpper(id))
	f.Fuzz(func(t *testing.T, body, mentions string) {
		in := commentInput{Body: &body}
		if mentions != "" {
			in.Mentions = strings.Split(mentions, ",")
		}
		got, ids, code := parseComment(in)
		if code != "" {
			if got != "" || ids != nil {
				t.Fatalf("refused (%s) but returned %q %v", code, got, ids)
			}
			return
		}
		if got == "" || got != strings.TrimSpace(got) || !utf8.ValidString(got) || len(got) > maxSocialBody {
			t.Fatalf("accepted body %q", got)
		}
		for _, r := range got {
			if r == '\r' || unicode.IsControl(r) && r != '\n' && r != '\t' || bidiControl(r) {
				t.Fatalf("accepted body %q with %U", got, r)
			}
		}
		tokens := map[string]bool{}
		for _, m := range mentionTokenRE.FindAllStringSubmatch(got, -1) {
			tokens[m[1]] = true
		}
		if len(ids) != len(tokens) {
			t.Fatalf("mentions %v, tokens %v", ids, tokens)
		}
		for i, id := range ids {
			if !tokens[id] || i > 0 && ids[i-1] >= id {
				t.Fatalf("mentions %v not the sorted distinct tokens %v", ids, tokens)
			}
		}
	})
}

// FuzzStoredFilename: whatever a client sends as X-Filename (or the
// filename of an upload session), the stored name is one path element: no
// separator, not "." or "..", at most 240 bytes. It is only ever metadata
// (originals are stored by SHA-256), but it must never look like a path.
func FuzzStoredFilename(f *testing.F) {
	sha := strings.Repeat("0", 64)
	for _, s := range []string{"", "a.jpg", "../../etc/passwd", "..\\..\\x", "%2e%2e%2fx", "a/b/c.heic", "/", ".", "..", "%", "%zz",
		"C:\\Users\\x\\IMG.JPG", strings.Repeat("a", 300), "a\x00b", "IMG%20001.jpg", "%2F", "..%2F.."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got := uploadFilename(name, sha)
		if got == "" || got == "." || got == ".." || strings.ContainsAny(got, `/\`) || len(got) > 240 && got != sha {
			t.Fatalf("uploadFilename(%q) = %q", name, got)
		}
		if filepath.Base(got) != got {
			t.Fatalf("uploadFilename(%q) = %q is not one path element", name, got)
		}
	})
}

// FuzzDecodeJSON: the shared JSON body decoder never reads more than its
// bound, never panics, and refuses anything but application/json.
func FuzzDecodeJSON(f *testing.F) {
	f.Add("application/json", []byte(`{"login_name":"a","password":"b"}`))
	f.Add("application/json; charset=utf-8", []byte(`{"bytes":1,"sha256":"x","filename":"y"}`))
	f.Add("text/plain", []byte(`{}`))
	f.Add("application/json", bytes.Repeat([]byte(`[`), 20000))
	f.Add("application/json", []byte(`{"hide_location":true}{"x":1}`))
	f.Add("application/json;;", []byte(`{}`))
	f.Fuzz(func(t *testing.T, ct string, body []byte) {
		r := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(body))
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		var v struct {
			LoginName    string  `json:"login_name"`
			Password     string  `json:"password"`
			Bytes        int64   `json:"bytes"`
			SHA256       string  `json:"sha256"`
			HideLocation *bool   `json:"hide_location"`
			Mentions     []any   `json:"mentions"`
			Number       float64 `json:"n"`
		}
		ok := decodeJSON(w, r, &v)
		if ok && w.Code != http.StatusOK {
			t.Fatalf("accepted with status %d", w.Code)
		}
		if !ok && w.Code != 400 && w.Code != 415 {
			t.Fatalf("refused with status %d", w.Code)
		}
		if ok && len(v.LoginName)+len(v.Password)+len(v.SHA256) > maxAuthBody {
			t.Fatalf("decoded more than %d bytes", maxAuthBody)
		}
	})
}

// FuzzUploadOffset: the Upload-Offset header parser used by PATCH
// .../uploads/{id} accepts only canonical non-negative decimal integers.
func FuzzUploadOffset(f *testing.F) {
	for _, s := range []string{"0", "1", "-1", "+1", " 1", "1 ", "0x10", "9223372036854775807", "9223372036854775808", "", "00", "1e3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		off, ok := parseUploadOffset(v)
		if !ok {
			return
		}
		if off < 0 || strings.TrimLeft(v, "0123456789") != "" {
			t.Fatalf("parseUploadOffset(%q) = %d", v, off)
		}
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n != off {
			t.Fatalf("parseUploadOffset(%q) = %d, ParseInt %d %v", v, off, n, err)
		}
	})
}

func TestPlainLabel(t *testing.T) {
	for s, want := range map[string]bool{
		"": true, "가족 앨범": true, "Mom's iPhone": true, "عائلة\u200f": true, "👨\u200d👩\u200d👧": true,
		"a\u202eb": false, "a\u2066b": false, "a\tb": false, "a\x7fb": false, "\xff": false,
	} {
		if got := plainLabel(s); got != want {
			t.Errorf("plainLabel(%q) = %v, want %v", s, got, want)
		}
	}
}
