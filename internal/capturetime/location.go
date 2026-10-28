package capturetime

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"strconv"
)

// Location is where a photo or video was taken, as its original recorded
// it: WGS 84 latitude and longitude in degrees, and the horizontal
// accuracy in metres when the original gave one (0: not recorded).
// Altitude, direction and speed are never read.
type Location struct {
	Lat, Lon  float64
	AccuracyM float64
}

// Limits of a recorded location.
const (
	// MaxAccuracyM is the largest horizontal accuracy kept; a larger value
	// (or zero, negative, not finite) is dropped, the position kept.
	MaxAccuracyM = 100_000
	// locationDecimals is the precision kept: 1e-7 degrees, about 1 cm.
	locationDecimals = 1e7
)

// Keys of the QuickTime metadata (moov/meta, keys + ilst) iPhones write.
const (
	iso6709Key  = "com.apple.quicktime.location.ISO6709"
	accuracyKey = "com.apple.quicktime.location.accuracy.horizontal"
)

// ExtractLocation reads the location of the original r of size bytes:
//
//   - JPEG, TIFF, HEIF/HEIC/AVIF: the EXIF GPS directory (GPSLatitude,
//     GPSLongitude with their N/S and E/W references, GPSHPositioningError
//     as the accuracy). A directory marked void (GPSStatus "V") or without
//     references is ignored.
//   - QuickTime/MP4: com.apple.quicktime.location.ISO6709 in moov/meta
//     (with com.apple.quicktime.location.accuracy.horizontal), else the
//     ©xyz item of moov/udta (Android and others), both ISO 6709 text.
//
// It has the limits of Extract (MaxReadBytes, counted reads, boxes and
// entries). It returns nil and a nil error when the original records no
// location it can read with certainty: out of range values, a zero
// denominator, minutes or seconds of 60 or more, and the position 0°, 0°
// (written by devices without a fix) all count as none. The error is
// non-nil only when r itself failed. It only reads.
func ExtractLocation(r io.ReaderAt, size int64) (*Location, error) {
	rd := &reader{r: r, size: size, left: MaxReadBytes}
	l := extractLocation(rd)
	if rd.ioErr != nil {
		return nil, rd.ioErr
	}
	return l, nil
}

func extractLocation(rd *reader) *Location {
	if rd.size < 12 {
		return nil
	}
	head, err := rd.readAt(0, 12)
	if err != nil {
		return nil
	}
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		var l *Location
		jpegExif(rd, func(tiff []byte) bool {
			l = gpsFromTIFF(tiff)
			return l == nil
		})
		return l
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		b, err := rd.readAt(0, min(rd.size, maxBlock))
		if err != nil {
			return nil
		}
		return gpsFromTIFF(b)
	case isTopLevelBox(string(head[4:8])):
		metaBox, moov := topBoxes(rd)
		if metaBox != nil {
			for _, tiff := range heifExif(rd, *metaBox) {
				if l := gpsFromTIFF(tiff); l != nil {
					return l
				}
			}
		}
		if moov != nil {
			return movieLocation(rd, *moov)
		}
	}
	return nil
}

// ---- EXIF GPS ----

const (
	tagGPSIFD         = 0x8825
	tagGPSLatRef      = 0x0001
	tagGPSLat         = 0x0002
	tagGPSLonRef      = 0x0003
	tagGPSLon         = 0x0004
	tagGPSStatus      = 0x0009
	tagGPSHPosError   = 0x001F
	typeRational      = 5
	maxRationalValues = 3
)

