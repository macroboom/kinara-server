//go:build !windows && !linux

package scanner

// detectDisk is not implemented on this OS yet (macOS could use
// `diskutil info`), so scans use the middle setting.
func detectDisk(path string) DiskKind { return DiskUnknown }
