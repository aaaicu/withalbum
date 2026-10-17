//go:build !darwin && !linux

package storage

func freeBytes(string) (uint64, error) { return 0, ErrFreeSpaceUnsupported }
