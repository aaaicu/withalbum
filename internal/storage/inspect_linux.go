package storage

import "syscall"

// stRdonly is ST_RDONLY of statfs(2) f_flags.
const stRdonly = 0x1

// linuxFSNames maps statfs f_type magic numbers to names; network file
// systems are marked as not local.
var linuxFSNames = map[uint32]struct {
	name    string
	network bool
}{
	0xEF53: {"ext4", false}, 0x9123683E: {"btrfs", false}, 0x58465342: {"xfs", false}, 0x2011BAB0: {"exfat", false},
	0x4d44: {"vfat", false}, 0x5346544e: {"ntfs", false}, 0x65735546: {"fuse", false}, 0x01021994: {"tmpfs", false},
	0xF2F52010: {"f2fs", false}, 0x6969: {"nfs", true}, 0xFF534D42: {"cifs", true}, 0xFE534D42: {"smb2", true},
}

// Field widths differ between architectures, hence the conversions.
func statFS(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	size := uint64(st.Frsize)
	if size == 0 {
		size = uint64(st.Bsize)
	}
	info := FSInfo{Known: true, TotalBytes: uint64(st.Blocks) * size, FreeBytes: uint64(st.Bavail) * size,
		ReadOnly: uint64(st.Flags)&stRdonly != 0, Local: true}
	if fs, ok := linuxFSNames[uint32(st.Type)]; ok {
		info.Type, info.Local = fs.name, !fs.network
	}
	return info, nil
}
