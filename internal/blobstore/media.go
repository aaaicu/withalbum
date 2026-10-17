package blobstore

import (
	"bytes"
	"encoding/binary"
	"net/http"
)

// SniffMIME returns the media type of a file from its first bytes (up to
// 512 are used). It is http.DetectContentType, plus the ISO base media
// file types that it reports as application/octet-stream or as plain MP4:
// HEIC/HEIF/AVIF stills and QuickTime/MP4/3GPP movies, told apart by the
// brands of their "ftyp" box.
func SniffMIME(head []byte) string {
	if len(head) > 512 {
		head = head[:512]
	}
	if t := ftypMIME(head); t != "" {
		return t
	}
	return http.DetectContentType(head)
}

// ftypMIME classifies an ISO BMFF "ftyp" box at the start of head, or
// returns "".
func ftypMIME(head []byte) string {
	if len(head) < 16 || !bytes.Equal(head[4:8], []byte("ftyp")) {
		return ""
	}
	size := int(binary.BigEndian.Uint32(head[:4]))
	if size < 16 || size%4 != 0 {
		return ""
	}
	end := min(size, len(head))
	brands := [][]byte{head[8:12]} // major brand, then compatible brands
	for i := 16; i+4 <= end; i += 4 {
		brands = append(brands, head[i:i+4])
	}
	has := func(names ...string) bool {
		for _, b := range brands {
			for _, n := range names {
				if string(b) == n {
					return true
				}
			}
		}
		return false
	}
	major := string(head[8:12])
	switch {
	case major == "avif" || major == "avis":
		return "image/avif"
	case has("heic", "heix", "heim", "heis", "hevc", "hevx"):
		return "image/heic"
	case has("mif1", "msf1"):
		return "image/heif"
	case major == "qt  ":
		return "video/quicktime"
	case has("3gp4", "3gp5", "3gp6", "3gg6", "3ge6"):
		return "video/3gpp"
	case has("isom", "iso2", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "M4V ", "M4VH", "M4VP", "dash", "qt  "):
		return "video/mp4"
	}
	return ""
}
