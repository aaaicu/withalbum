package capturetime

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Camera is the device a photo or video was taken with, as the original
// recorded it. Empty fields (0 for Focal35mm) were not recorded.
type Camera struct {
	Make, Model, Lens string
	// Focal35mm is the focal length in 35 mm film terms, whole millimetres
	// (EXIF FocalLengthIn35mmFilm).
	Focal35mm int
}

// Empty reports whether c records nothing.
func (c Camera) Empty() bool {
	return c.Make == "" && c.Model == "" && c.Lens == "" && c.Focal35mm == 0
}

// MediaInfo is the camera and the pixel size of an original. Width and
// Height are the size as displayed, after the recorded orientation (EXIF
// Orientation 5..8, HEIF irot 90/270, a track matrix turning by 90°
// swap them); 0 when unknown. Camera is nil when none is recorded.
type MediaInfo struct {
	Camera        *Camera
	Width, Height int
}

// Limits of what ExtractMediaInfo reports.
const (
	// MaxDimension is the largest width or height believed (a crafted
	// header claiming more is unknown).
	MaxDimension = 1 << 17
	// MaxFocal35mm is the largest focal length believed, in millimetres.
	MaxFocal35mm = 10000
	// maxCameraText bounds a make, model or lens, in bytes.
	maxCameraText = 64
)

// EXIF / TIFF tags read for the camera and the pixel size.
const (
	tagImageWidth  = 0x0100
	tagImageLength = 0x0101
	tagMake        = 0x010F
	tagModel       = 0x0110
	tagOrientation = 0x0112
	tagPixelX      = 0xA002
	tagPixelY      = 0xA003
	tagFocal35     = 0xA405
	tagLensModel   = 0xA434
	typeShort      = 3
	qtMakeKey      = "com.apple.quicktime.make"
	qtModelKey     = "com.apple.quicktime.model"
	maxTracks      = 16
	tkhdV0Size     = 84
	tkhdV1Size     = 96
	fixed16One     = 0x10000
	fixed16NegOne  = 0xFFFF0000
)

// ExtractMediaInfo reads the camera (EXIF Make, Model, LensModel,
// FocalLengthIn35mmFilm; QuickTime com.apple.quicktime.make/model) and
// the pixel size of the original r of size bytes:
//
//   - JPEG: the frame header (SOF) before the image data, else EXIF
//     PixelXDimension/PixelYDimension; turned by EXIF Orientation.
//   - HEIF/HEIC/AVIF: the ispe property of the primary item (pitm, ipma),
//     turned by its irot; else EXIF PixelX/YDimension.
//   - TIFF: ImageWidth/ImageLength of IFD0, else EXIF PixelX/YDimension.
//   - QuickTime/MP4: tkhd width and height of the first track that has
//     them (the video track), turned when its matrix rotates by 90°.
//
// It has the limits of Extract (MaxReadBytes, counted reads, boxes,
// segments and entries) and never reads image or movie data. Texts that
// are not printable UTF-8 or longer than 64 bytes are dropped, sizes
// outside 1..MaxDimension are unknown. The error is non-nil only when r
// itself failed. It only reads.
func ExtractMediaInfo(r io.ReaderAt, size int64) (MediaInfo, error) {
	rd := &reader{r: r, size: size, left: MaxReadBytes}
	m := extractMediaInfo(rd)
	if rd.ioErr != nil {
		return MediaInfo{}, rd.ioErr
	}
	return m, nil
}

func extractMediaInfo(rd *reader) MediaInfo {
	if rd.size < 12 {
		return MediaInfo{}
	}
	head, err := rd.readAt(0, 12)
	if err != nil {
		return MediaInfo{}
	}
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		return jpegMediaInfo(rd)
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		b, err := rd.readAt(0, min(rd.size, maxBlock))
		if err != nil {
			return MediaInfo{}
		}
		e := exifMedia(b, true)
		return e.info(0, 0, false)
	case isTopLevelBox(string(head[4:8])):
		metaBox, moov := topBoxes(rd)
		if metaBox != nil {
			var e exifFacts
			for _, tiff := range heifExif(rd, *metaBox) {
				if e = exifMedia(tiff, false); e.ok {
					break
				}
			}
			w, h, turned := heifSize(rd, *metaBox)
			if w > 0 || e.ok {
				// The ispe size is as stored; irot says how it is shown. EXIF
				// Orientation is not applied on top of it (HEIF writers mirror
				// irot into EXIF).
				if w > 0 {
					e.orientation = 1
				}
				return e.info(w, h, turned)
			}
		}
		if moov != nil {
			return movieMediaInfo(rd, *moov)
		}
	}
	return MediaInfo{}
}

