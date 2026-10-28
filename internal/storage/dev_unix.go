//go:build unix

package storage

import (
	"os"
	"syscall"
)

// devOf returns the st_dev of fi. Stat_t.Dev is int32 on darwin and uint64 on
// linux, so it is normalized to uint64; only equality is ever compared.
func devOf(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// syncDir fsyncs a directory so that a rename inside it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
