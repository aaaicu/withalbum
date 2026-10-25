package capturetime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	geo "github.com/aaaicu/withalbum/internal/capturetime/geotest"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

// All positions are synthetic (geo.SampleLat/Lon and offsets from it).

func locationOf(t *testing.T, b []byte) *Location {
	t.Helper()
	l, err := ExtractLocation(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("ExtractLocation: %v", err)
	}
	return l
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-7 }

func TestExtractLocation(t *testing.T) {
	lat, lon := geo.SampleLat, geo.SampleLon
	le := binary.LittleEndian
	for _, tc := range []struct {
		name               string
		b                  []byte
		lat, lon, accuracy float64
	}{
		{"jpeg", geo.JPEG(lat, lon, 0, 1), lat, lon, 0},
		{"jpeg with accuracy", geo.JPEG(lat, lon, 4.5, 2), lat, lon, 4.5},
		{"jpeg south west", geo.JPEG(-lat, -lon, 0, 3), -lat, -lon, 0},
		{"tiff little endian", geo.TIFF(le, nil, geo.GPS(le, -lat, lon, 30)), -lat, lon, 30},
		{"heic", geo.HEIC(lat, -lon, 12.25, 4), lat, -lon, 12.25},
		{"jpeg with exif and gps", lpt.JPEGWithTIFF(geo.LiveTIFF("ID-1", lat, lon, false), 5), lat, lon, 0},
		{"mov iso6709", geo.MOV(1, geo.Item{Key: "com.apple.quicktime.make", Value: "Test"},
			geo.Item{Key: iso6709Key, Value: "+12.3457+123.4568+012.345/"}), 12.3457, 123.4568, 0},
		{"mov iso6709 with accuracy", geo.MOV(2, geo.Item{Key: iso6709Key, Value: "-12.3457-023.4568/"},
			geo.Item{Key: accuracyKey, Value: "35.000000"}), -12.3457, -23.4568, 35},
		{"mov iso6709 crs", geo.MOV(3, geo.Item{Key: iso6709Key, Value: "+12.5+123.5CRSWGS_84/"}), 12.5, 123.5, 0},
		{"mov dms", geo.MOV(4, geo.Item{Key: iso6709Key, Value: "+122030+1232045.5/"}), 12 + 20.0/60 + 30.0/3600, 123 + 20.0/60 + 45.5/3600, 0},
		{"mov dm", geo.MOV(5, geo.Item{Key: iso6709Key, Value: "+1230.5+12345/"}), 12 + 30.5/60, 123 + 45.0/60, 0},
		{"mp4 xyz", geo.MP4XYZ("+12.3457+123.4568/", 6), 12.3457, 123.4568, 0},
		{"mp4 xyz no slash", geo.MP4XYZ("-01.5000+002.2500", 7), -1.5, 2.25, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := locationOf(t, tc.b)
			if l == nil || !near(l.Lat, tc.lat) || !near(l.Lon, tc.lon) || l.AccuracyM != tc.accuracy {
				t.Fatalf("got %+v, want %v %v ±%v", l, tc.lat, tc.lon, tc.accuracy)
			}
		})
	}
	// The capture time of the same originals is unaffected.
	b := lpt.JPEGWithTIFF(geo.TIFF(binary.BigEndian, []geo.Entry{geo.ASCII(tagDateTimeOriginal, "2026:10:19 14:03:22")},
		geo.GPS(binary.BigEndian, lat, lon, 0)), 9)
	if got := extractBytes(t, b); got.String() != "2026-10-19T14:03:22" {
		t.Fatalf("capture time next to GPS: %q", got.String())
	}
	if l := locationOf(t, b); l == nil || !near(l.Lat, lat) {
		t.Fatalf("location next to a capture time: %+v", l)
	}
}