// exifFacts is what one TIFF/EXIF block says.
type exifFacts struct {
	ok          bool
	camera      Camera
	pixelW      int
	pixelH      int
	orientation int
}

// info combines a frame size (w×h as stored, turned when the container
// rotates by 90°) with the EXIF facts.
func (e exifFacts) info(w, h int, turned bool) MediaInfo {
	var m MediaInfo
	if !e.camera.Empty() {
		c := e.camera
		m.Camera = &c
	}
	if w == 0 || h == 0 {
		w, h = e.pixelW, e.pixelH
	}
	if !validDim(w) || !validDim(h) {
		return m
	}
	if e.orientation >= 5 && e.orientation <= 8 {
		turned = !turned
	}
	if turned {
		w, h = h, w
	}
	m.Width, m.Height = w, h
	return m
}

func validDim(n int) bool { return n > 0 && n <= MaxDimension }

// exifMedia reads the camera and size tags of a TIFF block. tiffFile:
// IFD0 ImageWidth/ImageLength are the image's own size (a TIFF file, not
// an EXIF block, where IFD0 describes the thumbnail or nothing).
func exifMedia(b []byte, tiffFile bool) exifFacts {
	var e exifFacts
	if len(b) < 8 {
		return e
	}
	var t tiff
	switch string(b[:2]) {
	case "II":
		t.bo = binary.LittleEndian
	case "MM":
		t.bo = binary.BigEndian
	default:
		return e
	}
	t.b = b
	if t.bo.Uint16(b[2:4]) != 42 {
		return e
	}
	ifd0, ok := t.ifdTags(t.bo.Uint32(b[4:8]), func(tag uint16) bool {
		switch tag {
		case tagMake, tagModel, tagOrientation, tagExifIFD, tagImageWidth, tagImageLength:
			return true
		}
		return false
	})
	if !ok {
		return e
	}
	e.ok = true
	e.camera.Make = t.text(ifd0[tagMake])
	e.camera.Model = t.text(ifd0[tagModel])
	if v, ok := t.short(ifd0[tagOrientation]); ok && v >= 1 && v <= 8 {
		e.orientation = v
	}
	if tiffFile {
		e.pixelW, _ = t.dim(ifd0[tagImageWidth])
		e.pixelH, _ = t.dim(ifd0[tagImageLength])
	}
	if x, ok := ifd0[tagExifIFD]; ok && x.count == 1 && (x.typ == typeLong || x.typ == 13) {
		exif, ok := t.ifdTags(t.bo.Uint32(x.value), func(tag uint16) bool {
			switch tag {
			case tagPixelX, tagPixelY, tagFocal35, tagLensModel:
				return true
			}
			return false
		})
		if ok {
			e.camera.Lens = t.text(exif[tagLensModel])
			if f, ok := t.short(exif[tagFocal35]); ok && f > 0 && f <= MaxFocal35mm {
				e.camera.Focal35mm = f
			}
			if e.pixelW == 0 || e.pixelH == 0 {
				w, okW := t.dim(exif[tagPixelX])
				h, okH := t.dim(exif[tagPixelY])
				if okW && okH {
					e.pixelW, e.pixelH = w, h
				}
			}
		}
	}
	return e
}

// short reads one SHORT value.
func (t tiff) short(e ifdEntry) (int, bool) {
	if e.typ != typeShort || e.count != 1 {
		return 0, false
	}
	return int(t.bo.Uint16(e.value)), true
}

// dim reads one SHORT or LONG pixel count.
func (t tiff) dim(e ifdEntry) (int, bool) {
	var v int
	switch {
	case e.typ == typeShort && e.count == 1:
		v = int(t.bo.Uint16(e.value))
	case e.typ == typeLong && e.count == 1:
		u := t.bo.Uint32(e.value)
		if u > MaxDimension {
			return 0, false
		}
		v = int(u)
	default:
		return 0, false
	}
	if !validDim(v) {
		return 0, false
	}
	return v, true
}

// text reads an ASCII (or UTF-8) camera text: trimmed, at most
// maxCameraText bytes, printable; "" otherwise.
func (t tiff) text(e ifdEntry) string {
	s, ok := t.ascii(e)
	if !ok {
		return ""
	}
	return cleanText(s)
}

