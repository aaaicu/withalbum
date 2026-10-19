// Package livephototest builds small, hand-made Live Photo originals for
// tests: a JPEG or HEIC still whose EXIF MakerNote carries an Apple content
// identifier, and a QuickTime movie whose metadata carries one. They hold
// only the structures the server reads; no real photo is used. Import it
// only from tests.
package livephototest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
)

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// Box returns an ISO BMFF box of type typ with the given contents.
func Box(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	return append(append(be32(uint32(8+len(body))), typ...), body...)
}

// MakerNote returns an Apple MakerNote ("Apple iOS\0", version 1, "MM")
// whose directory holds the content identifier id (tag 0x0011), or only
// another tag when id is "".
func MakerNote(id string) []byte {
	var b bytes.Buffer
	b.WriteString("Apple iOS\x00")
	b.Write(be16(1))
	b.WriteString("MM")
	tag, val := uint16(0x0011), append([]byte(id), 0)
	if id == "" {
		tag, val = 0x0008, []byte("other tag\x00")
	}
	// Directory at 14: one entry, next 0; the value follows it.
	b.Write(be16(1))
	b.Write(be16(tag))
	b.Write(be16(2)) // ASCII
	b.Write(be32(uint32(len(val))))
	b.Write(be32(14 + 2 + 12 + 4))
	b.Write(be32(0))
	b.Write(val)
	return b.Bytes()
}

// TIFF returns a big-endian TIFF block: IFD0 -> Exif IFD -> MakerNote mn.
// A nil mn gives an Exif IFD without MakerNote.
func TIFF(mn []byte) []byte {
	var b bytes.Buffer
	b.WriteString("MM")
	b.Write(be16(42))
	b.Write(be32(8))
	// IFD0 at 8: the Exif IFD pointer.
	b.Write(be16(1))
	b.Write(be16(0x8769))
	b.Write(be16(4)) // LONG
	b.Write(be32(1))
	b.Write(be32(26))
	b.Write(be32(0))
	// Exif IFD at 26: the MakerNote (UNDEFINED) or an unrelated tag.
	b.Write(be16(1))
	if mn == nil {
		b.Write(be16(0xA001)) // ColorSpace, SHORT 1
		b.Write(be16(3))
		b.Write(be32(1))
		b.Write([]byte{0, 1, 0, 0})
	} else {
		b.Write(be16(0x927C))
		b.Write(be16(7))
		b.Write(be32(uint32(len(mn))))
		b.Write(be32(44))
	}
	b.Write(be32(0))
	b.Write(mn)
	return b.Bytes()
}

// exifTIFF is the TIFF block of a still with identifier id ("": a
// MakerNote without one).
func exifTIFF(id string) []byte { return TIFF(MakerNote(id)) }

// JPEGWithTIFF returns a decodable 8x8 JPEG with an EXIF APP1 segment
// holding tiff, right after SOI. A seed varies the pixels (and so the
// bytes).
func JPEGWithTIFF(tiff []byte, seed int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = byte(i*7 + seed*31)
	}
	img.Set(0, 0, color.RGBA{byte(seed), byte(seed >> 8), 3, 255})
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, nil); err != nil {
		panic(err)
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := append([]byte{0xFF, 0xE1}, be16(uint16(len(payload)+2))...)
	seg = append(seg, payload...)
	return bytes.Join([][]byte{enc.Bytes()[:2], seg, enc.Bytes()[2:]}, nil)
}

// JPEG returns a Live Photo still in JPEG with content identifier id
// ("": none).
func JPEG(id string, seed int) []byte { return JPEGWithTIFF(exifTIFF(id), seed) }

// HEICWithTIFF returns a minimal HEIC file (ftyp, meta with hdlr, iinf,
// iloc, and mdat) whose Exif item holds tiff. It has no image data; the
// server only reads its metadata (and makes no built-in preview of HEIC).
func HEICWithTIFF(tiff []byte, seed int) []byte {
	ftyp := Box("ftyp", []byte("heic"), be32(0), []byte("mif1"), []byte("heic"))
	item := append(be32(6), append([]byte("Exif\x00\x00"), tiff...)...)
	meta := func(offset uint32) []byte {
		hdlr := Box("hdlr", make([]byte, 8), []byte("pict"), make([]byte, 13))
		infe := Box("infe", []byte{2, 0, 0, 0}, be16(1), be16(0), []byte("Exif"), []byte{0})
		iinf := Box("iinf", []byte{0, 0, 0, 0}, be16(1), infe)
		iloc := Box("iloc", []byte{0, 0, 0, 0}, []byte{0x44, 0x00}, be16(1),
			be16(1), be16(0), be16(1), be32(offset), be32(uint32(len(item))))
		return Box("meta", []byte{0, 0, 0, 0}, hdlr, iinf, iloc)
	}
	offset := uint32(len(ftyp) + len(meta(0)) + 8)
	mdat := Box("mdat", item, []byte{byte(seed), byte(seed >> 8), byte(seed >> 16)})
	return bytes.Join([][]byte{ftyp, meta(offset), mdat}, nil)
}

// HEIC returns a Live Photo still in HEIC with content identifier id
// ("": none).
func HEIC(id string, seed int) []byte { return HEICWithTIFF(exifTIFF(id), seed) }

// MOV returns a QuickTime movie (ftyp qt, wide, mdat, moov with mvhd and
// meta/keys/ilst) whose metadata holds the content identifier id ("":
// only another key).
func MOV(id string, seed int) []byte {
	key := "com.apple.quicktime.content.identifier"
	if id == "" {
		key, id = "com.apple.quicktime.make", "Apple"
	}
	keys := Box("keys", []byte{0, 0, 0, 0}, be32(2),
		be32(uint32(8+len("com.apple.quicktime.model"))), []byte("mdta"), []byte("com.apple.quicktime.model"),
		be32(uint32(8+len(key))), []byte("mdta"), []byte(key))
	ilst := Box("ilst",
		Box(string(be32(1)), Box("data", be32(1), be32(0), []byte("iPhone"))),
		Box(string(be32(2)), Box("data", be32(1), be32(0), []byte(id))))
	hdlr := Box("hdlr", make([]byte, 8), []byte("mdta"), make([]byte, 13))
	mvhd := Box("mvhd", []byte{0, 0, 0, 0}, be32(3_800_000_000), be32(3_800_000_000), be32(600), be32(1800), make([]byte, 80))
	mdat := make([]byte, 64)
	for i := range mdat {
		mdat[i] = byte(i + seed*13)
	}
	return bytes.Join([][]byte{
		Box("ftyp", []byte("qt  "), be32(0), []byte("qt  ")),
		Box("wide"),
		Box("mdat", mdat, be32(uint32(seed))),
		Box("moov", mvhd, Box("meta", hdlr, keys, ilst)),
	}, nil)
}