func gpsFromTIFF(b []byte) *Location {
	if len(b) < 8 {
		return nil
	}
	var t tiff
	switch string(b[:2]) {
	case "II":
		t.bo = binary.LittleEndian
	case "MM":
		t.bo = binary.BigEndian
	default:
		return nil
	}
	t.b = b
	if t.bo.Uint16(b[2:4]) != 42 {
		return nil
	}
	ifd0, ok := t.ifdTags(t.bo.Uint32(b[4:8]), func(tag uint16) bool { return tag == tagGPSIFD })
	if !ok {
		return nil
	}
	e, ok := ifd0[tagGPSIFD]
	if !ok || e.count != 1 || (e.typ != typeLong && e.typ != 13) {
		return nil
	}
	gps, ok := t.ifdTags(t.bo.Uint32(e.value), func(tag uint16) bool {
		switch tag {
		case tagGPSLatRef, tagGPSLat, tagGPSLonRef, tagGPSLon, tagGPSStatus, tagGPSHPosError:
			return true
		}
		return false
	})
	if !ok {
		return nil
	}
	if st, ok := t.ascii(gps[tagGPSStatus]); ok && st == "V" {
		return nil // measurement void
	}
	latRef, ok1 := t.ascii(gps[tagGPSLatRef])
	lonRef, ok2 := t.ascii(gps[tagGPSLonRef])
	lat, ok3 := t.degrees(gps[tagGPSLat], 90)
	lon, ok4 := t.degrees(gps[tagGPSLon], 180)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil
	}
	switch latRef {
	case "N":
	case "S":
		lat = -lat
	default:
		return nil
	}
	switch lonRef {
	case "E":
	case "W":
		lon = -lon
	default:
		return nil
	}
	l := newLocation(lat, lon)
	if l == nil {
		return nil
	}
	if v, ok := t.rationals(gps[tagGPSHPosError], 1); ok {
		l.AccuracyM = accuracy(v[0])
	}
	return l
}

// rationals reads an entry of n unsigned RATIONAL values.
func (t tiff) rationals(e ifdEntry, n int) ([]float64, bool) {
	if e.typ != typeRational || int(e.count) != n || n > maxRationalValues {
		return nil, false
	}
	o := int64(t.bo.Uint32(e.value)) // 8 bytes per value: never inline
	if o < 8 || o+int64(n)*8 > int64(len(t.b)) {
		return nil, false
	}
	out := make([]float64, n)
	for i := range n {
		p := t.b[o+int64(i)*8:]
		num, den := t.bo.Uint32(p), t.bo.Uint32(p[4:])
		if den == 0 {
			return nil, false
		}
		out[i] = float64(num) / float64(den)
	}
	return out, true
}

// degrees reads GPSLatitude or GPSLongitude: degrees, minutes, seconds.
func (t tiff) degrees(e ifdEntry, limit float64) (float64, bool) {
	v, ok := t.rationals(e, 3)
	if !ok || v[0] > limit || v[1] >= 60 || v[2] >= 60 {
		return 0, false
	}
	d := v[0] + v[1]/60 + v[2]/3600
	return d, d <= limit
}

// ---- QuickTime / MP4 ----

// movieLocation reads moov/meta ISO6709 (and its accuracy), else
// moov/udta ©xyz.
func movieLocation(rd *reader, moov box) *Location {
	var metaBox, udta *box
	rd.boxes(moov.body, moov.end, func(b box) bool {
		switch b.typ {
		case "meta":
			if metaBox == nil {
				metaBox = &b
			}
		case "udta":
			if udta == nil {
				udta = &b
			}
		}
		return true
	})
	if metaBox != nil {
		if v, ok := quickTimeValue(rd, *metaBox, iso6709Key, 64); ok {
			if l := parseISO6709(v); l != nil {
				if a, ok := quickTimeValue(rd, *metaBox, accuracyKey, 32); ok {
					if f, err := strconv.ParseFloat(a, 64); err == nil {
						l.AccuracyM = accuracy(f)
					}
				}
				return l
			}
		}
	}
	if udta != nil {
		return udtaLocation(rd, *udta)
	}
	return nil
}

// udtaLocation reads the ©xyz item of a udta box: a 16-bit text length, a
// 16-bit language code, then the ISO 6709 text.
func udtaLocation(rd *reader, udta box) *Location {
	var l *Location
	rd.boxes(udta.body, udta.end, func(b box) bool {
		if b.typ != "\xa9xyz" {
			return true
		}
		n := b.end - b.body
		if n < 4 || n > 4+64 {
			return false
		}
		d, err := rd.readAt(b.body, n)
		if err != nil {
			return false
		}
		size := int(binary.BigEndian.Uint16(d))
		if size > len(d)-4 {
			return false
		}
		l = parseISO6709(string(d[4 : 4+size]))
		return false
	})
	return l
}