// cleanText trims s and returns it when it is printable UTF-8 of at most
// maxCameraText bytes, else "".
func cleanText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxCameraText || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return ""
		}
	}
	return s
}

// ---- JPEG ----

// jpegMediaInfo walks the segments before the image data: EXIF APP1 for
// the camera and orientation, the first SOF for the frame size.
func jpegMediaInfo(rd *reader) MediaInfo {
	var e exifFacts
	var w, h int
	off := int64(2)
	for range maxSegments {
		hd, err := rd.readAt(off, 4)
		if err != nil || hd[0] != 0xFF {
			break
		}
		marker := hd[1]
		switch {
		case marker == 0xFF:
			off++
			continue
		case marker == 0xD8, marker == 0x01, marker >= 0xD0 && marker <= 0xD7:
			off += 2
			continue
		case marker == 0xDA, marker == 0xD9:
			return e.info(w, h, false)
		}
		n := int64(binary.BigEndian.Uint16(hd[2:4]))
		if n < 2 {
			break
		}
		switch {
		case marker == 0xE1 && n >= 8 && !e.ok:
			seg, err := rd.readAt(off+4, n-2)
			if err != nil {
				return e.info(w, h, false)
			}
			if bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
				e = exifMedia(seg[6:], false)
			}
		case isSOF(marker) && w == 0 && n >= 7:
			f, err := rd.readAt(off+4, 5) // precision, height u16, width u16
			if err != nil {
				return e.info(w, h, false)
			}
			hh, ww := int(binary.BigEndian.Uint16(f[1:3])), int(binary.BigEndian.Uint16(f[3:5]))
			if validDim(ww) && validDim(hh) {
				w, h = ww, hh
			}
		}
		off += 2 + n
	}
	return e.info(w, h, false)
}

// isSOF reports the start-of-frame markers (not DHT C4, JPG C8, DAC CC).
func isSOF(m byte) bool {
	return m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC
}

// ---- HEIF ----

// heifSize returns the ispe size of the primary item and whether its irot
// turns it by 90° or 270°. 0, 0 when it cannot be told with certainty.
func heifSize(rd *reader, m box) (int, int, bool) {
	if m.body+4 > m.end {
		return 0, 0, false
	}
	var pitm, iprp *box
	rd.boxes(m.body+4, m.end, func(b box) bool {
		switch b.typ {
		case "pitm":
			if pitm == nil {
				pitm = &b
			}
		case "iprp":
			if iprp == nil {
				iprp = &b
			}
		}
		return true
	})
	if pitm == nil || iprp == nil {
		return 0, 0, false
	}
	pb, err := rd.readAt(pitm.body, min(pitm.end-pitm.body, 8))
	if err != nil || len(pb) < 6 {
		return 0, 0, false
	}
	var primary uint32
	if pb[0] == 0 {
		primary = uint32(binary.BigEndian.Uint16(pb[4:6]))
	} else {
		if len(pb) < 8 {
			return 0, 0, false
		}
		primary = binary.BigEndian.Uint32(pb[4:8])
	}
	var ipco, ipma *box
	rd.boxes(iprp.body, iprp.end, func(b box) bool {
		switch b.typ {
		case "ipco":
			if ipco == nil {
				ipco = &b
			}
		case "ipma":
			if ipma == nil {
				ipma = &b
			}
		}
		return true
	})
	if ipco == nil || ipma == nil {
		return 0, 0, false
	}
	props, ok := itemProperties(rd, *ipma, primary)
	if !ok {
		return 0, 0, false
	}
	// Property boxes of ipco, 1-based.
	var w, h int
	turned := false
	idx := 0
	rd.boxes(ipco.body, ipco.end, func(b box) bool {
		idx++
		if idx > maxItems {
			return false
		}
		if !props[idx] {
			return true
		}
		switch b.typ {
		case "ispe": // FullBox, u32 width, u32 height
			if w != 0 {
				return true
			}
			d, err := rd.readAt(b.body, min(b.end-b.body, 12))
			if err != nil || len(d) < 12 {
				return true
			}
			ww, hh := binary.BigEndian.Uint32(d[4:8]), binary.BigEndian.Uint32(d[8:12])
			if ww > 0 && ww <= MaxDimension && hh > 0 && hh <= MaxDimension {
				w, h = int(ww), int(hh)
			}
		case "irot": // u8: angle in units of 90° anticlockwise (low 2 bits)
			d, err := rd.readAt(b.body, min(b.end-b.body, 1))
			if err == nil && len(d) == 1 {
				turned = d[0]&3 == 1 || d[0]&3 == 3
			}
		}
		return true
	})
	return w, h, turned
}

