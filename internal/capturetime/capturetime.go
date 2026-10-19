// Package capturetime reads when a photo or video was taken from the
// metadata of its original: EXIF DateTimeOriginal (with OffsetTimeOriginal)
// in JPEG, TIFF and HEIF/HEIC/AVIF files, and the creation date of
// QuickTime/MP4 movies (com.apple.quicktime.creationdate, else the mvhd
// creation time).
//
// Originals are untrusted input. Extract never panics, never allocates
// more than a fixed amount, reads at most MaxReadBytes through at most
// maxReads ReadAt calls, and walks boxes and directories with count limits
// and a nesting depth fixed by the code (it only descends into the boxes it
// knows: meta, moov, iinf, ilst). Anything it cannot read with certainty gives "unknown"
// (the zero Time); it never guesses, and in particular never substitutes
// the upload time or a file system time. It only reads; the original is
// never changed. Nothing else (GPS position, camera) is extracted.
package capturetime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// Kind tells which parts of a capture time the original recorded.
type Kind uint8

const (
	// Unknown: no usable capture time.
	Unknown Kind = iota
	// Offset: the local time and its offset from UTC (an instant, and the
	// wall clock where it was taken).
	Offset
	// UTC: an instant, but not the local offset (QuickTime mvhd).
	UTC
	// Floating: a local wall-clock time without any zone (EXIF without
	// OffsetTimeOriginal); it cannot be converted to an instant.
	Floating
)

// Time is a capture time. Wall holds the recorded clock reading: for
// Offset in a fixed zone of the recorded offset, for UTC in UTC, for
// Floating in UTC as a carrier of the wall-clock fields only.
type Time struct {
	Kind Kind
	Wall time.Time
}

// Known reports whether t holds a capture time.
func (t Time) Known() bool { return t.Kind != Unknown }

// String is the canonical text form (ISO 8601, second precision):
//
//	Offset:   2026-10-19T14:03:22+09:00
//	UTC:      2026-10-19T05:03:22Z
//	Floating: 2026-10-19T14:03:22        (no zone designator)
//	Unknown:  ""
//
// An offset of zero recorded by the original is "+00:00", never "Z", so
// the suffix alone tells the three kinds apart.
func (t Time) String() string {
	switch t.Kind {
	case Offset:
		_, off := t.Wall.Zone()
		sign := byte('+')
		if off < 0 {
			sign, off = '-', -off
		}
		b := t.Wall.AppendFormat(nil, "2006-01-02T15:04:05")
		b = append(b, sign)
		b = append2(b, off/3600)
		b = append(b, ':')
		b = append2(b, off%3600/60)
		return string(b)
	case UTC:
		return t.Wall.UTC().Format("2006-01-02T15:04:05Z")
	case Floating:
		return t.Wall.Format("2006-01-02T15:04:05")
	}
	return ""
}

func append2(b []byte, n int) []byte { return append(b, byte('0'+n/10), byte('0'+n%10)) }

// Instant returns the capture time as a UTC instant, if the original
// recorded one (Offset and UTC).
func (t Time) Instant() (time.Time, bool) {
	if t.Kind == Offset || t.Kind == UTC {
		return t.Wall.UTC(), true
	}
	return time.Time{}, false
}

// Limits. They bound the work done for one original, whatever its bytes.
const (
	// MaxReadBytes is the most Extract reads from one original.
	MaxReadBytes = 1 << 20
	// maxReads bounds the ReadAt calls.
	maxReads = 4096
	// maxBlock is the largest single metadata block read into memory: a
	// JPEG APP1 segment is at most 64 KiB; a HEIF Exif item, a TIFF
	// header area or a QuickTime keys/ilst box is cut there.
	maxBlock = 256 << 10
	// maxBoxes bounds the ISO BMFF boxes looked at, over all levels.
	maxBoxes = 2048
	// maxSegments bounds the JPEG segments looked at before giving up.
	maxSegments = 128
	// maxIFDEntries bounds one TIFF directory.
	maxIFDEntries = 1024
	// maxItems bounds HEIF item info and location entries looked at.
	maxItems = 4096
)

