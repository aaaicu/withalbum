//go:build !darwin && !linux

package storage

// platformDiskUUID is not implemented here: the disk identity check is
// skipped (the volume marker and st_dev checks still apply).
func platformDiskUUID(string) (string, error) { return "", nil }
