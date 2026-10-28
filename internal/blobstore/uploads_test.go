package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

const testUploadID = "0123456789abcdef0123456789abcdef"

func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func partBytes(t *testing.T, s *Store, id string) []byte {
	t.Helper()
	p, err := s.partPath(id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// brokenReader returns its bytes and then an error, like a request body
// whose connection broke.
type brokenReader struct{ r io.Reader }

func (b *brokenReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func TestAppendPartAppendsTruncatesAndRefuses(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.AppendPart("../x", 0, strings.NewReader("a"), 1); err == nil {
		t.Fatal("invalid upload id accepted")
	}
	// No file yet and offset > 0: the recorded bytes are gone.
	if _, err := s.AppendPart(testUploadID, 5, strings.NewReader("a"), 1); !errors.Is(err, ErrPartShort) {
		t.Fatalf("missing part at offset 5: %v", err)
	}
	res, err := s.AppendPart(testUploadID, 0, strings.NewReader("hello "), 100)
	if err != nil || res.Written != 6 || res.SHA256 != hexSHA([]byte("hello ")) {
		t.Fatalf("first chunk: %+v %v", res, err)
	}
	if p, _ := s.partPath(testUploadID); true {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("part mode: %v %v", fi, err)
		}
	}
	// A crash after a write that was never recorded leaves extra bytes:
	// appending at the recorded offset cuts them off first.
	if _, err := s.AppendPart(testUploadID, 6, strings.NewReader("junk"), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPart(testUploadID, 6, strings.NewReader("world"), 100); err != nil {
		t.Fatal(err)
	}
	if got := partBytes(t, s, testUploadID); string(got) != "hello world" {
		t.Fatalf("part = %q", got)
	}
	// More than the limit: nothing of it stays.
	if _, err := s.AppendPart(testUploadID, 11, strings.NewReader("123456"), 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	if got := partBytes(t, s, testUploadID); string(got) != "hello world" {
		t.Fatalf("after refused chunk part = %q", got)
	}
	// A broken body: what arrived is on disk and reported.
	res, err = s.AppendPart(testUploadID, 11, &brokenReader{strings.NewReader("!!")}, 100)
	if !errors.Is(err, ErrCopy) || res.Written != 2 {
		t.Fatalf("broken body: %+v %v", res, err)
	}
	if got := partBytes(t, s, testUploadID); string(got) != "hello world!!" {
		t.Fatalf("after broken chunk part = %q", got)
	}
	// Shorter than recorded.
	if _, err := s.AppendPart(testUploadID, 100, strings.NewReader("x"), 10); !errors.Is(err, ErrPartShort) {
		t.Fatalf("short part: %v", err)
	}
	if err := s.TruncatePart(testUploadID, 5); err != nil {
		t.Fatal(err)
	}
	if got := partBytes(t, s, testUploadID); string(got) != "hello" {
		t.Fatalf("after truncate part = %q", got)
	}
	parts, other, err := s.ListParts()
	if err != nil || len(parts) != 1 || parts[0] != (PartInfo{UploadID: testUploadID, Size: 5}) || len(other) != 0 {
		t.Fatalf("ListParts = %v %v %v", parts, other, err)
	}
	if err := s.RemovePart(testUploadID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePart(testUploadID); err != nil {
		t.Fatalf("removing a missing part: %v", err)
	}
}

func TestFinishPartVerifiesAndFallsBackToPool(t *testing.T) {
	s, _ := newTestStore(t)
	body := []byte(strings.Repeat("v", 3000))
	sha := hexSHA(body)
	if _, err := s.AppendPart(testUploadID, 0, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishPart(testUploadID, sha, int64(len(body))+1); !errors.Is(err, ErrPartMismatch) {
		t.Fatalf("wrong size: %v", err)
	}
	if _, err := s.FinishPart(testUploadID, hexSHA([]byte("other")), int64(len(body))); !errors.Is(err, ErrPartMismatch) {
		t.Fatalf("wrong hash: %v", err)
	}
	f, err := s.FinishPart(testUploadID, sha, int64(len(body)))
	if err != nil || f.SHA256 != sha || f.Size != int64(len(body)) || f.MimeType != "text/plain; charset=utf-8" {
		t.Fatalf("FinishPart = %+v %v", f, err)
	}
	if err := s.Promote(f, sha); err != nil {
		t.Fatal(err)
	}
	// The partial file is now the pool file. A completion retried after a
	// failed commit finds it there.
	g, err := s.FinishPart(testUploadID, sha, int64(len(body)))
	if err != nil || g.SHA256 != sha || g.MimeType != f.MimeType {
		t.Fatalf("FinishPart from pool = %+v %v", g, err)
	}
	if err := s.Promote(g, sha); err != nil {
		t.Fatalf("Promote of the pool fallback: %v", err)
	}
	if _, err := s.FinishPart(testUploadID, hexSHA([]byte("x")), 1); !errors.Is(err, ErrPartMissing) {
		t.Fatalf("nothing anywhere: %v", err)
	}
}

func TestSniffMIMEISOMediaBrands(t *testing.T) {
	ftyp := func(major string, compatible ...string) []byte {
		b := []byte{0, 0, 0, byte(16 + 4*len(compatible)), 'f', 't', 'y', 'p'}
		b = append(b, major...)
		b = append(b, 0, 0, 0, 0)
		for _, c := range compatible {
			b = append(b, c...)
		}
		return append(b, make([]byte, 32)...)
	}
	for _, tc := range []struct {
		head []byte
		want string
	}{
		{ftyp("heic", "mif1", "heic"), "image/heic"},
		{ftyp("mif1", "heic"), "image/heic"},
		{ftyp("mif1", "mif1"), "image/heif"},
		{ftyp("avif", "mif1"), "image/avif"},
		{ftyp("qt  ", "qt  "), "video/quicktime"},
		{ftyp("isom", "isom", "avc1"), "video/mp4"},
		{ftyp("mp42", "mp41"), "video/mp4"},
		{ftyp("3gp4", "3gp4"), "video/3gpp"},
		{[]byte("\xff\xd8\xff\xe0 jpeg"), "image/jpeg"},
		{[]byte("plain text"), "text/plain; charset=utf-8"},
		{[]byte{0, 0, 0, 3, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c', 0, 0, 0, 0}, "application/octet-stream"},
	} {
		if got := SniffMIME(tc.head); got != tc.want {
			t.Errorf("SniffMIME(%q) = %q, want %q", tc.head[:min(len(tc.head), 24)], got, tc.want)
		}
	}
}