// errBudget: the original needed more reading than the limits allow.
var errBudget = errors.New("capturetime: read budget exhausted")

// reader is a bounded view of the original.
type reader struct {
	r      io.ReaderAt
	size   int64
	left   int64 // bytes that may still be read
	reads  int
	ioErr  error // the first error of r other than io.EOF
	nboxes int
}

// readAt reads exactly n bytes at off, or fails. n must be at most
// maxBlock.
func (rd *reader) readAt(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || n > maxBlock || off > rd.size || n > rd.size-off {
		return nil, io.ErrUnexpectedEOF
	}
	if n > rd.left || rd.reads >= maxReads {
		return nil, errBudget
	}
	rd.left -= n
	rd.reads++
	b := make([]byte, n)
	got, err := rd.r.ReadAt(b, off)
	if got == len(b) {
		return b, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return nil, io.ErrUnexpectedEOF
	}
	if rd.ioErr == nil {
		rd.ioErr = err
	}
	return nil, err
}

// Extract reads the capture time of the original r of size bytes. The
// format is recognised from the content, not from a file name or MIME
// type. It returns the zero Time and a nil error when the original has no
// capture time it can read; the error is non-nil only when r itself failed
// (an I/O error), so the caller can try again later.
func Extract(r io.ReaderAt, size int64) (Time, error) {
	rd := &reader{r: r, size: size, left: MaxReadBytes}
	t := extract(rd)
	if rd.ioErr != nil {
		return Time{}, rd.ioErr
	}
	return t, nil
}

func extract(rd *reader) Time {
	if rd.size < 12 {
		return Time{}
	}
	head, err := rd.readAt(0, 12)
	if err != nil {
		return Time{}
	}
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		return fromJPEG(rd)
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		b, err := rd.readAt(0, min(rd.size, maxBlock))
		if err != nil {
			return Time{}
		}
		return fromTIFF(b)
	case isTopLevelBox(string(head[4:8])):
		return fromISOBMFF(rd)
	}
	return Time{}
}

// isTopLevelBox lists the box types an ISO BMFF or QuickTime file starts
// with (old QuickTime movies have no ftyp).
func isTopLevelBox(t string) bool {
	switch t {
	case "ftyp", "moov", "mdat", "wide", "free", "skip", "pnot":
		return true
	}
	return false
}

// ---- JPEG ----

// fromJPEG looks for the EXIF APP1 segment before the image data.
func fromJPEG(rd *reader) Time {
	off := int64(2)
	for range maxSegments {
		h, err := rd.readAt(off, 4)
		if err != nil {
			return Time{}
		}
		if h[0] != 0xFF {
			return Time{}
		}
		marker := h[1]
		switch {
		case marker == 0xFF: // fill byte
			off++
			continue
		case marker == 0xD8, marker == 0x01, marker >= 0xD0 && marker <= 0xD7:
			off += 2 // no length
			continue
		case marker == 0xDA, marker == 0xD9: // start of scan, end of image
			return Time{}
		}
		n := int64(binary.BigEndian.Uint16(h[2:4]))
		if n < 2 {
			return Time{}
		}
		if marker == 0xE1 && n >= 8 {
			seg, err := rd.readAt(off+4, n-2)
			if err != nil {
				return Time{}
			}
			if bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
				if t := fromTIFF(seg[6:]); t.Known() {
					return t
				}
			}
		}
		off += 2 + n
	}
	return Time{}
}

// ---- TIFF / EXIF ----

const (
	tagExifIFD            = 0x8769
	tagDateTimeOriginal   = 0x9003
	tagOffsetTimeOriginal = 0x9011
	typeASCII             = 2
	typeLong              = 4
)

type tiff struct {
	b  []byte
	bo binary.ByteOrder
}

