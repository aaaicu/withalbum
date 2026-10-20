package storage

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// byUUIDDir lists the file system UUIDs udev knows (/dev/disk/by-uuid/<uuid>
// -> ../../sdXN). A variable for tests.
var byUUIDDir = "/dev/disk/by-uuid"

// platformDiskUUID returns the UUID of the block device holding path:
// the /dev/disk/by-uuid entry whose device number is the st_dev of path.
// Best effort: without udev, or for file systems whose st_dev is not the
// block device (btrfs subvolumes, overlay, tmpfs, network file systems), it
// returns "" without an error.
func platformDiskUUID(path string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(byUUIDDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		var dev syscall.Stat_t
		if syscall.Stat(filepath.Join(byUUIDDir, e.Name()), &dev) != nil {
			continue
		}
		if dev.Mode&syscall.S_IFMT == syscall.S_IFBLK && uint64(dev.Rdev) == uint64(st.Dev) {
			return e.Name(), nil
		}
	}
	return "", nil
}
