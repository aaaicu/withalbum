package storage

import (
	"encoding/binary"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// attrList is struct attrlist of <sys/attr.h>.
type attrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

const (
	attrBitMapCount = 5          // ATTR_BIT_MAP_COUNT
	attrVolInfo     = 0x80000000 // ATTR_VOL_INFO
	attrVolUUID     = 0x00040000 // ATTR_VOL_UUID
)

// platformDiskUUID returns the volume UUID of the file system holding path
// (the "Volume UUID" diskutil shows) with getattrlist(2) ATTR_VOL_UUID. It
// returns "" without an error when the file system reports none.
func platformDiskUUID(path string) (string, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return "", err
	}
	al := attrList{bitmapCount: attrBitMapCount, volAttr: attrVolInfo | attrVolUUID}
	var buf [64]byte
	// #nosec G103 -- getattrlist(2) needs pointers to the attribute list and buffer, both alive for the call
	_, _, errno := syscall.Syscall6(syscall.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&al)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	if errno != 0 {
		return "", fmt.Errorf("getattrlist %s: %w", path, errno)
	}
	// The buffer starts with the uint32 length of the returned data; a
	// file system without ATTR_VOL_UUID returns just that length.
	n := binary.LittleEndian.Uint32(buf[:4])
	if n < 20 {
		return "", nil
	}
	return formatUUID(buf[4:20]), nil
}

// formatUUID writes 16 bytes as an upper-case 8-4-4-4-12 UUID; all zeros
// is "" (unknown).
func formatUUID(b []byte) string {
	zero := true
	for _, c := range b {
		if c != 0 {
			zero = false
		}
	}
	if zero {
		return ""
	}
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}
