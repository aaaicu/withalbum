// Package livephoto checks that the two originals of a Live Photo belong
// together: the still (HEIC or JPEG) and its paired video (QuickTime)
// carry the same Apple content identifier, in the still's EXIF MakerNote
// (Apple tag 0x0011) and in the movie's QuickTime metadata
// (com.apple.quicktime.content.identifier).
//
// Originals are untrusted input. The containers (JPEG segments, HEIF
// items, ISO/QuickTime boxes) are read through internal/capturetime with
// its limits (at most 1 MiB read through a bounded number of ReadAt
// calls, counted boxes and segments, metadata blocks of at most 256 KiB);
// this package only walks the EXIF and Apple MakerNote directories inside
// the block it gets back, with fixed entry limits and checked offsets. It
// never panics, never allocates by a size read from the file, and only
// reads. A missing or unreadable identifier is "" (unknown), never a
// guess.
package livephoto

import (
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/aaaicu/withalbum/internal/capturetime"
)

// Pairing states of a Live Photo (assets.pairing_state).
const (
	// Verified: both originals carry the same content identifier.
	Verified = "verified"
	// Unverified: at least one of them carries none that could be read;
	// the client said they belong together and nothing contradicts it.
	Unverified = "unverified"
)

// ErrMismatch: both originals carry a content identifier and they differ,
// so they are not the two halves of one Live Photo.
var ErrMismatch = errors.New("livephoto: the still and the video carry different content identifiers")

// Pair decides the pairing state from the two identifiers ("" = unknown).
func Pair(stillID, videoID string) (string, error) {
	switch {
	case stillID == "" || videoID == "":
		return Unverified, nil
	case stillID == videoID:
		return Verified, nil
	}
	return "", ErrMismatch
}

// StillType reports whether a sniffed MIME type can be the still of a
// Live Photo.
func StillType(mime string) bool {
	switch mime {
	case "image/heic", "image/heif", "image/jpeg":
		return true
	}
	return false
}

// VideoType reports whether a sniffed MIME type can be the paired video of
// a Live Photo.
func VideoType(mime string) bool {
	switch mime {
	case "video/quicktime", "video/mp4":
		return true
	}
	return false
}

// VideoKey is the QuickTime metadata key of the content identifier.
const VideoKey = "com.apple.quicktime.content.identifier"

// MaxIdentifier bounds the length of a content identifier (Apple writes a
// 36-character UUID).
const MaxIdentifier = 64

const (
	tagExifIFD           = 0x8769
	tagMakerNote         = 0x927C
	tagContentIdentifier = 0x0011 // Apple MakerNote

	typeASCII     = 2
	typeLong      = 4
	typeUndefined = 7
	typeIFD       = 13

	// maxEntries bounds one directory (EXIF or MakerNote).
	maxEntries = 1024
)

// appleHeader starts an Apple MakerNote: "Apple iOS\0", a 2-byte version
// and the byte order ("MM" or "II") of the directory that follows at
// offset 14. Offsets inside the MakerNote count from its first byte.
const appleHeader = "Apple iOS\x00"

// StillIdentifier reads the content identifier of a Live Photo still
// (JPEG or HEIF/HEIC) of size bytes. It returns "" when there is none it
// can read; the error is non-nil only when r itself failed.
func StillIdentifier(r io.ReaderAt, size int64) (string, error) {
	tiff, err := capturetime.ExifTIFF(r, size)
	if err != nil || tiff == nil {
		return "", err
	}
	return makerNoteIdentifier(tiff), nil
}

// VideoIdentifier reads the content identifier of a Live Photo video
// (QuickTime) of size bytes, from moov/meta. It returns "" when there is
// none it can read; the error is non-nil only when r itself failed.
func VideoIdentifier(r io.ReaderAt, size int64) (string, error) {
	v, err := capturetime.QuickTimeString(r, size, VideoKey, MaxIdentifier)
	if err != nil {
		return "", err
	}
	return clean([]byte(v)), nil
}