type ifdEntry struct {
	typ   uint16
	count uint32
	value []byte // the 4-byte value/offset field
}

// fromTIFF reads DateTimeOriginal and OffsetTimeOriginal from a TIFF
// structure (the body of an EXIF block) held in b.
func fromTIFF(b []byte) Time {
	if len(b) < 8 {
		return Time{}
	}
	var t tiff
	switch string(b[:2]) {
	case "II":
		t.bo = binary.LittleEndian
	case "MM":
		t.bo = binary.BigEndian
	default:
		return Time{}
	}
	t.b = b
	if t.bo.Uint16(b[2:4]) != 42 {
		return Time{}
	}
	ifd0, ok := t.ifd(t.bo.Uint32(b[4:8]))
	if !ok {
		return Time{}
	}
	e, ok := ifd0[tagExifIFD]
	if !ok || e.count != 1 || (e.typ != typeLong && e.typ != 13) { // 13: IFD
		return Time{}
	}
	exif, ok := t.ifd(t.bo.Uint32(e.value))
	if !ok {
		return Time{}
	}
	dt, ok := t.ascii(exif[tagDateTimeOriginal])
	if !ok {
		return Time{}
	}
	wall, ok := parseExifDateTime(dt)
	if !ok {
		return Time{}
	}
	if ots, ok := t.ascii(exif[tagOffsetTimeOriginal]); ok {
		if off, ok := parseOffset(ots); ok {
			return Time{Kind: Offset, Wall: time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.FixedZone("", off))}
		}
	}
	return Time{Kind: Floating, Wall: wall}
}

// ifd reads the directory at off. Only the entries are kept; later
// directories (IFD1, thumbnails) are not followed.
func (t tiff) ifd(off uint32) (map[uint16]ifdEntry, bool) {
	o := int64(off)
	if o < 8 || o+2 > int64(len(t.b)) {
		return nil, false
	}
	n := int64(t.bo.Uint16(t.b[o:]))
	if n == 0 || n > maxIFDEntries || o+2+n*12 > int64(len(t.b)) {
		return nil, false
	}
	m := make(map[uint16]ifdEntry, min(n, 64))
	for i := range n {
		p := t.b[o+2+i*12:]
		tag := t.bo.Uint16(p)
		if tag != tagExifIFD && tag != tagDateTimeOriginal && tag != tagOffsetTimeOriginal {
			continue
		}
		if _, dup := m[tag]; dup {
			return nil, false // ambiguous: do not pick one
		}
		m[tag] = ifdEntry{typ: t.bo.Uint16(p[2:]), count: t.bo.Uint32(p[4:]), value: p[8:12]}
	}
	return m, true
}

// ascii returns an ASCII value without its terminating NUL(s).
func (t tiff) ascii(e ifdEntry) (string, bool) {
	if e.typ != typeASCII || e.count == 0 || e.count > 64 {
		return "", false
	}
	var v []byte
	if e.count <= 4 {
		v = e.value[:e.count]
	} else {
		o := int64(t.bo.Uint32(e.value))
		if o+int64(e.count) > int64(len(t.b)) {
			return "", false
		}
		v = t.b[o : o+int64(e.count)]
	}
	if i := bytes.IndexByte(v, 0); i >= 0 {
		v = v[:i]
	}
	return string(bytes.TrimRight(v, " ")), true
}

// parseExifDateTime parses "YYYY:MM:DD HH:MM:SS" (EXIF 2.3). Unset values
// ("0000:00:00 00:00:00", blanks) and impossible dates are rejected.
func parseExifDateTime(s string) (time.Time, bool) {
	if len(s) != 19 || s[4] != ':' || s[7] != ':' || s[10] != ' ' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	return wallClock(s[0:4], s[5:7], s[8:10], s[11:13], s[14:16], s[17:19])
}

