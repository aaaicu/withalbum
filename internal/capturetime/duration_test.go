package capturetime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// mvhdV0 is a version 0 movie header with the given timescale and
// duration.
func mvhdV0(timescale, d uint32) []byte {
	return mkbox("mvhd", []byte{0, 0, 0, 0}, be32(mac(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))), be32(0), be32(timescale), be32(d), make([]byte, 80))
}

// mvhdV1 is a version 1 movie header (64-bit times and duration).
func mvhdV1(timescale uint32, d uint64) []byte {
	return mkbox("mvhd", []byte{1, 0, 0, 0}, make([]byte, 16), be32(timescale), binary.BigEndian.AppendUint64(nil, d), make([]byte, 80))
}

func mehd(version byte, d uint64) []byte {
	if version == 1 {
		return mkbox("mehd", []byte{1, 0, 0, 0}, binary.BigEndian.AppendUint64(nil, d))
	}
	return mkbox("mehd", []byte{0, 0, 0, 0}, be32(uint32(d)))
}

func durationBytes(t *testing.T, b []byte) int64 {
	t.Helper()
	ms, err := Duration(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("Duration: %v", err)
	}
	return ms
}

func TestDuration(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want int64
	}{
		{"v0 600", buildMOV(mvhdV0(600, 8400)), 14000},
		{"v0 rounds down", buildMOV(mvhdV0(600, 8401)), 14001},
		{"v0 sub-ms remainder", buildMOV(mvhdV0(90000, 1260089)), 14000},
		{"v1 1000", buildMOV(mvhdV1(1000, 61_234)), 61234},
		{"with creation date meta", buildMOV(mvhdV0(600, 600), qtMeta("2026-10-19T14:03:22+0900", false)), 1000},
		{"moov before mdat", append(mkbox("ftyp", []byte("isom"), be32(0)), mkbox("moov", mvhdV0(30000, 30030))...), 1001},
		{"fragmented, mehd v0", buildMOV(mvhdV0(1000, 0), mkbox("mvex", mehd(0, 5500), mkbox("trex", make([]byte, 24)))), 5500},
		{"fragmented, mehd v1", buildMOV(mvhdV1(48000, 0), mkbox("mvex", mehd(1, 48000*3))), 3000},
		{"fragmented without mehd", buildMOV(mvhdV0(1000, 0), mkbox("mvex", mkbox("trex", make([]byte, 24)))), 0},
		{"zero duration", buildMOV(mvhdV0(600, 0)), 0},
		{"zero timescale", buildMOV(mvhdV0(0, 600)), 0},
		{"indefinite v0", buildMOV(mvhdV0(600, 0xFFFFFFFF)), 0},
		{"indefinite v1", buildMOV(mvhdV1(600, ^uint64(0))), 0},
		{"less than a millisecond", buildMOV(mvhdV0(1_000_000, 999)), 0},
		{"one week", buildMOV(mvhdV1(1, MaxDurationMS/1000)), MaxDurationMS},
		{"over a week", buildMOV(mvhdV1(1, MaxDurationMS/1000+1)), 0},
		{"huge v1", buildMOV(mvhdV1(1, 1<<63)), 0},
		{"unknown version", buildMOV(mkbox("mvhd", []byte{2, 0, 0, 0}, make([]byte, 96))), 0},
		{"short mvhd", buildMOV(mkbox("mvhd", []byte{0, 0, 0, 0}, be32(1), be32(2))), 0},
		{"no mvhd", buildMOV(mkbox("trak")), 0},
		{"no moov", bytes.Join([][]byte{mkbox("ftyp", []byte("isom"), be32(0)), mkbox("mdat", make([]byte, 50))}, nil), 0},
		{"mvhd outside moov", append(mkbox("ftyp", []byte("isom"), be32(0)), mvhdV0(600, 6000)...), 0},
		{"HEIF still", buildHEIF(exifItem(exifTIFF("2026:10:19 14:03:22", "+09:00")), false, 2), 0},
		{"JPEG", buildJPEG(exifTIFF("2026:10:19 14:03:22", "+09:00")), 0},
		{"empty", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := durationBytes(t, c.b); got != c.want {
				t.Fatalf("Duration = %d, want %d", got, c.want)
			}
		})
	}
}

// The movie after a 6 GiB mdat is read without reading the mdat.
func TestDurationLargeMdat(t *testing.T) {
	const mdatSize = 6 << 30
	head := mkbox("ftyp", []byte("isom"), be32(0))
	mdat := append([]byte{0, 0, 0, 1}, "mdat"...)
	mdat = binary.BigEndian.AppendUint64(mdat, mdatSize)
	moov := mkbox("moov", mvhdV1(600, 600*125))
	moovOff := int64(len(head)) + mdatSize
	r := &sparseReader{parts: map[int64][]byte{0: append(head, mdat...), moovOff: moov}, size: moovOff + int64(len(moov))}
	got, err := Duration(r, r.size)
	if err != nil || got != 125000 {
		t.Fatalf("got %d %v", got, err)
	}
	if r.read > 4096 {
		t.Fatalf("read %d bytes", r.read)
	}
}

func TestDurationLimitsAndErrors(t *testing.T) {
	var boxes []byte
	boxes = append(boxes, mkbox("ftyp", []byte("isom"), be32(0))...)
	for range 100000 {
		boxes = append(boxes, mkbox("free")...)
	}
	boxes = append(boxes, mkbox("moov", mvhdV0(600, 6000))...)
	r := &countingReader{data: boxes, size: int64(len(boxes))}
	if got, err := Duration(r, r.size); err != nil || got != 0 {
		t.Fatalf("got %d %v; moov is beyond the box limit", got, err)
	}
	if r.read > MaxReadBytes || r.calls > maxReads {
		t.Fatalf("read %d bytes in %d calls", r.read, r.calls)
	}

	boom := errors.New("disk on fire")
	if _, err := Duration(&countingReader{size: 1000, fail: boom}, 1000); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the I/O error", err)
	}
	for _, f := range durationFixtures() {
		for n := range len(f) {
			_ = durationBytes(t, f[:n])
		}
	}
}

func durationFixtures() [][]byte {
	return [][]byte{
		buildMOV(mvhdV0(600, 8400), qtMeta("2026-10-19T14:03:22+0900", false)),
		buildMOV(mvhdV1(90000, 900000)),
		buildMOV(mvhdV0(1000, 0), mkbox("mvex", mehd(1, 5500))),
	}
}

func FuzzDuration(f *testing.F) {
	for _, b := range append(durationFixtures(), fixtures()...) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := &countingReader{data: b, size: int64(len(b))}
		ms, err := Duration(r, r.size)
		if err != nil {
			t.Fatalf("no I/O error expected: %v", err)
		}
		if ms < 0 || ms > MaxDurationMS {
			t.Fatalf("duration %d out of range", ms)
		}
		if r.read > MaxReadBytes || r.calls > maxReads {
			t.Fatalf("read %d bytes in %d calls", r.read, r.calls)
		}
	})
}