// parseISO6709 parses the ISO 6709 point text of QuickTime files:
// latitude ±DD[MM[SS]][.f], longitude ±DDD[MM[SS]][.f], an optional
// altitude (ignored), an optional "CRS…" designator up to "/", and an
// optional terminating "/". Anything else gives nil.
func parseISO6709(s string) *Location {
	lat, rest, ok := iso6709Part(s, 2, 90)
	if !ok {
		return nil
	}
	lon, rest, ok := iso6709Part(rest, 3, 180)
	if !ok {
		return nil
	}
	if rest != "" && (rest[0] == '+' || rest[0] == '-') { // altitude
		i := 1
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 1 {
			return nil
		}
		rest = rest[i:]
	}
	if len(rest) >= 3 && rest[:3] == "CRS" {
		i := bytes.IndexByte([]byte(rest), '/')
		if i < 0 || rest[3:i] != "WGS_84" && rest[3:i] != "WGS84" {
			return nil // another datum: not converted
		}
		rest = rest[i:]
	}
	if rest != "" && rest != "/" {
		return nil
	}
	return newLocation(lat, lon)
}

// iso6709Part parses one signed coordinate whose degrees have deg digits,
// followed by 0, 2 or 4 digits of minutes and seconds and an optional
// fraction (of the last unit).
func iso6709Part(s string, deg int, limit float64) (float64, string, bool) {
	if len(s) < 1+deg || s[0] != '+' && s[0] != '-' {
		return 0, "", false
	}
	i := 1
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intDigits := s[1:i]
	frac := ""
	if i < len(s) && s[i] == '.' {
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i+1 || j-i-1 > 12 {
			return 0, "", false
		}
		frac, i = s[i+1:j], j
	}
	var units []string
	switch len(intDigits) {
	case deg:
		units = []string{intDigits}
	case deg + 2:
		units = []string{intDigits[:deg], intDigits[deg:]}
	case deg + 4:
		units = []string{intDigits[:deg], intDigits[deg : deg+2], intDigits[deg+2:]}
	default:
		return 0, "", false
	}
	vals := make([]float64, len(units))
	for k, u := range units {
		n, ok := digits(u)
		if !ok {
			return 0, "", false
		}
		vals[k] = float64(n)
	}
	if frac != "" {
		f, err := strconv.ParseFloat("0."+frac, 64)
		if err != nil {
			return 0, "", false
		}
		vals[len(vals)-1] += f
	}
	v := vals[0]
	if len(vals) > 1 {
		if vals[1] >= 60 {
			return 0, "", false
		}
		v += vals[1] / 60
	}
	if len(vals) > 2 {
		if vals[2] >= 60 {
			return 0, "", false
		}
		v += vals[2] / 3600
	}
	if v > limit {
		return 0, "", false
	}
	if s[0] == '-' {
		v = -v
	}
	return v, s[i:], true
}

// ---- common ----

// newLocation validates and rounds a position; nil when it is out of
// range, not finite, or 0°, 0°.
func newLocation(lat, lon float64) *Location {
	lat, lon = round7(lat), round7(lon)
	if !ValidPosition(lat, lon) {
		return nil
	}
	return &Location{Lat: lat, Lon: lon}
}

// ValidPosition reports whether lat, lon is a position Location may hold
// (also for values read back from storage).
func ValidPosition(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return false
	}
	return lat != 0 || lon != 0
}

func round7(v float64) float64 {
	r := math.Round(v*locationDecimals) / locationDecimals
	if r == 0 {
		return 0 // no negative zero
	}
	return r
}

// accuracy keeps a horizontal accuracy in (0, MaxAccuracyM] metres,
// rounded to centimetres; anything else is 0 (not recorded).
func accuracy(m float64) float64 {
	if math.IsNaN(m) || m <= 0 || m > MaxAccuracyM {
		return 0
	}
	r := math.Round(m*100) / 100
	if r == 0 {
		return 0
	}
	return r
}
