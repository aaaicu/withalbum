// Package geotest builds small, hand-made originals that record a
// location, for tests: TIFF/EXIF blocks with a GPS directory (in a JPEG or
// a HEIC through livephototest), and QuickTime/MP4 movies with an ISO 6709
// location. No real photo and no real place is used: tests pass synthetic
// coordinates (see Sample). Import it only from tests.
package geotest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

// Sample is an obviously synthetic test position (the digits 1 to 9 in
// order, not a place anyone recorded); tests use it and offsets from it.
const (
	SampleLat = 12.3456789
	SampleLon = 123.4567891
)

// ByteOrder is binary.BigEndian or binary.LittleEndian.
type ByteOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// Entry is one TIFF directory entry; Data is already in the byte order of
// the TIFF it goes into (inline when at most 4 bytes).
type Entry struct {
	Tag   uint16
	Type  uint16
	Count uint32
	Data  []byte
}

// ASCII is an ASCII entry (NUL-terminated).
func ASCII(tag uint16, s string) Entry {
	return Entry{Tag: tag, Type: 2, Count: uint32(len(s) + 1), Data: append([]byte(s), 0)}
}

// Rationals is an unsigned RATIONAL entry of the given numerator,
// denominator pairs.
func Rationals(bo ByteOrder, tag uint16, vals ...[2]uint32) Entry {
	var b []byte
	for _, v := range vals {
		b = bo.AppendUint32(b, v[0])
		b = bo.AppendUint32(b, v[1])
	}
	return Entry{Tag: tag, Type: 5, Count: uint32(len(vals)), Data: b}
}

// DMS splits |deg| into degrees, minutes and seconds rationals (seconds
// with a denominator of 1e6).
func DMS(deg float64) [][2]uint32 {
	a := math.Abs(deg)
	d := math.Floor(a)
	m := math.Floor((a - d) * 60)
	s := ((a-d)*60 - m) * 60
	return [][2]uint32{{uint32(d), 1}, {uint32(m), 1}, {uint32(math.Round(s * 1e6)), 1e6}}
}

// GPS returns the GPS directory entries of a position (accuracy 0: no
// GPSHPositioningError).
func GPS(bo ByteOrder, lat, lon, accuracyM float64) []Entry {
	latRef, lonRef := "N", "E"
	if lat < 0 {
		latRef = "S"
	}
	if lon < 0 {
		lonRef = "W"
	}
	es := []Entry{
		{Tag: 0, Type: 1, Count: 4, Data: []byte{2, 3, 0, 0}}, // GPSVersionID
		ASCII(1, latRef),
		Rationals(bo, 2, DMS(lat)...),
		ASCII(3, lonRef),
		Rationals(bo, 4, DMS(lon)...),
		{Tag: 5, Type: 1, Count: 1, Data: []byte{0, 0, 0, 0}}, // altitude ref: above sea level
		Rationals(bo, 6, [2]uint32{12345, 1000}),              // altitude (never read)
	}
	if accuracyM > 0 {
		es = append(es, Rationals(bo, 0x1F, [2]uint32{uint32(math.Round(accuracyM * 100)), 100}))
	}
	return es
}

