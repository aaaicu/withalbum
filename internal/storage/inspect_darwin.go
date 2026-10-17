package storage

import "syscall"

// statfs(2) flags of <sys/mount.h>.
const (
	mntReadOnly = 0x00000001 // MNT_RDONLY
	mntLocal    = 0x00001000 // MNT_LOCAL
)

func statFS(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return FSInfo{Known: true, Type: string(name), TotalBytes: st.Blocks * uint64(st.Bsize), FreeBytes: st.Bavail * uint64(st.Bsize),
		ReadOnly: st.Flags&mntReadOnly != 0, Local: st.Flags&mntLocal != 0}, nil
}