func TestExtractLocationNone(t *testing.T) {
	be := binary.BigEndian
	lat, lon := geo.SampleLat, geo.SampleLon
	gps := func(mut func([]geo.Entry) []geo.Entry) []byte {
		return lpt.JPEGWithTIFF(geo.TIFF(be, nil, mut(geo.GPS(be, lat, lon, 5))), 1)
	}
	set := func(es []geo.Entry, e geo.Entry) []geo.Entry {
		for i := range es {
			if es[i].Tag == e.Tag {
				es[i] = e
				return es
			}
		}
		return append(es, e)
	}
	drop := func(es []geo.Entry, tag uint16) []geo.Entry {
		out := es[:0]
		for _, e := range es {
			if e.Tag != tag {
				out = append(out, e)
			}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"jpeg without exif", jpegWithoutExif()},
		{"exif without gps", lpt.JPEG("ID", 1)},
		{"null island", geo.JPEG(0, 0, 0, 1)},
		{"void", gps(func(es []geo.Entry) []geo.Entry { return set(es, geo.ASCII(tagGPSStatus, "V")) })},
		{"no lat ref", gps(func(es []geo.Entry) []geo.Entry { return drop(es, tagGPSLatRef) })},
		{"bad lon ref", gps(func(es []geo.Entry) []geo.Entry { return set(es, geo.ASCII(tagGPSLonRef, "X")) })},
		{"no lon", gps(func(es []geo.Entry) []geo.Entry { return drop(es, tagGPSLon) })},
		{"zero denominator", gps(func(es []geo.Entry) []geo.Entry {
			return set(es, geo.Rationals(be, tagGPSLat, [2]uint32{12, 1}, [2]uint32{3, 0}, [2]uint32{0, 1}))
		})},
		{"minutes 60", gps(func(es []geo.Entry) []geo.Entry {
			return set(es, geo.Rationals(be, tagGPSLat, [2]uint32{12, 1}, [2]uint32{60, 1}, [2]uint32{0, 1}))
		})},
		{"lat 91", gps(func(es []geo.Entry) []geo.Entry {
			return set(es, geo.Rationals(be, tagGPSLat, [2]uint32{91, 1}, [2]uint32{0, 1}, [2]uint32{0, 1}))
		})},
		{"lat 89 59 60", gps(func(es []geo.Entry) []geo.Entry {
			return set(es, geo.Rationals(be, tagGPSLat, [2]uint32{90, 1}, [2]uint32{0, 1}, [2]uint32{1, 1}))
		})},
		{"two values", gps(func(es []geo.Entry) []geo.Entry {
			return set(es, geo.Rationals(be, tagGPSLat, [2]uint32{12, 1}, [2]uint32{3, 1}))
		})},
		{"wrong type", gps(func(es []geo.Entry) []geo.Entry {
			return set(es, geo.Entry{Tag: tagGPSLat, Type: 4, Count: 3, Data: make([]byte, 12)})
		})},
		{"duplicate tag", gps(func(es []geo.Entry) []geo.Entry { return append(es, geo.ASCII(tagGPSLatRef, "S")) })},
		{"mov other datum", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+12.5+123.5CRSEPSG4326X/"})},
		{"mov garbage", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "somewhere nice"})},
		{"mov lat 3 digits", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+123.5+123.5/"})},
		{"mov lon 200", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+12.5+200.5/"})},
		{"mov minutes 75", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+1275+12345/"})},
		{"mov trailing", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+12.5+123.5/x"})},
		{"mov null island", geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+00.0000+000.0000/"})},
		{"mov binary type", movWithType(2)},
		{"mp4 xyz short length", geo.MP4XYZ("", 1)},
		{"plain movie", lpt.MOV("", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if l := locationOf(t, tc.b); l != nil {
				t.Fatalf("got %+v, want none", l)
			}
		})
	}
	// An out-of-range accuracy is dropped, the position kept.
	for _, acc := range []string{"0", "-3", "NaN", "Inf", "1e9", "abc"} {
		l := locationOf(t, geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+12.5+123.5/"}, geo.Item{Key: accuracyKey, Value: acc}))
		if l == nil || l.AccuracyM != 0 {
			t.Fatalf("accuracy %q: %+v", acc, l)
		}
	}
	l := locationOf(t, gps(func(es []geo.Entry) []geo.Entry {
		return set(es, geo.Rationals(be, tagGPSHPosError, [2]uint32{1, 0}))
	}))
	if l == nil || l.AccuracyM != 0 {
		t.Fatalf("accuracy with zero denominator: %+v", l)
	}
}

// movWithType is a movie whose ISO6709 item is not UTF-8 text.
func movWithType(typ uint32) []byte {
	b := geo.MOV(1, geo.Item{Key: iso6709Key, Value: "+12.5+123.5/"})
	i := bytes.Index(b, []byte("data\x00\x00\x00\x01"))
	binary.BigEndian.PutUint32(b[i+4:], typ)
	return b
}

func TestLocationLimitsAndErrors(t *testing.T) {
	b := geo.JPEG(geo.SampleLat, geo.SampleLon, 0, 1)
	for n := range len(b) {
		_, _ = ExtractLocation(bytes.NewReader(b[:n]), int64(n)) // never panics
	}
	r := &countingReader{data: b, size: int64(len(b))}
	if l, err := ExtractLocation(r, r.size); err != nil || l == nil {
		t.Fatalf("%+v %v", l, err)
	}
	boom := errors.New("disk on fire")
	if _, err := ExtractLocation(&countingReader{size: 1000, fail: boom}, 1000); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the I/O error", err)
	}
	// A movie whose moov follows a 6 GiB mdat is read by skipping it.
	const mdatSize = 6 << 30
	head := mkbox("ftyp", []byte("mp42"), be32(0))
	mdat := binary.BigEndian.AppendUint64(append([]byte{0, 0, 0, 1}, "mdat"...), mdatSize)
	xyz := "+12.5+123.5/"
	moov := mkbox("moov", mkbox("udta", mkbox("\xa9xyz", be16(uint16(len(xyz))), be16(0x15c7), []byte(xyz))))
	moovOff := int64(len(head)) + mdatSize
	sr := &sparseReader{parts: map[int64][]byte{0: append(head, mdat...), moovOff: moov}, size: moovOff + int64(len(moov))}
	if l, err := ExtractLocation(sr, sr.size); err != nil || l == nil || l.Lat != 12.5 {
		t.Fatalf("after a large mdat: %+v %v", l, err)
	}
	if sr.read > 4096 {
		t.Fatalf("read %d bytes", sr.read)
	}
	// Endless boxes stop at the box limit, within MaxReadBytes.
	var many []byte
	for range 100000 {
		many = append(many, mkbox("free")...)
	}
	big := append(mkbox("ftyp", []byte("mp42"), be32(0)), many...)
	cr := &countingReader{data: big, size: int64(len(big))}
	if l, _ := ExtractLocation(cr, cr.size); l != nil || cr.read > MaxReadBytes || cr.calls > maxReads {
		t.Fatalf("endless boxes: %+v, %d bytes in %d calls", l, cr.read, cr.calls)
	}
}

func TestISO6709RoundTrip(t *testing.T) {
	for _, p := range [][2]float64{{geo.SampleLat, geo.SampleLon}, {-geo.SampleLat, -geo.SampleLon}, {1.5, -0.25}, {-89.9999, 179.9999}} {
		l := parseISO6709(geo.ISO6709(p[0], p[1]))
		if l == nil || math.Abs(l.Lat-p[0]) > 1e-4 || math.Abs(l.Lon-p[1]) > 1e-4 {
			t.Fatalf("%v: %+v", p, l)
		}
	}
}

func locationFixtures() [][]byte {
	lat, lon := geo.SampleLat, geo.SampleLon
	return [][]byte{
		geo.JPEG(lat, lon, 4.5, 1),
		geo.TIFF(binary.LittleEndian, nil, geo.GPS(binary.LittleEndian, -lat, lon, 30)),
		geo.HEIC(lat, -lon, 12.25, 4),
		lpt.JPEGWithTIFF(geo.LiveTIFF("ID-1", lat, lon, false), 5),
		geo.MOV(2, geo.Item{Key: iso6709Key, Value: "-12.3457-023.4568/"}, geo.Item{Key: accuracyKey, Value: "35"}),
		geo.MP4XYZ("+12.3457+123.4568/", 6),
	}
}

func FuzzExtractLocation(f *testing.F) {
	for _, b := range locationFixtures() {
		f.Add(b)
	}
	for _, b := range fixtures() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := &countingReader{data: b, size: int64(len(b))}
		l, err := ExtractLocation(r, r.size)
		if err != nil {
			t.Fatalf("no I/O error expected: %v", err)
		}
		if r.read > MaxReadBytes || r.calls > maxReads {
			t.Fatalf("read %d bytes in %d calls", r.read, r.calls)
		}
		if l != nil {
			if !ValidPosition(l.Lat, l.Lon) || l.AccuracyM < 0 || l.AccuracyM > MaxAccuracyM || math.IsNaN(l.AccuracyM) {
				t.Fatalf("invalid location %+v", l)
			}
		}
	})
}

func FuzzISO6709(f *testing.F) {
	for _, s := range []string{"+12.3457+123.4568/", "+122030+1232045.5/", "+12.5+123.5+10.5CRSWGS_84/", "-01.5000+002.2500"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if l := parseISO6709(s); l != nil && !ValidPosition(l.Lat, l.Lon) {
			t.Fatalf("%q: %+v", s, l)
		}
	})
}

// jpegWithoutExif is a JPEG whose only APP1 is not EXIF.
func jpegWithoutExif() []byte { return buildJPEG(nil) }