// TIFF returns a TIFF block: IFD0 with pointers to an Exif IFD (when exif
// is not nil) and a GPS IFD (when gps is not nil).
func TIFF(bo ByteOrder, exif, gps []Entry) []byte {
	var hdr bytes.Buffer
	if bo.String() == binary.LittleEndian.String() {
		hdr.WriteString("II")
	} else {
		hdr.WriteString("MM")
	}
	hdr.Write(bo.AppendUint16(nil, 42))
	hdr.Write(bo.AppendUint32(nil, 8))
	type dir struct {
		tag     uint16
		entries []Entry
	}
	var dirs []dir
	if exif != nil {
		dirs = append(dirs, dir{0x8769, exif})
	}
	if gps != nil {
		dirs = append(dirs, dir{0x8825, gps})
	}
	ifdSize := func(n int) uint32 { return uint32(2 + n*12 + 4) }
	// Layout: header, IFD0, then each sub-directory followed by its data.
	off := uint32(8) + ifdSize(len(dirs))
	var body bytes.Buffer
	ifd0 := bo.AppendUint16(nil, uint16(len(dirs)))
	for _, d := range dirs {
		ifd0 = bo.AppendUint16(ifd0, d.tag)
		ifd0 = bo.AppendUint16(ifd0, 4)
		ifd0 = bo.AppendUint32(ifd0, 1)
		ifd0 = bo.AppendUint32(ifd0, off+uint32(body.Len()))
		dataOff := off + uint32(body.Len()) + ifdSize(len(d.entries))
		var data bytes.Buffer
		ifd := bo.AppendUint16(nil, uint16(len(d.entries)))
		for _, e := range d.entries {
			ifd = bo.AppendUint16(ifd, e.Tag)
			ifd = bo.AppendUint16(ifd, e.Type)
			ifd = bo.AppendUint32(ifd, e.Count)
			if len(e.Data) <= 4 {
				v := make([]byte, 4)
				copy(v, e.Data)
				ifd = append(ifd, v...)
			} else {
				ifd = bo.AppendUint32(ifd, dataOff+uint32(data.Len()))
				data.Write(e.Data)
				if data.Len()%2 == 1 {
					data.WriteByte(0)
				}
			}
		}
		ifd = bo.AppendUint32(ifd, 0)
		body.Write(ifd)
		body.Write(data.Bytes())
	}
	ifd0 = bo.AppendUint32(ifd0, 0)
	return bytes.Join([][]byte{hdr.Bytes(), ifd0, body.Bytes()}, nil)
}

// GPSTIFF is a big-endian TIFF block with only a GPS IFD.
func GPSTIFF(lat, lon, accuracyM float64) []byte {
	return TIFF(binary.BigEndian, nil, GPS(binary.BigEndian, lat, lon, accuracyM))
}

// LiveTIFF is a big-endian TIFF block with an Apple MakerNote carrying the
// Live Photo content identifier id and, unless noGPS, a GPS IFD.
func LiveTIFF(id string, lat, lon float64, noGPS bool) []byte {
	mn := lpt.MakerNote(id)
	exif := []Entry{{Tag: 0x927C, Type: 7, Count: uint32(len(mn)), Data: mn}}
	var gps []Entry
	if !noGPS {
		gps = GPS(binary.BigEndian, lat, lon, 0)
	}
	return TIFF(binary.BigEndian, exif, gps)
}

// JPEG is a decodable 8x8 JPEG whose EXIF records the position.
func JPEG(lat, lon, accuracyM float64, seed int) []byte {
	return lpt.JPEGWithTIFF(GPSTIFF(lat, lon, accuracyM), seed)
}

// HEIC is a minimal HEIC (metadata only) whose Exif item records the
// position.
func HEIC(lat, lon, accuracyM float64, seed int) []byte {
	return lpt.HEICWithTIFF(GPSTIFF(lat, lon, accuracyM), seed)
}

// ISO6709 formats a position as QuickTime writes it ("+12.3457+123.4568/"),
// with 4 decimals.
func ISO6709(lat, lon float64) string { return fmt.Sprintf("%+08.4f%+09.4f/", lat, lon) }

// Item is one QuickTime metadata item (moov/meta keys + ilst), UTF-8.
type Item struct{ Key, Value string }

// MOV returns a QuickTime movie (ftyp qt, wide, mdat, moov with mvhd of 3 s
// and meta/keys/ilst holding items).
func MOV(seed int, items ...Item) []byte {
	var keys, ilst [][]byte
	for i, it := range items {
		keys = append(keys, binary.BigEndian.AppendUint32(nil, uint32(8+len(it.Key))), []byte("mdta"), []byte(it.Key))
		idx := string(binary.BigEndian.AppendUint32(nil, uint32(i+1)))
		ilst = append(ilst, lpt.Box(idx, lpt.Box("data", be32(1), be32(0), []byte(it.Value))))
	}
	keysBox := lpt.Box("keys", append([][]byte{{0, 0, 0, 0}, be32(uint32(len(items)))}, keys...)...)
	hdlr := lpt.Box("hdlr", make([]byte, 8), []byte("mdta"), make([]byte, 13))
	meta := lpt.Box("meta", hdlr, keysBox, lpt.Box("ilst", ilst...))
	return movie("qt  ", seed, meta)
}

// MP4XYZ returns an MP4 movie whose moov/udta holds a ©xyz item with the
// ISO 6709 text iso (as Android writes it).
func MP4XYZ(iso string, seed int) []byte {
	xyz := lpt.Box("\xa9xyz", binary.BigEndian.AppendUint16(nil, uint16(len(iso))), []byte{0x15, 0xc7}, []byte(iso))
	return movie("mp42", seed, lpt.Box("udta", xyz))
}

