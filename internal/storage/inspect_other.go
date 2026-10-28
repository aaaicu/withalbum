//go:build !darwin && !linux

package storage

func statFS(string) (FSInfo, error) { return FSInfo{}, ErrInspectUnsupported }
