package capturetime

import (
	"encoding/binary"
	"io"
)

// MaxDurationMS is the longest playing time Duration reports: one week.
// A movie header claiming more is not believed (unknown), so that a
// crafted file cannot make a client show an absurd length.
const MaxDurationMS = 7 * 24 * 3600 * 1000

// Duration reads the playing time of a QuickTime/MP4 movie, in whole
// milliseconds (rounded down), from its movie header: moov/mvhd duration
// divided by the mvhd timescale. A fragmented movie whose mvhd duration is
// zero is read from moov/mvex/mehd (fragment_duration, in the same
// timescale); without mehd its length is unknown (the fragments are not
// summed). It returns 0 for anything that is not a movie with a usable
// header: no moov, a zero timescale, a zero or "indefinite" (all ones)
// duration, less than a millisecond, more than MaxDurationMS. Like
// Extract it reads at most MaxReadBytes, never reads mdat, only reads, and
// the error is non-nil only when r itself failed.
func Duration(r io.ReaderAt, size int64) (int64, error) {
	rd := &reader{r: r, size: size, left: MaxReadBytes}
	ms := duration(rd)
	if rd.ioErr != nil {
		return 0, rd.ioErr
	}
	return ms, nil
}

func duration(rd *reader) int64 {
	if rd.size < 12 {
		return 0
	}
	head, err := rd.readAt(0, 12)
	if err != nil || !isTopLevelBox(string(head[4:8])) {
		return 0
	}
	_, moov := topBoxes(rd)
	if moov == nil {
		return 0
	}
	var mvhd, mvex *box
	rd.boxes(moov.body, moov.end, func(b box) bool {
		switch b.typ {
		case "mvhd":
			if mvhd == nil {
				mvhd = &b
			}
		case "mvex":
			if mvex == nil {
				mvex = &b
			}
		}
		return true
	})
	if mvhd == nil {
		return 0
	}
	timescale, d, ok := mvhdDuration(rd, *mvhd)
	if !ok {
		return 0
	}
	if d == 0 && mvex != nil {
		d, ok = mehdDuration(rd, *mvex)
		if !ok {
			return 0
		}
	}
	return toMillis(d, timescale)
}

// mvhdDuration reads the timescale and duration fields of a movie header
// (version 0: 32-bit times and duration, version 1: 64-bit). An
// "indefinite" duration (all ones) is not ok; a zero duration is
// returned as zero (a fragmented movie may carry its length in mehd).
func mvhdDuration(rd *reader, m box) (timescale uint32, d uint64, ok bool) {
	h, err := rd.readAt(m.body, min(m.end-m.body, 32))
	if err != nil || len(h) < 4 {
		return 0, 0, false
	}
	switch h[0] {
	case 0: // version, flags, creation u32, modification u32, timescale u32, duration u32
		if len(h) < 20 {
			return 0, 0, false
		}
		timescale, d = binary.BigEndian.Uint32(h[12:16]), uint64(binary.BigEndian.Uint32(h[16:20]))
		if d == 0xFFFFFFFF {
			return 0, 0, false
		}
	case 1: // version, flags, creation u64, modification u64, timescale u32, duration u64
		if len(h) < 32 {
			return 0, 0, false
		}
		timescale, d = binary.BigEndian.Uint32(h[20:24]), binary.BigEndian.Uint64(h[24:32])
		if d == ^uint64(0) {
			return 0, 0, false
		}
	default:
		return 0, 0, false
	}
	return timescale, d, timescale != 0
}

// mehdDuration reads fragment_duration from the movie extends header in
// mvex (version 0: u32, version 1: u64).
func mehdDuration(rd *reader, mvex box) (uint64, bool) {
	var mehd *box
	rd.boxes(mvex.body, mvex.end, func(b box) bool {
		if b.typ == "mehd" {
			mehd = &b
			return false
		}
		return true
	})
	if mehd == nil {
		return 0, false
	}
	h, err := rd.readAt(mehd.body, min(mehd.end-mehd.body, 12))
	if err != nil || len(h) < 8 {
		return 0, false
	}
	switch h[0] {
	case 0:
		d := uint64(binary.BigEndian.Uint32(h[4:8]))
		return d, d != 0xFFFFFFFF
	case 1:
		if len(h) < 12 {
			return 0, false
		}
		d := binary.BigEndian.Uint64(h[4:12])
		return d, d != ^uint64(0)
	}
	return 0, false
}

// toMillis converts d units of 1/timescale seconds to whole milliseconds
// without overflow; 0 when the result is out of (0, MaxDurationMS].
func toMillis(d uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	ts := uint64(timescale)
	secs := d / ts
	if secs > MaxDurationMS/1000 {
		return 0
	}
	ms := secs*1000 + (d%ts)*1000/ts // d%ts < 2^32, so the product fits
	if ms == 0 || ms > MaxDurationMS {
		return 0
	}
	return int64(ms)
}
