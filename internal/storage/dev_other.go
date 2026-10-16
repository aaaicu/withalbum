//go:build !unix

package storage

import "os"

// devOf is unsupported on this platform; Check then skips the st_dev comparison.
func devOf(os.FileInfo) (uint64, bool) { return 0, false }

// syncDir is a no-op where directories cannot be fsynced.
func syncDir(string) error { return nil }
