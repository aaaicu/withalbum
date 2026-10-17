package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/storage"
)

func newTestStore(t *testing.T) (*Store, *storage.Volume) {
	t.Helper()
	root := t.TempDir()
	if _, err := storage.Init(root); err != nil {
		t.Fatal(err)
	}
	vol, err := storage.Open(root, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(vol)
	if err != nil {
		t.Fatal(err)
	}
	return s, vol
}

func incomingCount(t *testing.T, s *Store) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.root, ".system", "incoming"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestNewRefusesUnverifiedVolume(t *testing.T) {
	_, vol := newTestStore(t)
	if err := os.Remove(filepath.Join(vol.Root(), storage.MarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := New(vol); err == nil {
		t.Fatal("New accepted a volume without marker")
	}
}

func TestStage(t *testing.T) {
	s, _ := newTestStore(t)
	body := []byte(strings.Repeat("x", 1000))
	f, err := s.Stage(bytes.NewReader(body), 1000)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if f.SHA256 != hex.EncodeToString(sum[:]) || f.Size != 1000 || f.MimeType != "text/plain; charset=utf-8" {
		t.Fatalf("staged %+v", f)
	}
	if got, err := os.ReadFile(f.Path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("staged content %v", err)
	}
	f.Remove()
	if n := incomingCount(t, s); n != 0 {
		t.Fatalf("incoming %d after Remove", n)
	}

	for _, tc := range []struct {
		body  []byte
		limit int64
		want  error
	}{
		{body, 999, ErrTooLarge},
		{nil, 10, ErrEmpty},
	} {
		if _, err := s.Stage(bytes.NewReader(tc.body), tc.limit); !errors.Is(err, tc.want) {
			t.Fatalf("Stage(%d bytes, limit %d) err = %v, want %v", len(tc.body), tc.limit, err, tc.want)
		}
		if n := incomingCount(t, s); n != 0 {
			t.Fatalf("incoming %d after failed Stage", n)
		}
	}
	if _, err := s.Stage(iotestErrReader{}, 10); !errors.Is(err, ErrCopy) {
		t.Fatalf("read error err = %v, want ErrCopy", err)
	}
	if n := incomingCount(t, s); n != 0 {
		t.Fatalf("incoming %d after read error", n)
	}
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, errors.New("client went away") }

func TestPromoteAndPreview(t *testing.T) {
	s, _ := newTestStore(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 10, 3000))); err != nil {
		t.Fatal(err)
	}
	f, err := s.Stage(bytes.NewReader(buf.Bytes()), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()
	if f.MimeType != "image/png" {
		t.Fatalf("mime %q", f.MimeType)
	}
	if err := s.Promote(f, strings.Repeat("0", 64)); err == nil {
		t.Fatal("Promote accepted a different hash")
	}
	if err := s.Promote(f, f.SHA256); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(s.PoolPath(f.SHA256))
	if err != nil || st.Mode().Perm() != 0400 || st.Size() != int64(buf.Len()) {
		t.Fatalf("pool file %v %v", st, err)
	}
	if n := incomingCount(t, s); n != 0 {
		t.Fatalf("incoming %d after Promote", n)
	}
	// Promoting identical bytes again reuses the verified pool file.
	g, err := s.Stage(bytes.NewReader(buf.Bytes()), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(s.PoolPath(f.SHA256))
	if err := s.Promote(g, g.SHA256); err != nil {
		t.Fatalf("second Promote: %v", err)
	}
	after, _ := os.Stat(s.PoolPath(f.SHA256))
	if !os.SameFile(before, after) {
		t.Fatal("second Promote replaced the existing pool file")
	}
	if _, err := os.Stat(g.Path); err != nil {
		t.Fatalf("reused Promote consumed the staged file: %v", err)
	}
	g.Remove()
	if err := s.MakePreview(f.SHA256); err != nil {
		t.Fatal(err)
	}
	pf, err := os.Open(s.PreviewPath(f.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	cfg, format, err := image.DecodeConfig(pf)
	if err != nil || format != "jpeg" || cfg.Width != 5 || cfg.Height != 1600 {
		t.Fatalf("preview %s %+v %v", format, cfg, err)
	}
}

func TestMakePreviewRejectsNonImage(t *testing.T) {
	s, _ := newTestStore(t)
	f, err := s.Stage(strings.NewReader("not an image"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(f, f.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.MakePreview(f.SHA256); err == nil {
		t.Fatal("MakePreview accepted text")
	}
	if _, err := os.Stat(s.PreviewPath(f.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview written for text: %v", err)
	}
}

// TestPromoteQuarantinesDamagedPoolFile: a pool file whose bytes do not hash
// to its name is moved to orphans (not deleted) and replaced.
func TestPromoteQuarantinesDamagedPoolFile(t *testing.T) {
	s, _ := newTestStore(t)
	body := []byte("the real original")
	f, err := s.Stage(bytes.NewReader(body), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Remove()
	dest := s.PoolPath(f.SHA256)
	if err := os.Mkdir(filepath.Dir(dest), 0700); err != nil {
		t.Fatal(err)
	}
	damaged := []byte("the real originaX") // same size, other bytes
	if err := os.WriteFile(dest, damaged, 0400); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(f, f.SHA256); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pool file %q %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(OrphansRelDir)))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), f.SHA256+".mismatch.") {
		t.Fatalf("orphans %v %v", entries, err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(OrphansRelDir), entries[0].Name())); !bytes.Equal(got, damaged) {
		t.Fatal("quarantined file changed")
	}
}

func TestErrWriterKeepsFirstError(t *testing.T) {
	first := errors.New("disk full")
	w := &errWriter{w: failingWriter{first}}
	if _, err := w.Write([]byte("x")); err != first || w.err != first {
		t.Fatalf("err %v, kept %v", err, w.err)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }
