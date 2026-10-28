package capturetime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	geo "github.com/aaaicu/withalbum/internal/capturetime/geotest"
	lpt "github.com/aaaicu/withalbum/internal/livephoto/livephototest"
)

// Camera names in these tests are invented ("Testmaker", "TM-1"); no real
// device file is used.

func mediaTIFF(bo geo.ByteOrder, ifd0, exif []geo.Entry) []byte { return geo.TIFFIFD0(bo, ifd0, exif) }

func short(bo geo.ByteOrder, tag, v uint16) geo.Entry {
	return geo.Entry{Tag: tag, Type: 3, Count: 1, Data: bo.AppendUint16(nil, v)}
}

func long(bo geo.ByteOrder, tag uint16, v uint32) geo.Entry {
	return geo.Entry{Tag: tag, Type: 4, Count: 1, Data: bo.AppendUint32(nil, v)}
}

func cameraTIFF(bo geo.ByteOrder, orientation uint16) []byte {
	ifd0 := []geo.Entry{geo.ASCII(tagMake, "Testmaker"), geo.ASCII(tagModel, "TM-1 Pro")}
	if orientation > 0 {
		ifd0 = append(ifd0, short(bo, tagOrientation, orientation))
	}
	return mediaTIFF(bo, ifd0, []geo.Entry{
		long(bo, tagPixelX, 4032), long(bo, tagPixelY, 3024),
		short(bo, tagFocal35, 24),
		geo.ASCII(tagLensModel, "TM-1 Pro back camera 6.8mm f/1.8"),
	})
}

func mediaOf(t *testing.T, b []byte) MediaInfo {
	t.Helper()
	m, err := ExtractMediaInfo(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("ExtractMediaInfo: %v", err)
	}
	return m
}

// heicWithSize: a HEIC whose primary item (pitm) has ispe w×h (and irot
// rot when rot >= 0), next to a decoy tile ispe of another item, and an
// Exif item holding tiff (nil: none).
func heicWithSize(w, h uint32, rot int, tiff []byte) []byte {
	box := lpt.Box
	be := binary.BigEndian
	u16 := func(v uint16) []byte { return be.AppendUint16(nil, v) }
	u32 := func(v uint32) []byte { return be.AppendUint32(nil, v) }
	ispe := func(w, h uint32) []byte { return box("ispe", []byte{0, 0, 0, 0}, u32(w), u32(h)) }
	props := [][]byte{ispe(512, 512), ispe(w, h)} // 1: tile, 2: primary
	assoc := []byte{2, 0x82}                      // item 1 (primary): 2 props: 2 (essential)
	if rot >= 0 {
		props = append(props, box("irot", []byte{byte(rot)}))
		assoc = []byte{2, 0x82, 0x03}
		assoc[0] = 2
	}
	ipco := box("ipco", props...)
	ipmaBody := [][]byte{{0, 0, 0, 0}, u32(2),
		u16(1), {byte(len(assoc) - 1)}, assoc[1:],
		u16(2), {1}, {0x81}, // tile item 2: prop 1
	}
	ipma := box("ipma", ipmaBody...)
	iprp := box("iprp", ipco, ipma)
	pitm := box("pitm", []byte{0, 0, 0, 0}, u16(1))
	hdlr := box("hdlr", make([]byte, 8), []byte("pict"), make([]byte, 13))
	ftyp := box("ftyp", []byte("heic"), u32(0), []byte("mif1"), []byte("heic"))
	if tiff == nil {
		meta := box("meta", []byte{0, 0, 0, 0}, hdlr, pitm, iprp)
		return bytes.Join([][]byte{ftyp, meta, box("mdat", []byte{1, 2, 3})}, nil)
	}
	item := append(u32(6), append([]byte("Exif\x00\x00"), tiff...)...)
	meta := func(offset uint32) []byte {
		infe := box("infe", []byte{2, 0, 0, 0}, u16(3), u16(0), []byte("Exif"), []byte{0})
		iinf := box("iinf", []byte{0, 0, 0, 0}, u16(1), infe)
		iloc := box("iloc", []byte{0, 0, 0, 0}, []byte{0x44, 0x00}, u16(1),
			u16(3), u16(0), u16(1), u32(offset), u32(uint32(len(item))))
		return box("meta", []byte{0, 0, 0, 0}, hdlr, pitm, iinf, iloc, iprp)
	}
	offset := uint32(len(ftyp) + len(meta(0)) + 8)
	return bytes.Join([][]byte{ftyp, meta(offset), box("mdat", item)}, nil)
}

