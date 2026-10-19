package capturetime

import (
	"bytes"
	"io"
)

// Bounded metadata access for other readers of originals (internal/livephoto
// reads the Live Photo content identifier through these). Each call has
// the limits of Extract: at most MaxReadBytes through at most maxReads
// ReadAt calls, metadata blocks cut at 256 KiB, counted boxes, segments
// and items. They only read.

// ExifTIFF returns the TIFF structure of the original's EXIF metadata: the
// first EXIF APP1 segment of a JPEG, the Exif item of a HEIF/HEIC file
// (iinf + iloc), or the start of a TIFF file. It is at most 256 KiB, and
// nil when the original has none that can be read. The error is non-nil
// only when r itself failed.
func ExifTIFF(r io.ReaderAt, size int64) ([]byte, error) {
	rd := &reader{r: r, size: size, left: MaxReadBytes}
	b := findExifTIFF(rd)
	if rd.ioErr != nil {
		return nil, rd.ioErr
	}
	return b, nil
}

func findExifTIFF(rd *reader) []byte {
	if rd.size < 12 {
		return nil
	}
	head, err := rd.readAt(0, 12)
	if err != nil {
		return nil
	}
	switch {
	case head[0] == 0xFF && head[1] == 0xD8:
		var out []byte
		jpegExif(rd, func(tiff []byte) bool {
			out = tiff
			return false
		})
		return out
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		b, err := rd.readAt(0, min(rd.size, maxBlock))
		if err != nil {
			return nil
		}
		return b
	case isTopLevelBox(string(head[4:8])):
		metaBox, _ := topBoxes(rd)
		if metaBox == nil {
			return nil
		}
		for _, tiff := range heifExif(rd, *metaBox) {
			if len(tiff) >= 8 && (string(tiff[:2]) == "II" || string(tiff[:2]) == "MM") {
				return tiff
			}
		}
	}
	return nil
}

// QuickTimeString returns the UTF-8 value of the QuickTime metadata item
// key in the movie's moov/meta box (keys + ilst), as stored, or "" when
// the movie has none (or one longer than maxLen bytes, at most 256). The
// error is non-nil only when r itself failed.
func QuickTimeString(r io.ReaderAt, size int64, key string, maxLen int) (string, error) {
	rd := &reader{r: r, size: size, left: MaxReadBytes}
	v := quickTimeString(rd, key, int64(maxLen))
	if rd.ioErr != nil {
		return "", rd.ioErr
	}
	return v, nil
}

func quickTimeString(rd *reader, key string, maxLen int64) string {
	if rd.size < 12 || maxLen <= 0 {
		return ""
	}
	head, err := rd.readAt(0, 12)
	if err != nil || !isTopLevelBox(string(head[4:8])) {
		return ""
	}
	_, moov := topBoxes(rd)
	if moov == nil {
		return ""
	}
	var metaBox *box
	rd.boxes(moov.body, moov.end, func(b box) bool {
		if b.typ == "meta" {
			metaBox = &b
			return false
		}
		return true
	})
	if metaBox == nil {
		return ""
	}
	v, _ := quickTimeValue(rd, *metaBox, key, maxLen)
	return v
}