func movie(brand string, seed int, parts ...[]byte) []byte {
	mvhd := lpt.Box("mvhd", []byte{0, 0, 0, 0}, be32(3_800_000_000), be32(3_800_000_000), be32(600), be32(1800), make([]byte, 80))
	mdat := make([]byte, 64)
	for i := range mdat {
		mdat[i] = byte(i + seed*13)
	}
	return bytes.Join([][]byte{
		lpt.Box("ftyp", []byte(brand), be32(0), []byte(brand)),
		lpt.Box("wide"),
		lpt.Box("mdat", mdat, be32(uint32(seed))),
		lpt.Box("moov", append([][]byte{mvhd}, parts...)...),
	}, nil)
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// TIFFIFD0 builds a TIFF block whose IFD0 holds ifd0 (plus a pointer to
// an Exif IFD holding exif, when exif is not nil).
func TIFFIFD0(bo ByteOrder, ifd0, exif []Entry) []byte {
	dirSize := func(n int) int { return 2 + n*12 + 4 }
	// layout: header | IFD0 | IFD0 data | Exif IFD | Exif data
	writeDir := func(base int, es []Entry, extra func(dir []byte) []byte) []byte {
		count := len(es)
		if extra != nil {
			count++
		}
		dataOff := base + dirSize(count)
		dir := bo.AppendUint16(nil, uint16(count))
		var data []byte
		for _, e := range es {
			dir = bo.AppendUint16(dir, e.Tag)
			dir = bo.AppendUint16(dir, e.Type)
			dir = bo.AppendUint32(dir, e.Count)
			if len(e.Data) <= 4 {
				v := make([]byte, 4)
				copy(v, e.Data)
				dir = append(dir, v...)
			} else {
				dir = bo.AppendUint32(dir, uint32(dataOff+len(data)))
				data = append(data, e.Data...)
				if len(data)%2 == 1 {
					data = append(data, 0)
				}
			}
		}
		if extra != nil {
			dir = extra(dir)
		}
		dir = bo.AppendUint32(dir, 0)
		return append(dir, data...)
	}
	// First pass for the size of IFD0 and its data.
	probe := writeDir(8, ifd0, func(d []byte) []byte { return append(d, make([]byte, 12)...) })
	if exif == nil {
		probe = writeDir(8, ifd0, nil)
	}
	exifOff := 8 + len(probe)
	var extra func([]byte) []byte
	if exif != nil {
		extra = func(d []byte) []byte {
			d = bo.AppendUint16(d, 0x8769)
			d = bo.AppendUint16(d, 4)
			d = bo.AppendUint32(d, 1)
			return bo.AppendUint32(d, uint32(exifOff))
		}
	}
	var hdr []byte
	if bo.String() == binary.LittleEndian.String() {
		hdr = []byte("II")
	} else {
		hdr = []byte("MM")
	}
	hdr = bo.AppendUint16(hdr, 42)
	hdr = bo.AppendUint32(hdr, 8)
	out := append(hdr, writeDir(8, ifd0, extra)...)
	if exif != nil {
		out = append(out, writeDir(exifOff, exif, nil)...)
	}
	return out
}

// CameraJPEG is a decodable 8x8 JPEG whose EXIF records an invented
// camera (Make, Model, LensModel, FocalLengthIn35mmFilm) and a pixel size
// of w×h in its Exif IFD (the frame itself is 8×8).
func CameraJPEG(maker, model, lens string, focal uint16, w, h uint32, seed int) []byte {
	bo := binary.BigEndian
	ifd0 := []Entry{ASCII(0x010F, maker), ASCII(0x0110, model)}
	exif := []Entry{
		{Tag: 0xA002, Type: 4, Count: 1, Data: bo.AppendUint32(nil, w)},
		{Tag: 0xA003, Type: 4, Count: 1, Data: bo.AppendUint32(nil, h)},
		{Tag: 0xA405, Type: 3, Count: 1, Data: bo.AppendUint16(nil, focal)},
		ASCII(0xA434, lens),
	}
	return lpt.JPEGWithTIFF(TIFFIFD0(bo, ifd0, exif), seed)
}