// wallClock builds a validated wall-clock time (in UTC as a carrier).
func wallClock(ys, mos, ds, hs, mis, ss string) (time.Time, bool) {
	var v [6]int
	for i, f := range []string{ys, mos, ds, hs, mis, ss} {
		n, ok := digits(f)
		if !ok {
			return time.Time{}, false
		}
		v[i] = n
	}
	y, mo, d, h, mi, sec := v[0], v[1], v[2], v[3], v[4], v[5]
	if y < 1900 || mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, 0, time.UTC)
	if t.Day() != d { // 31 April and the like normalise into the next month
		return time.Time{}, false
	}
	return t, true
}

func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// parseOffset parses "+HH:MM", "-HH:MM" (EXIF), also "+HHMM" and "Z"
// (QuickTime), into seconds east of UTC. Offsets beyond ±14:00 are
// rejected.
func parseOffset(s string) (int, bool) {
	if s == "Z" {
		return 0, true
	}
	if len(s) != 6 && len(s) != 5 || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	hh, mm := s[1:3], s[3:5]
	if len(s) == 6 {
		if s[3] != ':' {
			return 0, false
		}
		mm = s[4:6]
	}
	h, ok1 := digits(hh)
	m, ok2 := digits(mm)
	if !ok1 || !ok2 || m > 59 || h*60+m > 14*60 {
		return 0, false
	}
	off := (h*60 + m) * 60
	if s[0] == '-' {
		off = -off
	}
	return off, true
}

// ---- ISO BMFF (HEIF, QuickTime, MP4) ----

type box struct {
	typ        string
	start, end int64 // the whole box
	body       int64 // first byte after the header
}

// boxes calls fn for every child box in [start, end) until fn returns
// false. It stops at the first malformed header.
func (rd *reader) boxes(start, end int64, fn func(b box) bool) {
	for off := start; off+8 <= end; {
		rd.nboxes++
		if rd.nboxes > maxBoxes {
			return
		}
		h, err := rd.readAt(off, 8)
		if err != nil {
			return
		}
		size := int64(binary.BigEndian.Uint32(h))
		hdr := int64(8)
		switch size {
		case 0: // to the end of the enclosing box
			size = end - off
		case 1: // 64-bit size follows
			if off+16 > end {
				return
			}
			l, err := rd.readAt(off+8, 8)
			if err != nil {
				return
			}
			u := binary.BigEndian.Uint64(l)
			if u > uint64(end-off) {
				return
			}
			size, hdr = int64(u), 16
		}
		if size < hdr || size > end-off {
			return
		}
		if !fn(box{typ: string(h[4:8]), start: off, end: off + size, body: off + hdr}) {
			return
		}
		off += size
	}
}

func fromISOBMFF(rd *reader) Time {
	var metaBox, moov *box
	rd.boxes(0, rd.size, func(b box) bool {
		switch b.typ {
		case "meta":
			if metaBox == nil {
				metaBox = &b
			}
		case "moov":
			if moov == nil {
				moov = &b
			}
		}
		return true
	})
	if metaBox != nil {
		if t := fromHEIF(rd, *metaBox); t.Known() {
			return t
		}
	}
	if moov != nil {
		return fromMovie(rd, *moov)
	}
	return Time{}
}

// fromHEIF finds the Exif item of a HEIF meta box (iinf + iloc) and reads
// its TIFF block.
func fromHEIF(rd *reader, m box) Time {
	// meta is a FullBox: version and flags first.
	if m.body+4 > m.end {
		return Time{}
	}
	var iinf, iloc, idat *box
	rd.boxes(m.body+4, m.end, func(b box) bool {
		switch b.typ {
		case "iinf":
			iinf = &b
		case "iloc":
			iloc = &b
		case "idat":
			idat = &b
		}
		return true
	})
	if iinf == nil || iloc == nil {
		return Time{}
	}
	id, ok := exifItemID(rd, *iinf)
	if !ok {
		return Time{}
	}
	data, ok := itemData(rd, *iloc, idat, id)
	if !ok || len(data) < 4 {
		return Time{}
	}
	// The Exif item starts with the offset of the TIFF header from the
	// end of this field (usually 6, past "Exif\0\0").
	skip := int64(binary.BigEndian.Uint32(data))
	if skip <= int64(len(data))-4-8 {
		if t := fromTIFF(data[4+skip:]); t.Known() {
			return t
		}
	}
	if bytes.HasPrefix(data[4:], []byte("Exif\x00\x00")) {
		return fromTIFF(data[10:])
	}
	return Time{}
}

