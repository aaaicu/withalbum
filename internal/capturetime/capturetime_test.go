package capturetime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

// The fixtures are built by hand here; no real photo is committed.

type entry struct {
	tag, typ uint16
	count    uint32
	data     []byte // inline (<= 4 bytes) or stored after the directory
}

func asciiEntry(tag uint16, s string) entry {
	return entry{tag: tag, typ: typeASCII, count: uint32(len(s) + 1), data: append([]byte(s), 0)}
}

// buildTIFF returns a TIFF block whose IFD0 points to an Exif IFD with
// the given entries.
func buildTIFF(bo binary.ByteOrder, exif []entry) []byte {
	var b bytes.Buffer
	if bo == binary.LittleEndian {
		b.WriteString("II")
	} else {
		b.WriteString("MM")
	}
	w16 := func(v uint16) { _ = binary.Write(&b, bo, v) }
	w32 := func(v uint32) { _ = binary.Write(&b, bo, v) }
	w16(42)
	w32(8)
	// IFD0: one entry (Exif IFD pointer), next IFD 0.
	exifOff := uint32(8 + 2 + 12 + 4)
	w16(1)
	w16(tagExifIFD)
	w16(typeLong)
	w32(1)
	w32(exifOff)
	w32(0)
	// Exif IFD.
	dataOff := exifOff + 2 + uint32(len(exif))*12 + 4
	var data bytes.Buffer
	w16(uint16(len(exif)))
	for _, e := range exif {
		w16(e.tag)
		w16(e.typ)
		w32(e.count)
		if len(e.data) <= 4 {
			v := make([]byte, 4)
			copy(v, e.data)
			b.Write(v)
		} else {
			w32(dataOff + uint32(data.Len()))
			data.Write(e.data)
		}
	}
	w32(0)
	b.Write(data.Bytes())
	return b.Bytes()
}

func exifTIFF(dt, offset string) []byte {
	es := []entry{asciiEntry(tagDateTimeOriginal, dt)}
	if offset != "" {
		es = append(es, asciiEntry(tagOffsetTimeOriginal, offset))
	}
	return buildTIFF(binary.BigEndian, es)
}

func segment(marker byte, payload []byte) []byte {
	b := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(len(payload)+2))
	return append(b, payload...)
}

func buildJPEG(tiffBlock []byte) []byte {
	b := []byte{0xFF, 0xD8}
	b = append(b, segment(0xE0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))...)
	if tiffBlock != nil {
		b = append(b, segment(0xE1, append([]byte("Exif\x00\x00"), tiffBlock...))...)
	}
	b = append(b, segment(0xDB, make([]byte, 65))...)
	b = append(b, segment(0xDA, []byte{1, 1, 0, 0, 0x3f, 0})...)
	b = append(b, 0x12, 0x34, 0xFF, 0xD9)
	return b
}