// tkhd builds a track header (version 0 or 1) of w×h with the matrix
// a, b, c, d (16.16).
func tkhd(version byte, w, h uint32, a, b, c, d uint32) []byte {
	be := binary.BigEndian
	body := []byte{version, 0, 0, 7}
	if version == 0 {
		body = append(body, make([]byte, 20)...) // creation, modification, track_ID, reserved, duration
	} else {
		body = append(body, make([]byte, 32)...)
	}
	body = append(body, make([]byte, 16)...) // reserved 8, layer, group, volume, reserved
	for _, v := range []uint32{a, b, 0, c, d, 0, 0, 0, 0x40000000} {
		body = be.AppendUint32(body, v)
	}
	body = be.AppendUint32(body, w<<16)
	body = be.AppendUint32(body, h<<16)
	return lpt.Box("tkhd", body)
}

func movWithTracks(meta []byte, traks ...[]byte) []byte {
	box := lpt.Box
	mvhd := box("mvhd", []byte{0, 0, 0, 0}, make([]byte, 8), binary.BigEndian.AppendUint32(nil, 600), binary.BigEndian.AppendUint32(nil, 1800), make([]byte, 80))
	parts := [][]byte{mvhd}
	if meta != nil {
		parts = append(parts, meta)
	}
	for _, t := range traks {
		parts = append(parts, box("trak", t))
	}
	return bytes.Join([][]byte{
		box("ftyp", []byte("qt  "), make([]byte, 4), []byte("qt  ")),
		box("mdat", make([]byte, 32)),
		box("moov", parts...),
	}, nil)
}

func TestExtractMediaInfo(t *testing.T) {
	be, le := binary.BigEndian, binary.LittleEndian
	const one, neg = 0x10000, 0xFFFF0000
	full := &Camera{Make: "Testmaker", Model: "TM-1 Pro", Lens: "TM-1 Pro back camera 6.8mm f/1.8", Focal35mm: 24}
	qt := geo.MOV(1, geo.Item{Key: qtMakeKey, Value: "Testmaker"}, geo.Item{Key: qtModelKey, Value: "TM-1 Pro"})
	// geo.MOV has no track; add one through movWithTracks for size cases.
	qtMeta := func() []byte {
		keys := [][]byte{}
		ilst := [][]byte{}
		for i, it := range []geo.Item{{Key: qtMakeKey, Value: "Testmaker"}, {Key: qtModelKey, Value: "TM-1 Pro"}} {
			keys = append(keys, binary.BigEndian.AppendUint32(nil, uint32(8+len(it.Key))), []byte("mdta"), []byte(it.Key))
			ilst = append(ilst, lpt.Box(string(binary.BigEndian.AppendUint32(nil, uint32(i+1))), lpt.Box("data", binary.BigEndian.AppendUint32(nil, 1), make([]byte, 4), []byte(it.Value))))
		}
		return lpt.Box("meta", lpt.Box("hdlr", make([]byte, 8), []byte("mdta"), make([]byte, 13)),
			lpt.Box("keys", append([][]byte{{0, 0, 0, 0}, binary.BigEndian.AppendUint32(nil, 2)}, keys...)...),
			lpt.Box("ilst", ilst...))
	}()
	audio := tkhd(0, 0, 0, one, 0, 0, one)
	for _, tc := range []struct {
		name string
		b    []byte
		cam  *Camera
		w, h int
	}{
		// The encoded 8×8 frame (SOF) wins over EXIF PixelX/YDimension.
		{"jpeg sof", lpt.JPEGWithTIFF(cameraTIFF(be, 1), 1), full, 8, 8},
		{"jpeg little endian", lpt.JPEGWithTIFF(cameraTIFF(le, 0), 2), full, 8, 8},
		{"jpeg without exif", jpegWithoutExif(), nil, 0, 0},
		{"tiff exif pixel size turned", cameraTIFF(be, 6), full, 3024, 4032},
		{"tiff image size", mediaTIFF(be, []geo.Entry{long(be, tagImageWidth, 640), short(be, tagImageLength, 480)}, nil), nil, 640, 480},
		{"heic ispe of primary", heicWithSize(4032, 3024, -1, cameraTIFF(be, 1)), full, 4032, 3024},
		{"heic irot 90", heicWithSize(4032, 3024, 1, nil), nil, 3024, 4032},
		{"heic irot 180", heicWithSize(4032, 3024, 2, nil), nil, 4032, 3024},
		// EXIF orientation is not applied on top of ispe (irot says it).
		{"heic exif orientation ignored", heicWithSize(4032, 3024, -1, cameraTIFF(be, 6)), full, 4032, 3024},
		{"heic exif only", lpt.HEICWithTIFF(cameraTIFF(be, 6), 3), full, 3024, 4032},
		{"mov camera only", qt, &Camera{Make: "Testmaker", Model: "TM-1 Pro"}, 0, 0},
		{"mov tkhd v0", movWithTracks(qtMeta, audio, tkhd(0, 1920, 1080, one, 0, 0, one)), &Camera{Make: "Testmaker", Model: "TM-1 Pro"}, 1920, 1080},
		{"mov tkhd rotated 90", movWithTracks(nil, tkhd(0, 1920, 1080, 0, one, neg, 0)), nil, 1080, 1920},
		{"mov tkhd rotated 270 v1", movWithTracks(nil, tkhd(1, 3840, 2160, 0, neg, one, 0)), nil, 2160, 3840},
		{"mov tkhd 180", movWithTracks(nil, tkhd(0, 1280, 720, neg, 0, 0, neg)), nil, 1280, 720},
		{"mov no video track", movWithTracks(nil, audio), nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mediaOf(t, tc.b)
			if (m.Camera == nil) != (tc.cam == nil) || m.Camera != nil && *m.Camera != *tc.cam {
				t.Fatalf("camera %+v, want %+v", m.Camera, tc.cam)
			}
			if m.Width != tc.w || m.Height != tc.h {
				t.Fatalf("size %d×%d, want %d×%d", m.Width, m.Height, tc.w, tc.h)
			}
		})
	}
}