// exifItemID returns the item_ID of the first item of type "Exif".
func exifItemID(rd *reader, iinf box) (uint32, bool) {
	b, err := rd.readAt(iinf.body, min(iinf.end-iinf.body, 6))
	if err != nil || len(b) < 6 {
		return 0, false
	}
	start := iinf.body + 6 // version/flags + u16 count
	if b[0] != 0 {
		start = iinf.body + 8 // u32 count
	}
	var id uint32
	found := false
	n := 0
	rd.boxes(start, iinf.end, func(e box) bool {
		n++
		if n > maxItems {
			return false
		}
		if e.typ != "infe" {
			return true
		}
		h, err := rd.readAt(e.body, min(e.end-e.body, 14))
		if err != nil || len(h) < 4 {
			return true
		}
		var itemType []byte
		var itemID uint32
		switch h[0] { // version
		case 2: // u16 item_ID, u16 protection index, item_type
			if len(h) < 12 {
				return true
			}
			itemID, itemType = uint32(binary.BigEndian.Uint16(h[4:6])), h[8:12]
		case 3: // u32 item_ID, u16 protection index, item_type
			if len(h) < 14 {
				return true
			}
			itemID, itemType = binary.BigEndian.Uint32(h[4:8]), h[10:14]
		default:
			return true // versions 0 and 1 carry no item type
		}
		if len(itemType) == 4 && string(itemType) == "Exif" {
			id, found = itemID, true
			return false
		}
		return true
	})
	return id, found
}

// itemData reads the extents of item id from the iloc box, concatenated
// and cut at maxBlock. Only file offsets (construction method 0) and idat
// offsets (1) are supported.
func itemData(rd *reader, iloc box, idat *box, id uint32) ([]byte, bool) {
	b, err := rd.readAt(iloc.body, min(iloc.end-iloc.body, maxBlock))
	if err != nil || len(b) < 8 {
		return nil, false
	}
	p := &cursor{b: b}
	version := p.u8()
	p.skip(3)
	sizes := p.u8()
	offSize, lenSize := int(sizes>>4), int(sizes&0xF)
	sizes2 := p.u8()
	baseSize, idxSize := int(sizes2>>4), 0
	if version == 1 || version == 2 {
		idxSize = int(sizes2 & 0xF)
	}
	if version > 2 || !validSize(offSize) || !validSize(lenSize) || !validSize(baseSize) || !validSize(idxSize) {
		return nil, false
	}
	var count uint32
	if version < 2 {
		count = uint32(p.u16())
	} else {
		count = p.u32()
	}
	for i := uint32(0); i < count && i < maxItems && !p.bad; i++ {
		var itemID uint32
		if version < 2 {
			itemID = uint32(p.u16())
		} else {
			itemID = p.u32()
		}
		method := 0
		if version == 1 || version == 2 {
			method = int(p.u16() & 0xF)
		}
		p.skip(2) // data_reference_index
		base := p.uint(baseSize)
		extents := int(p.u16())
		var out []byte
		ok := extents > 0 && extents <= 64
		for j := 0; j < extents && !p.bad; j++ {
			p.uint(idxSize)
			eo, el := p.uint(offSize), p.uint(lenSize)
			if itemID != id || !ok {
				continue
			}
			var origin, limit int64
			switch method {
			case 0:
				origin, limit = 0, rd.size
			case 1:
				if idat == nil {
					ok = false
					continue
				}
				origin, limit = idat.body, idat.end
			default:
				ok = false
				continue
			}
			if base > uint64(limit) || eo > uint64(limit) || el > uint64(limit) {
				ok = false
				continue
			}
			start := origin + int64(base) + int64(eo)
			n := int64(el)
			if el == 0 { // the rest of the data
				n = limit - start
			}
			n = min(n, int64(maxBlock-len(out)))
			if start < origin || start > limit || n < 0 || n > limit-start {
				ok = false
				continue
			}
			if n == 0 {
				continue
			}
			d, err := rd.readAt(start, n)
			if err != nil {
				ok = false
				continue
			}
			out = append(out, d...)
		}
		if itemID == id {
			return out, ok && !p.bad && len(out) > 0
		}
	}
	return nil, false
}

