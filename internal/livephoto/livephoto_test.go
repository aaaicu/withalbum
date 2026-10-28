package livephoto_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aaaicu/withalbum/internal/blobstore"
	"github.com/aaaicu/withalbum/internal/livephoto"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

const uuid = "2F1C6E5A-0B4D-4C3E-9A8B-7D6E5F4A3B2C"

func still(t *testing.T, b []byte) string {
	t.Helper()
	id, err := livephoto.StillIdentifier(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func video(t *testing.T, b []byte) string {
	t.Helper()
	id, err := livephoto.VideoIdentifier(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestIdentifiers(t *testing.T) {
	for name, tc := range map[string]struct {
		b    []byte
		fn   func(*testing.T, []byte) string
		want string
		mime string
	}{
		"jpeg":                 {lpt.JPEG(uuid, 1), still, uuid, "image/jpeg"},
		"heic":                 {lpt.HEIC(uuid, 1), still, uuid, "image/heic"},
		"mov":                  {lpt.MOV(uuid, 1), video, uuid, "video/quicktime"},
		"jpeg without id":      {lpt.JPEG("", 1), still, "", "image/jpeg"},
		"heic without id":      {lpt.HEIC("", 1), still, "", "image/heic"},
		"mov without id":       {lpt.MOV("", 1), video, "", "video/quicktime"},
		"jpeg without makernt": {lpt.JPEGWithTIFF(lpt.TIFF(nil), 1), still, "", "image/jpeg"},
		"other makernote":      {lpt.JPEGWithTIFF(lpt.TIFF(append([]byte("Nikon\x00\x02\x10\x00\x00"), make([]byte, 40)...)), 1), still, "", "image/jpeg"},
		"mov as still":         {lpt.MOV(uuid, 1), still, "", "video/quicktime"},
		"jpeg as video":        {lpt.JPEG(uuid, 1), video, "", "image/jpeg"},
		"id with a space":      {lpt.MOV("bad id", 1), video, "", "video/quicktime"},
		"empty":                {nil, still, "", "text/plain; charset=utf-8"},
	} {
		if got := tc.fn(t, tc.b); got != tc.want {
			t.Errorf("%s: identifier %q, want %q", name, got, tc.want)
		}
		if got := blobstore.SniffMIME(tc.b); got != tc.mime {
			t.Errorf("%s: sniffed %q, want %q", name, got, tc.mime)
		}
	}
}

// TestLittleEndianMakerNote: the MakerNote's own byte order counts, not
// the TIFF block's.
func TestLittleEndianMakerNote(t *testing.T) {
	mn := lpt.MakerNote(uuid)
	le := append([]byte{}, mn[:12]...)
	le = append(le, "II"...)
	dir := mn[14:]
	put := func(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
	put32 := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	le = append(le, put(1)...)
	le = append(le, put(0x0011)...)
	le = append(le, put(2)...)
	le = append(le, put32(binary.BigEndian.Uint32(dir[6:10]))...)
	le = append(le, put32(binary.BigEndian.Uint32(dir[10:14]))...)
	le = append(le, put32(0)...)
	le = append(le, dir[18:]...)
	if got := still(t, lpt.JPEGWithTIFF(lpt.TIFF(le), 2)); got != uuid {
		t.Fatalf("little-endian MakerNote: %q", got)
	}
}

func TestPair(t *testing.T) {
	for _, tc := range []struct {
		s, v, want string
		err        error
	}{
		{uuid, uuid, livephoto.Verified, nil},
		{uuid, "", livephoto.Unverified, nil},
		{"", uuid, livephoto.Unverified, nil},
		{"", "", livephoto.Unverified, nil},
		{uuid, "OTHER", "", livephoto.ErrMismatch},
	} {
		got, err := livephoto.Pair(tc.s, tc.v)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("Pair(%q, %q) = %q, %v", tc.s, tc.v, got, err)
		}
	}
	for _, m := range []string{"image/heic", "image/heif", "image/jpeg"} {
		if !livephoto.StillType(m) || livephoto.VideoType(m) {
			t.Errorf("%s", m)
		}
	}
	for _, m := range []string{"video/quicktime", "video/mp4"} {
		if livephoto.StillType(m) || !livephoto.VideoType(m) {
			t.Errorf("%s", m)
		}
	}
	for _, m := range []string{"image/png", "application/octet-stream", "text/html; charset=utf-8", ""} {
		if livephoto.StillType(m) || livephoto.VideoType(m) {
			t.Errorf("%s accepted", m)
		}
	}
}

// TestTruncatedAndCorrupt: every prefix and many single-byte corruptions
// of the fixtures give an identifier or "", never a panic or an error
// (the reader itself never fails here).
func TestTruncatedAndCorrupt(t *testing.T) {
	for _, b := range [][]byte{lpt.JPEG(uuid, 3), lpt.HEIC(uuid, 3), lpt.MOV(uuid, 3)} {
		for n := 0; n < len(b); n++ {
			still(t, b[:n])
			video(t, b[:n])
		}
		for i := 0; i < len(b); i++ {
			c := append([]byte{}, b...)
			c[i] ^= 0xFF
			if id := still(t, c); id != "" && id != uuid {
				// A flipped byte inside the identifier is still printable
				// sometimes; anything else must not appear.
				if len(id) != len(uuid) {
					t.Fatalf("corrupt byte %d: identifier %q", i, id)
				}
			}
			video(t, c)
		}
	}
}

// TestOffsetsOutOfRange: offsets pointing outside the block are refused.
func TestOffsetsOutOfRange(t *testing.T) {
	mn := lpt.MakerNote(uuid)
	bad := append([]byte{}, mn...)
	binary.BigEndian.PutUint32(bad[14+2+8:], 0xFFFFFFF0) // value offset
	if got := still(t, lpt.JPEGWithTIFF(lpt.TIFF(bad), 4)); got != "" {
		t.Fatalf("offset beyond the MakerNote: %q", got)
	}
	bad = append([]byte{}, mn...)
	binary.BigEndian.PutUint32(bad[14+2+4:], 0xFFFFFFFF) // count
	if got := still(t, lpt.JPEGWithTIFF(lpt.TIFF(bad), 4)); got != "" {
		t.Fatalf("huge count: %q", got)
	}
	bad = append([]byte{}, mn...)
	binary.BigEndian.PutUint16(bad[14:], 0xFFFF) // entries
	if got := still(t, lpt.JPEGWithTIFF(lpt.TIFF(bad), 4)); got != "" {
		t.Fatalf("huge directory: %q", got)
	}
	// The same tag twice is ambiguous.
	dup := append([]byte{}, mn[:14]...)
	dup = append(dup, 0, 2)
	entry := mn[16:28]
	// Both entries point past the new, longer directory.
	e := append([]byte{}, entry...)
	binary.BigEndian.PutUint32(e[8:], 14+2+24+4)
	dup = append(dup, e...)
	dup = append(dup, e...)
	dup = append(dup, 0, 0, 0, 0)
	dup = append(dup, mn[32:]...)
	if got := still(t, lpt.JPEGWithTIFF(lpt.TIFF(dup), 4)); got != "" {
		t.Fatalf("duplicate tag: %q", got)
	}
}

type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("disk on fire") }

func TestIOError(t *testing.T) {
	if _, err := livephoto.StillIdentifier(failingReader{}, 1000); err == nil {
		t.Fatal("still: no error from a failing reader")
	}
	if _, err := livephoto.VideoIdentifier(failingReader{}, 1000); err == nil {
		t.Fatal("video: no error from a failing reader")
	}
}

// countingReader counts what the parsers read.
type countingReader struct {
	r     io.ReaderAt
	bytes int64
	reads int
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.bytes += int64(len(p))
	c.reads++
	return c.r.ReadAt(p, off)
}

// TestLargeVideoNotRead: a movie whose mdat is huge (a sparse 4 GiB
// claim) is walked by seeking; the identifier is found with little I/O.
func TestLargeVideoNotRead(t *testing.T) {
	mov := lpt.MOV(uuid, 5)
	// Grow mdat to claim 3 GiB of zeros (without allocating them).
	i := bytes.Index(mov, []byte("mdat")) - 4
	moov := mov[bytes.Index(mov, []byte("moov"))-4:]
	head := append([]byte{}, mov[:i]...)
	const big = 3 << 30
	mdat := append(binary.BigEndian.AppendUint32(nil, 1), "mdat"...)
	mdat = binary.BigEndian.AppendUint64(mdat, big)
	r := &sparse{head: append(head, mdat...), tail: moov, size: int64(len(head)) + big + int64(len(moov))}
	c := &countingReader{r: r}
	id, err := livephoto.VideoIdentifier(c, r.size)
	if err != nil || id != uuid {
		t.Fatalf("identifier %q, %v", id, err)
	}
	if c.bytes > 64<<10 {
		t.Fatalf("read %d bytes in %d calls", c.bytes, c.reads)
	}
}

// sparse is head, then zeros, then tail at the end.
type sparse struct {
	head, tail []byte
	size       int64
}

func (s *sparse) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		o := off + int64(i)
		switch {
		case o >= s.size:
			return i, io.EOF
		case o < int64(len(s.head)):
			p[i] = s.head[o]
		case o >= s.size-int64(len(s.tail)):
			p[i] = s.tail[o-(s.size-int64(len(s.tail)))]
		default:
			p[i] = 0
		}
	}
	return len(p), nil
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	sp, vp := filepath.Join(dir, "IMG_0001.HEIC"), filepath.Join(dir, "IMG_0001.MOV")
	if err := os.WriteFile(sp, lpt.HEIC(uuid, 6), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vp, lpt.MOV(uuid, 6), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := livephoto.StillIdentifierFile(sp)
	if err != nil {
		t.Fatal(err)
	}
	v, err := livephoto.VideoIdentifierFile(vp)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := livephoto.Pair(s, v); p != livephoto.Verified || err != nil {
		t.Fatalf("pair %q %v (%q, %q)", p, err, s, v)
	}
	if _, err := livephoto.StillIdentifierFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file: no error")
	}
}

func seeds(f *testing.F) {
	for _, b := range [][]byte{lpt.JPEG(uuid, 1), lpt.HEIC(uuid, 1), lpt.MOV(uuid, 1), lpt.JPEG("", 1), lpt.MOV("", 2),
		[]byte(strings.Repeat("\xff", 40))} {
		f.Add(b)
	}
}

func FuzzStillIdentifier(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		id, err := livephoto.StillIdentifier(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatalf("error from an in-memory reader: %v", err)
		}
		if len(id) > livephoto.MaxIdentifier || strings.ContainsAny(id, " \x00") {
			t.Fatalf("identifier %q", id)
		}
	})
}

func FuzzVideoIdentifier(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		id, err := livephoto.VideoIdentifier(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatalf("error from an in-memory reader: %v", err)
		}
		if len(id) > livephoto.MaxIdentifier || strings.ContainsAny(id, " \x00") {
			t.Fatalf("identifier %q", id)
		}
	})
}