// itemProperties returns the 1-based ipco indexes associated with item id
// in an ipma box.
func itemProperties(rd *reader, ipma box, id uint32) (map[int]bool, bool) {
	b, err := rd.readAt(ipma.body, min(ipma.end-ipma.body, maxBlock))
	if err != nil || len(b) < 8 {
		return nil, false
	}
	p := &cursor{b: b}
	version := p.u8()
	flags := p.take(3)
	if p.bad {
		return nil, false
	}
	large := flags[2]&1 == 1
	count := p.u32()
	for i := uint32(0); i < count && i < maxItems && !p.bad; i++ {
		var itemID uint32
		if version < 1 {
			itemID = uint32(p.u16())
		} else {
			itemID = p.u32()
		}
		n := int(p.u8())
		var out map[int]bool
		if itemID == id {
			out = make(map[int]bool, n)
		}
		for j := 0; j < n && !p.bad; j++ {
			var idx int
			if large {
				idx = int(p.u16() & 0x7FFF)
			} else {
				idx = int(p.u8() & 0x7F)
			}
			if out != nil && idx > 0 {
				out[idx] = true
			}
		}
		if out != nil {
			return out, !p.bad
		}
	}
	return nil, false
}

// ---- QuickTime / MP4 ----

// movieMediaInfo reads the camera from moov/meta and the size from the
// first track header with one.
func movieMediaInfo(rd *reader, moov box) MediaInfo {
	var m MediaInfo
	var metaBox *box
	var traks []box
	rd.boxes(moov.body, moov.end, func(b box) bool {
		switch b.typ {
		case "meta":
			if metaBox == nil {
				metaBox = &b
			}
		case "trak":
			if len(traks) < maxTracks {
				traks = append(traks, b)
			}
		}
		return true
	})
	if metaBox != nil {
		var c Camera
		if v, ok := quickTimeValue(rd, *metaBox, qtMakeKey, maxCameraText); ok {
			c.Make = cleanText(v)
		}
		if v, ok := quickTimeValue(rd, *metaBox, qtModelKey, maxCameraText); ok {
			c.Model = cleanText(v)
		}
		if !c.Empty() {
			m.Camera = &c
		}
	}
	for _, tr := range traks {
		w, h, ok := trackSize(rd, tr)
		if ok {
			m.Width, m.Height = w, h
			break
		}
	}
	return m
}

// trackSize reads the display size of a track header: width and height
// (16.16 fixed point), swapped when the matrix turns by 90° or 270°.
func trackSize(rd *reader, trak box) (int, int, bool) {
	var tkhd *box
	rd.boxes(trak.body, trak.end, func(b box) bool {
		if b.typ == "tkhd" {
			tkhd = &b
			return false
		}
		return true
	})
	if tkhd == nil {
		return 0, 0, false
	}
	d, err := rd.readAt(tkhd.body, min(tkhd.end-tkhd.body, tkhdV1Size))
	if err != nil || len(d) < 4 {
		return 0, 0, false
	}
	var mat int
	switch d[0] {
	case 0:
		if len(d) < tkhdV0Size {
			return 0, 0, false
		}
		mat = 40
	case 1:
		if len(d) < tkhdV1Size {
			return 0, 0, false
		}
		mat = 52
	default:
		return 0, 0, false
	}
	be := binary.BigEndian
	a, bb, c, dd := be.Uint32(d[mat:]), be.Uint32(d[mat+4:]), be.Uint32(d[mat+12:]), be.Uint32(d[mat+16:])
	w, h := int(be.Uint32(d[mat+36:])>>16), int(be.Uint32(d[mat+40:])>>16)
	if !validDim(w) || !validDim(h) {
		return 0, 0, false
	}
	isUnit := func(v uint32) bool { return v == fixed16One || v == fixed16NegOne }
	switch {
	case a == 0 && dd == 0 && isUnit(bb) && isUnit(c):
		w, h = h, w // 90° or 270°
	default:
		// 0°, 180°, or not a plain rotation (mirror, scale): as stored.
	}
	return w, h, true
}