func validSize(n int) bool { return n == 0 || n == 4 || n == 8 }

// cursor reads big-endian fields from b; past the end it sets bad and
// returns zeros.
type cursor struct {
	b   []byte
	off int
	bad bool
}

func (c *cursor) take(n int) []byte {
	if c.bad || n < 0 || n > len(c.b)-c.off {
		c.bad = true
		return nil
	}
	v := c.b[c.off : c.off+n]
	c.off += n
	return v
}
func (c *cursor) skip(n int) { c.take(n) }
func (c *cursor) u8() uint8 {
	if v := c.take(1); v != nil {
		return v[0]
	}
	return 0
}
func (c *cursor) u16() uint16 {
	if v := c.take(2); v != nil {
		return binary.BigEndian.Uint16(v)
	}
	return 0
}
func (c *cursor) u32() uint32 {
	if v := c.take(4); v != nil {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}
func (c *cursor) uint(size int) uint64 {
	switch size {
	case 4:
		return uint64(c.u32())
	case 8:
		if v := c.take(8); v != nil {
			return binary.BigEndian.Uint64(v)
		}
	}
	return 0
}

// creationDateKey is the QuickTime metadata key iPhones (and others)
// write with the local time and offset of the recording.
const creationDateKey = "com.apple.quicktime.creationdate"

// macEpochOffset is 1904-01-01 to 1970-01-01 in seconds.
const macEpochOffset = 2082844800

// fromMovie reads moov/meta (keys + ilst) creationdate, else moov/mvhd.
func fromMovie(rd *reader, moov box) Time {
	var mvhd, metaBox *box
	rd.boxes(moov.body, moov.end, func(b box) bool {
		switch b.typ {
		case "mvhd":
			if mvhd == nil {
				mvhd = &b
			}
		case "meta":
			if metaBox == nil {
				metaBox = &b
			}
		}
		return true
	})
	if metaBox != nil {
		if t := quickTimeCreationDate(rd, *metaBox); t.Known() {
			return t
		}
	}
	if mvhd != nil {
		return mvhdTime(rd, *mvhd)
	}
	return Time{}
}

// mvhdTime reads the creation time of the movie header, seconds since
// 1904 in UTC. Zero (unset) and times before 1971 are rejected.
func mvhdTime(rd *reader, m box) Time {
	h, err := rd.readAt(m.body, min(m.end-m.body, 12))
	if err != nil || len(h) < 8 {
		return Time{}
	}
	var secs uint64
	switch h[0] {
	case 0:
		secs = uint64(binary.BigEndian.Uint32(h[4:8]))
	case 1:
		if len(h) < 12 {
			return Time{}
		}
		secs = binary.BigEndian.Uint64(h[4:12])
	default:
		return Time{}
	}
	if secs < macEpochOffset+365*24*3600 || secs > macEpochOffset+253402300799 { // 1971 .. 9999-12-31
		return Time{}
	}
	return Time{Kind: UTC, Wall: time.Unix(int64(secs-macEpochOffset), 0).UTC()}
}

// quickTimeCreationDate reads com.apple.quicktime.creationdate from a
// QuickTime metadata box (hdlr mdta, keys, ilst).
func quickTimeCreationDate(rd *reader, m box) Time {
	// In QuickTime files meta is a plain box; in ISO files a FullBox.
	start := m.body
	if p, err := rd.readAt(m.body, min(m.end-m.body, 8)); err == nil && len(p) == 8 && !isMetaChild(string(p[4:8])) {
		start += 4
	}
	var keys, ilst *box
	rd.boxes(start, m.end, func(b box) bool {
		switch b.typ {
		case "keys":
			keys = &b
		case "ilst":
			ilst = &b
		}
		return true
	})
	if keys == nil || ilst == nil {
		return Time{}
	}
	idx, ok := keyIndex(rd, *keys)
	if !ok {
		return Time{}
	}
	var t Time
	rd.boxes(ilst.body, ilst.end, func(item box) bool {
		if item.typ != string(binary.BigEndian.AppendUint32(nil, idx)) {
			return true
		}
		rd.boxes(item.body, item.end, func(d box) bool {
			if d.typ != "data" {
				return true
			}
			n := d.end - d.body
			if n < 8 || n > 8+64 {
				return false
			}
			v, err := rd.readAt(d.body, n)
			if err != nil || binary.BigEndian.Uint32(v[:4]) != 1 { // well-known type 1: UTF-8
				return false
			}
			t = parseISO8601(string(v[8:]))
			return false
		})
		return false
	})
	return t
}

func isMetaChild(t string) bool {
	switch t {
	case "hdlr", "keys", "ilst", "free", "skip":
		return true
	}
	return false
}

// keyIndex returns the 1-based index of creationDateKey in a keys box.
func keyIndex(rd *reader, k box) (uint32, bool) {
	b, err := rd.readAt(k.body, min(k.end-k.body, maxBlock))
	if err != nil || len(b) < 8 {
		return 0, false
	}
	p := &cursor{b: b}
	p.skip(4) // version, flags
	count := p.u32()
	for i := uint32(1); i <= count && i <= maxItems && !p.bad; i++ {
		size := p.u32()
		if size < 8 || p.bad {
			return 0, false
		}
		p.skip(4) // namespace, "mdta"
		name := p.take(int(size - 8))
		if p.bad {
			return 0, false
		}
		if string(name) == creationDateKey {
			return i, true
		}
	}
	return 0, false
}

// parseISO8601 parses the creationdate value: "2006-01-02T15:04:05" plus
// optional fractional seconds and a zone ("Z", "+0900", "+09:00"). Without
// a zone the time is Floating.
func parseISO8601(s string) Time {
	if len(s) < 19 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return Time{}
	}
	wall, ok := wallClock(s[0:4], s[5:7], s[8:10], s[11:13], s[14:16], s[17:19])
	if !ok {
		return Time{}
	}
	rest := s[19:]
	if len(rest) > 0 && (rest[0] == '.' || rest[0] == ',') {
		i := 1
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 1 {
			return Time{}
		}
		rest = rest[i:]
	}
	if rest == "" {
		return Time{Kind: Floating, Wall: wall}
	}
	off, ok := parseOffset(rest)
	if !ok {
		return Time{}
	}
	return Time{Kind: Offset, Wall: time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, time.FixedZone("", off))}
}

// Parse parses the canonical text form (Time.String) back. It is for
// stored values; "" gives the zero Time.
func Parse(s string) (Time, bool) {
	if s == "" {
		return Time{}, true
	}
	if len(s) == 20 && s[19] == 'Z' {
		t := parseISO8601(s[:19])
		if t.Kind != Floating {
			return Time{}, false
		}
		return Time{Kind: UTC, Wall: t.Wall}, true
	}
	t := parseISO8601(s)
	if !t.Known() || len(s) != 19 && len(s) != 25 {
		return Time{}, false
	}
	return t, true
}