func TestExtractMediaInfoRejects(t *testing.T) {
	be := binary.BigEndian
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"control characters", mediaTIFF(be, []geo.Entry{geo.ASCII(tagMake, "Bad\x07Make")}, nil)},
		{"invalid utf-8", mediaTIFF(be, []geo.Entry{geo.ASCII(tagModel, "\xff\xfe")}, nil)},
		{"blank", mediaTIFF(be, []geo.Entry{geo.ASCII(tagModel, "   ")}, nil)},
		{"too long", mediaTIFF(be, []geo.Entry{geo.ASCII(tagModel, string(bytes.Repeat([]byte("x"), 70)))}, nil)},
		{"duplicate make", mediaTIFF(be, []geo.Entry{geo.ASCII(tagMake, "A"), geo.ASCII(tagMake, "B")}, nil)},
		{"zero focal", mediaTIFF(be, nil, []geo.Entry{short(be, tagFocal35, 0)})},
		{"huge size", mediaTIFF(be, nil, []geo.Entry{long(be, tagPixelX, 1<<20), long(be, tagPixelY, 10)})},
		{"zero size", mediaTIFF(be, nil, []geo.Entry{long(be, tagPixelX, 0), long(be, tagPixelY, 10)})},
		{"heic huge ispe", heicWithSize(1<<20, 10, -1, nil)},
		{"mov zero size", movWithTracks(nil, tkhd(0, 0, 1080, 0x10000, 0, 0, 0x10000))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mediaOf(t, tc.b)
			if m.Camera != nil || m.Width != 0 || m.Height != 0 {
				t.Fatalf("got %+v %+v", m, m.Camera)
			}
		})
	}
	// Garbage and short inputs.
	for _, b := range [][]byte{nil, {0xFF, 0xD8}, []byte("not a photo at all"), lpt.Box("moov")} {
		if m := mediaOf(t, b); m.Camera != nil || m.Width != 0 {
			t.Fatalf("garbage: %+v", m)
		}
	}
	// An I/O error is returned.
	b := lpt.JPEGWithTIFF(cameraTIFF(be, 1), 1)
	boom := errors.New("boom")
	if _, err := ExtractMediaInfo(&countingReader{data: b, size: int64(len(b)), fail: boom}, int64(len(b))); !errors.Is(err, boom) {
		t.Fatalf("want the read error, got %v", err)
	}
}

