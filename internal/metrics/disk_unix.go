//go:build !windows

package metrics

import "syscall"

// DiskUsage is the size of the volume holding path.
func DiskUsage(path string) (Disk, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, false
	}
	bs := uint64(st.Bsize)
	return Disk{Total: uint64(st.Blocks) * bs, Free: uint64(st.Bavail) * bs}, true
}
