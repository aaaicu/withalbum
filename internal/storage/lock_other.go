//go:build !darwin && !linux

package storage

import "os"

// lockFile is a no-op where flock(2) is not available.
func lockFile(*os.File) error { return nil }