func mediaFixtures() [][]byte {
	be := binary.BigEndian
	return [][]byte{
		lpt.JPEGWithTIFF(cameraTIFF(be, 6), 1),
		cameraTIFF(binary.LittleEndian, 8),
		heicWithSize(4032, 3024, 1, cameraTIFF(be, 1)),
		movWithTracks(nil, tkhd(1, 3840, 2160, 0, 0xFFFF0000, 0x10000, 0)),
	}
}

func FuzzExtractMediaInfo(f *testing.F) {
	for _, b := range mediaFixtures() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		cr := &countingReader{data: b, size: int64(len(b))}
		m, err := ExtractMediaInfo(cr, int64(len(b)))
		if err != nil {
			t.Fatalf("error from a bytes reader: %v", err)
		}
		if cr.read > MaxReadBytes || cr.calls > maxReads {
			t.Fatalf("read %d bytes in %d calls", cr.read, cr.calls)
		}
		if m.Width < 0 || m.Width > MaxDimension || m.Height < 0 || m.Height > MaxDimension || (m.Width == 0) != (m.Height == 0) {
			t.Fatalf("size %d×%d", m.Width, m.Height)
		}
		if c := m.Camera; c != nil {
			if c.Empty() || c.Focal35mm < 0 || c.Focal35mm > MaxFocal35mm {
				t.Fatalf("camera %+v", c)
			}
			for _, s := range []string{c.Make, c.Model, c.Lens} {
				if s != cleanText(s) {
					t.Fatalf("unclean text %q", s)
				}
			}
		}
	})
}

// TestMediaInfoRealTools reads files written by real tools when they are
// installed: a HEIC by sips (macOS) and a rotated MP4 by ffmpeg. Content is
// a generated gradient; no real photo is used.
func TestMediaInfoRealTools(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 1200, 900))
	for i := range img.Pix {
		img.Pix[i] = byte(i / 97)
	}
	src := filepath.Join(dir, "src.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(p string) MediaInfo {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return mediaOf(t, b)
	}
	t.Run("sips heic", func(t *testing.T) {
		sips, err := exec.LookPath("sips")
		if err != nil {
			t.Skip("no sips")
		}
		out := filepath.Join(dir, "x.heic")
		if o, err := exec.Command(sips, "-s", "format", "heic", src, "--out", out).CombinedOutput(); err != nil {
			t.Skipf("sips cannot write HEIC here: %v %s", err, o)
		}
		if m := read(out); m.Width != 1200 || m.Height != 900 {
			t.Fatalf("heic %d×%d", m.Width, m.Height)
		}
	})
	t.Run("ffmpeg rotated mp4", func(t *testing.T) {
		ff, err := exec.LookPath("ffmpeg")
		if err != nil {
			t.Skip("no ffmpeg")
		}
		// Encode 320×240, then mark the track as turned by 90° without
		// re-encoding (the display matrix of the track header).
		plain, out := filepath.Join(dir, "p.mp4"), filepath.Join(dir, "x.mp4")
		if o, err := exec.Command(ff, "-loglevel", "error", "-f", "lavfi", "-i", "color=c=gray:s=320x240:d=1", "-c:v", "mpeg4", "-y", plain).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg: %v %s", err, o)
		}
		if m := read(plain); m.Width != 320 || m.Height != 240 {
			t.Fatalf("mp4 %d×%d", m.Width, m.Height)
		}
		if o, err := exec.Command(ff, "-loglevel", "error", "-display_rotation:v:0", "90", "-i", plain, "-c", "copy", "-y", out).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg without -display_rotation: %v %s", err, o)
		}
		m := read(out)
		if !(m.Width == 240 && m.Height == 320) {
			t.Fatalf("rotated mp4 %d×%d, want 240×320", m.Width, m.Height)
		}
	})
}
