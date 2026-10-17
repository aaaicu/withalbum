package storage

import "syscall"

// freeBytes uses statfs(2); on linux f_bavail counts f_frsize blocks.
// Field widths differ between architectures, hence the conversions.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	size := uint64(st.Frsize)
	if size == 0 {
		size = uint64(st.Bsize)
	}
	return uint64(st.Bavail) * size, nil
}