func mkbox(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func infe(version byte, id uint32, typ string) []byte {
	h := []byte{version, 0, 0, 0}
	if version == 3 {
		h = append(h, be32(id)...)
	} else {
		h = append(h, be16(uint16(id))...)
	}
	h = append(h, 0, 0) // protection index
	h = append(h, typ...)
	h = append(h, 0) // item_name ""
	return mkbox("infe", h)
}

// exifItem is the content of a HEIF Exif item.
func exifItem(tiffBlock []byte) []byte {
	return append(append(be32(6), "Exif\x00\x00"...), tiffBlock...)
}

// buildHEIF puts the Exif item either in mdat (file offset) or in idat.
func buildHEIF(item []byte, inIdat bool, infeVersion byte) []byte {
	ftyp := mkbox("ftyp", []byte("heic"), be32(0), []byte("mif1heic"))
	meta := func(offset uint32) []byte {
		method := uint16(0)
		if inIdat {
			method = 1
		}
		iloc := []byte{1, 0, 0, 0, 0x44, 0x00} // v1, offset 4, length 4, base 0, index 0
		iloc = append(iloc, be16(2)...)
		// item 1 (the image): somewhere in mdat
		iloc = append(iloc, be16(1)...)
		iloc = append(iloc, be16(0)...)
		iloc = append(iloc, be16(0)...)
		iloc = append(iloc, be16(1)...)
		iloc = append(iloc, be32(offset)...)
		iloc = append(iloc, be32(1)...)
		// item 2 (Exif)
		iloc = append(iloc, be16(2)...)
		iloc = append(iloc, be16(method)...)
		iloc = append(iloc, be16(0)...)
		iloc = append(iloc, be16(1)...)
		if inIdat {
			iloc = append(iloc, be32(0)...)
		} else {
			iloc = append(iloc, be32(offset)...)
		}
		iloc = append(iloc, be32(uint32(len(item)))...)
		iinf := mkbox("iinf", []byte{0, 0, 0, 0}, be16(2), infe(infeVersion, 1, "hvc1"), infe(infeVersion, 2, "Exif"))
		parts := [][]byte{{0, 0, 0, 0}, mkbox("hdlr", make([]byte, 8), []byte("pict"), make([]byte, 13)), iinf, mkbox("iloc", iloc)}
		if inIdat {
			parts = append(parts, mkbox("idat", item))
		}
		return mkbox("meta", parts...)
	}
	m := meta(0)
	off := uint32(len(ftyp) + len(m) + 8)
	m = meta(off)
	mdat := mkbox("mdat", item)
	if inIdat {
		mdat = mkbox("mdat", []byte("image data"))
	}
	return bytes.Join([][]byte{ftyp, m, mdat}, nil)
}

func mvhd(secs uint32) []byte {
	return mkbox("mvhd", []byte{0, 0, 0, 0}, be32(secs), be32(secs), be32(600), be32(0), make([]byte, 80))
}

func qtMeta(creationDate string, fullBox bool) []byte {
	key := creationDateKey
	keys := mkbox("keys", []byte{0, 0, 0, 0}, be32(2),
		be32(uint32(8+len("com.apple.quicktime.make"))), []byte("mdta"), []byte("com.apple.quicktime.make"),
		be32(uint32(8+len(key))), []byte("mdta"), []byte(key))
	data := mkbox("data", be32(1), be32(0), []byte(creationDate))
	ilst := mkbox("ilst",
		mkbox(string(be32(1)), mkbox("data", be32(1), be32(0), []byte("Apple"))),
		mkbox(string(be32(2)), data))
	hdlr := mkbox("hdlr", make([]byte, 8), []byte("mdta"), make([]byte, 13))
	if fullBox {
		return mkbox("meta", []byte{0, 0, 0, 0}, hdlr, keys, ilst)
	}
	return mkbox("meta", hdlr, keys, ilst)
}

func buildMOV(moovParts ...[]byte) []byte {
	return bytes.Join([][]byte{
		mkbox("ftyp", []byte("qt  "), be32(0), []byte("qt  ")),
		mkbox("wide"),
		mkbox("mdat", make([]byte, 100)),
		mkbox("moov", moovParts...),
	}, nil)
}

func mac(t time.Time) uint32 { return uint32(t.Unix() + macEpochOffset) }

func extractBytes(t *testing.T, b []byte) Time {
	t.Helper()
	got, err := Extract(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return got
}

func TestExtract(t *testing.T) {
	rec := time.Date(2026, 10, 19, 5, 3, 22, 0, time.UTC)
	cases := []struct {
		name string
		in   []byte
		want string
		kind Kind
	}{
		{"jpeg offset", buildJPEG(exifTIFF("2026:10:19 14:03:22", "+09:00")), "2026-10-19T14:03:22+09:00", Offset},
		{"jpeg negative offset", buildJPEG(exifTIFF("2026:01:02 03:04:05", "-03:30")), "2026-01-02T03:04:05-03:30", Offset},
		{"jpeg zero offset", buildJPEG(exifTIFF("2026:01:02 03:04:05", "+00:00")), "2026-01-02T03:04:05+00:00", Offset},
		{"jpeg no offset", buildJPEG(exifTIFF("2026:10:19 14:03:22", "")), "2026-10-19T14:03:22", Floating},
		{"jpeg blank offset", buildJPEG(exifTIFF("2026:10:19 14:03:22", "   :  ")), "2026-10-19T14:03:22", Floating},
		{"jpeg little endian", buildJPEG(buildTIFF(binary.LittleEndian, []entry{asciiEntry(tagDateTimeOriginal, "2020:02:29 23:59:59"), asciiEntry(tagOffsetTimeOriginal, "+05:45")})), "2020-02-29T23:59:59+05:45", Offset},
		{"jpeg unset date", buildJPEG(exifTIFF("0000:00:00 00:00:00", "+09:00")), "", Unknown},
		{"jpeg blank date", buildJPEG(exifTIFF("    :  :     :  :  ", "")), "", Unknown},
		{"jpeg impossible date", buildJPEG(exifTIFF("2026:02:30 10:00:00", "")), "", Unknown},
		{"jpeg bad offset", buildJPEG(exifTIFF("2026:10:19 14:03:22", "+25:00")), "2026-10-19T14:03:22", Floating},
		{"jpeg no exif", buildJPEG(nil), "", Unknown},
		{"jpeg exif without date", buildJPEG(buildTIFF(binary.BigEndian, []entry{asciiEntry(0x9004, "2026:10:19 14:03:22")})), "", Unknown},
		{"tiff", exifTIFF("2026:10:19 14:03:22", "+09:00"), "2026-10-19T14:03:22+09:00", Offset},
		{"heic mdat", buildHEIF(exifItem(exifTIFF("2026:10:19 14:03:22", "+09:00")), false, 2), "2026-10-19T14:03:22+09:00", Offset},
		{"heic idat", buildHEIF(exifItem(exifTIFF("2026:10:19 14:03:22", "")), true, 2), "2026-10-19T14:03:22", Floating},
		{"heic infe v3", buildHEIF(exifItem(exifTIFF("2026:10:19 14:03:22", "+01:00")), false, 3), "2026-10-19T14:03:22+01:00", Offset},
		{"heic without exif date", buildHEIF(exifItem(buildTIFF(binary.BigEndian, []entry{asciiEntry(0x0110, "x")})), false, 2), "", Unknown},
		{"mov creationdate", buildMOV(mvhd(mac(rec)), qtMeta("2026-10-19T14:03:22+0900", false)), "2026-10-19T14:03:22+09:00", Offset},
		{"mp4 creationdate fullbox meta", buildMOV(mvhd(mac(rec)), qtMeta("2026-10-19T14:03:22.123-07:00", true)), "2026-10-19T14:03:22-07:00", Offset},
		{"mov creationdate no zone", buildMOV(qtMeta("2026-10-19T14:03:22", false)), "2026-10-19T14:03:22", Floating},
		{"mov bad creationdate falls back to mvhd", buildMOV(mvhd(mac(rec)), qtMeta("yesterday", false)), "2026-10-19T05:03:22Z", UTC},
		{"mp4 mvhd only", buildMOV(mvhd(mac(rec))), "2026-10-19T05:03:22Z", UTC},
		{"mp4 mvhd unset", buildMOV(mvhd(0)), "", Unknown},
		{"mp4 no moov", mkbox("ftyp", []byte("isom"), be32(0)), "", Unknown},
		{"png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...), "", Unknown},
		{"empty", nil, "", Unknown},
		{"short", []byte{0xFF, 0xD8, 0xFF}, "", Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractBytes(t, c.in)
			if got.String() != c.want || got.Kind != c.kind {
				t.Fatalf("got %q kind %d, want %q kind %d", got.String(), got.Kind, c.want, c.kind)
			}
			back, ok := Parse(got.String())
			if !ok || back.String() != got.String() || back.Kind != got.Kind {
				t.Fatalf("Parse(%q) = %q kind %d %v", got.String(), back.String(), back.Kind, ok)
			}
		})
	}
}

func TestInstant(t *testing.T) {
	got := extractBytes(t, buildJPEG(exifTIFF("2026:10:19 14:03:22", "+09:00")))
	if in, ok := got.Instant(); !ok || !in.Equal(time.Date(2026, 10, 19, 5, 3, 22, 0, time.UTC)) {
		t.Fatalf("Instant = %v %v", in, ok)
	}
	if _, ok := extractBytes(t, buildJPEG(exifTIFF("2026:10:19 14:03:22", ""))).Instant(); ok {
		t.Fatal("a floating time has no instant")
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"x", "2026-10-19", "2026-10-19T14:03:22.5Z", "2026-10-19T14:03:22+0900", "2026-13-19T14:03:22", "2026-10-19 14:03:22"} {
		if _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
	if tm, ok := Parse(""); !ok || tm.Known() {
		t.Fatal(`Parse("") must be the unknown time`)
	}
}

// Every truncation of every fixture must be handled without a panic.
func TestTruncated(t *testing.T) {
	for _, f := range fixtures() {
		for n := range len(f) {
			_ = extractBytes(t, f[:n])
		}
	}
}

// countingReader counts the bytes read and can present a huge file whose
// unwritten parts read as zeros.
type countingReader struct {
	data  []byte
	size  int64
	read  int64
	calls int
	fail  error
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	c.read += int64(len(p))
	if c.fail != nil {
		return 0, c.fail
	}
	if off >= c.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off+int64(n) < c.size {
		i := off + int64(n)
		if i < int64(len(c.data)) {
			p[n] = c.data[i]
		} else {
			p[n] = 0
		}
		n++
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// A movie whose moov follows a 6 GiB mdat (64-bit size) is read by
// skipping the mdat, not by reading it.
func TestLargeMdatSkipped(t *testing.T) {
	const mdatSize = 6 << 30
	head := mkbox("ftyp", []byte("isom"), be32(0))
	mdat := append([]byte{0, 0, 0, 1}, "mdat"...)
	mdat = binary.BigEndian.AppendUint64(mdat, mdatSize)
	moov := mkbox("moov", mvhd(mac(time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC))))
	// The moov is placed after the virtual mdat.
	moovOff := int64(len(head)) + mdatSize
	r := &sparseReader{parts: map[int64][]byte{0: append(head, mdat...), moovOff: moov}, size: moovOff + int64(len(moov))}
	got, err := Extract(r, r.size)
	if err != nil || got.String() != "2024-05-06T07:08:09Z" {
		t.Fatalf("got %q %v", got.String(), err)
	}
	if r.read > 4096 {
		t.Fatalf("read %d bytes", r.read)
	}
}

type sparseReader struct {
	parts map[int64][]byte
	size  int64
	read  int64
}

func (s *sparseReader) ReadAt(p []byte, off int64) (int, error) {
	s.read += int64(len(p))
	clear(p)
	for start, b := range s.parts {
		for i := range p {
			j := off + int64(i) - start
			if j >= 0 && j < int64(len(b)) {
				p[i] = b[j]
			}
		}
	}
	if off+int64(len(p)) > s.size {
		return int(max(s.size-off, 0)), io.EOF
	}
	return len(p), nil
}

// A JPEG made of endless small segments stops at the segment limit; one
// with a huge chain of boxes stops at the box limit; reading never goes
// past MaxReadBytes.
func TestLimits(t *testing.T) {
	b := []byte{0xFF, 0xD8}
	for range 100000 {
		b = append(b, segment(0xE2, make([]byte, 30))...)
	}
	b = append(b, segment(0xE1, append([]byte("Exif\x00\x00"), exifTIFF("2026:10:19 14:03:22", "")...))...)
	r := &countingReader{data: b, size: int64(len(b))}
	if got, err := Extract(r, r.size); err != nil || got.Known() {
		t.Fatalf("got %q %v; the Exif segment is beyond the segment limit", got.String(), err)
	}
	if r.read > MaxReadBytes || r.calls > maxReads {
		t.Fatalf("read %d bytes in %d calls", r.read, r.calls)
	}

	var boxes []byte
	boxes = append(boxes, mkbox("ftyp", []byte("isom"), be32(0))...)
	for range 100000 {
		boxes = append(boxes, mkbox("free")...)
	}
	boxes = append(boxes, mkbox("moov", mvhd(mac(time.Now())))...)
	r = &countingReader{data: boxes, size: int64(len(boxes))}
	if got, err := Extract(r, r.size); err != nil || got.Known() {
		t.Fatalf("got %q %v; moov is beyond the box limit", got.String(), err)
	}
	if r.read > MaxReadBytes || r.calls > maxReads {
		t.Fatalf("read %d bytes in %d calls", r.read, r.calls)
	}

	// A file that claims to be huge but is short reads only what exists.
	r = &countingReader{data: buildJPEG(nil), size: 1 << 40}
	if _, err := Extract(r, r.size); err != nil {
		t.Fatal(err)
	}
}

// Nested boxes deeper than the walk goes are simply not found.
func TestDeepNesting(t *testing.T) {
	inner := mvhd(mac(time.Now()))
	for range 200 {
		inner = mkbox("moov", inner)
	}
	b := append(mkbox("ftyp", []byte("isom"), be32(0)), inner...)
	_ = extractBytes(t, b) // must terminate without a panic
}

func TestIOError(t *testing.T) {
	boom := errors.New("disk on fire")
	r := &countingReader{size: 1000, fail: boom}
	if _, err := Extract(r, r.size); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the I/O error", err)
	}
}

func fixtures() [][]byte {
	rec := time.Date(2026, 10, 19, 5, 3, 22, 0, time.UTC)
	return [][]byte{
		buildJPEG(exifTIFF("2026:10:19 14:03:22", "+09:00")),
		buildJPEG(buildTIFF(binary.LittleEndian, []entry{asciiEntry(tagDateTimeOriginal, "2020:02:29 23:59:59")})),
		buildHEIF(exifItem(exifTIFF("2026:10:19 14:03:22", "+09:00")), false, 2),
		buildHEIF(exifItem(exifTIFF("2026:10:19 14:03:22", "")), true, 3),
		buildMOV(mvhd(mac(rec)), qtMeta("2026-10-19T14:03:22+0900", false)),
		buildMOV(mvhd(mac(rec)), qtMeta("2026-10-19T14:03:22Z", true)),
		exifTIFF("2026:10:19 14:03:22", "-04:00"),
	}
}

func FuzzExtract(f *testing.F) {
	for _, b := range fixtures() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := &countingReader{data: b, size: int64(len(b))}
		got, err := Extract(r, r.size)
		if err != nil {
			t.Fatalf("no I/O error expected: %v", err)
		}
		if r.read > MaxReadBytes || r.calls > maxReads {
			t.Fatalf("read %d bytes in %d calls", r.read, r.calls)
		}
		if got.Known() {
			back, ok := Parse(got.String())
			if !ok || back.String() != got.String() || back.Kind != got.Kind {
				t.Fatalf("round trip of %q: %q %v", got.String(), back.String(), ok)
			}
		}
	})
}