// StillIdentifierFile and VideoIdentifierFile read the identifier of the
// file at path.
func StillIdentifierFile(path string) (string, error) { return fromFile(path, StillIdentifier) }

// VideoIdentifierFile: see StillIdentifierFile.
func VideoIdentifierFile(path string) (string, error) { return fromFile(path, VideoIdentifier) }

func fromFile(path string, fn func(io.ReaderAt, int64) (string, error)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	return fn(f, fi.Size())
}

// entry is one 12-byte directory entry; value is its 4-byte value field.
type entry struct {
	typ   uint16
	count uint32
	value []byte
}

// find returns the entry tag of the directory at off in b (byte order bo).
// A malformed directory, or one with the tag twice, gives false.
func find(b []byte, bo binary.ByteOrder, off int64, tag uint16) (entry, bool) {
	if off < 0 || off+2 > int64(len(b)) {
		return entry{}, false
	}
	n := int64(bo.Uint16(b[off:]))
	if n == 0 || n > maxEntries || off+2+n*12 > int64(len(b)) {
		return entry{}, false
	}
	var e entry
	found := false
	for i := range n {
		p := b[off+2+i*12:]
		if bo.Uint16(p) != tag {
			continue
		}
		if found {
			return entry{}, false // ambiguous: do not pick one
		}
		e, found = entry{typ: bo.Uint16(p[2:]), count: bo.Uint32(p[4:]), value: p[8:12]}, true
	}
	return e, found
}

// data returns the bytes of e in b: inline when they fit the value field,
// else at the offset it holds (relative to the start of b).
func data(b []byte, bo binary.ByteOrder, e entry, size int64) ([]byte, bool) {
	if size <= 4 {
		return e.value[:size], true
	}
	o := int64(bo.Uint32(e.value))
	if o < 0 || o > int64(len(b)) || size > int64(len(b))-o {
		return nil, false
	}
	return b[o : o+size], true
}

// makerNoteIdentifier follows IFD0 -> Exif IFD -> MakerNote -> Apple tag
// 0x0011 in a TIFF block.
func makerNoteIdentifier(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	var bo binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return ""
	}
	if bo.Uint16(b[2:4]) != 42 {
		return ""
	}
	ifd0 := int64(bo.Uint32(b[4:8]))
	e, ok := find(b, bo, ifd0, tagExifIFD)
	if !ok || e.count != 1 || (e.typ != typeLong && e.typ != typeIFD) {
		return ""
	}
	e, ok = find(b, bo, int64(bo.Uint32(e.value)), tagMakerNote)
	if !ok || e.typ != typeUndefined || e.count < 16 {
		return ""
	}
	mn, ok := data(b, bo, e, int64(e.count))
	if !ok || string(mn[:len(appleHeader)]) != appleHeader {
		return ""
	}
	var mbo binary.ByteOrder
	switch string(mn[12:14]) {
	case "MM":
		mbo = binary.BigEndian
	case "II":
		mbo = binary.LittleEndian
	default:
		return ""
	}
	e, ok = find(mn, mbo, 14, tagContentIdentifier)
	if !ok || e.typ != typeASCII || e.count == 0 || e.count > MaxIdentifier+1 {
		return ""
	}
	v, ok := data(mn, mbo, e, int64(e.count))
	if !ok {
		return ""
	}
	return clean(v)
}

// clean returns an identifier without its terminating NUL, or "" unless
// it is 1..MaxIdentifier printable ASCII characters (no spaces).
func clean(v []byte) string {
	for i, c := range v {
		if c == 0 {
			v = v[:i]
			break
		}
	}
	if len(v) == 0 || len(v) > MaxIdentifier {
		return ""
	}
	for _, c := range v {
		if c <= 0x20 || c >= 0x7F {
			return ""
		}
	}
	return string(v)
}
