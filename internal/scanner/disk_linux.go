//go:build linux

package scanner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Network file systems, by statfs magic number.
var networkFS = map[int64]bool{
	0x6969:     true, // NFS
	0xFF534D42: true, // CIFS / SMB
	0xFE534D42: true, // SMB2
	0x517B:     true, // SMB (old)
	0x65735546: true, // FUSE (sshfs, rclone and similar)
}

// detectDisk reads the kernel's "rotational" flag for the block device that
// holds path (/sys/dev/block/MAJOR:MINOR/queue/rotational).
func detectDisk(path string) DiskKind {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err == nil && networkFS[int64(fs.Type)] {
		return DiskNetwork
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return DiskUnknown
	}
	dev := uint64(st.Dev)
	major := (dev>>8)&0xfff | (dev>>32)&^0xfff
	minor := dev&0xff | (dev>>12)&^0xff
	if major == 0 {
		return DiskUnknown // virtual file systems (overlay, tmpfs)
	}
	sys, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", major, minor))
	if err != nil {
		return DiskUnknown
	}
	// A partition (sda1) keeps the flag on its parent disk (sda).
	for _, dir := range []string{sys, filepath.Dir(sys)} {
		b, err := os.ReadFile(filepath.Join(dir, "queue", "rotational"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(b)) {
		case "1":
			return DiskHDD
		case "0":
			return DiskSSD
		}
	}
	return DiskUnknown
}
