package scanner

import "runtime"

// DiskKind is what kind of storage a library lives on.
type DiskKind string

const (
	DiskSSD     DiskKind = "SSD"
	DiskHDD     DiskKind = "hard drive"
	DiskNetwork DiskKind = "network drive"
	DiskUnknown DiskKind = "unknown drive"
)

// DetectDisk reports the kind of storage holding path. It never fails: when
// the OS can't tell (some USB enclosures hide it), it returns DiskUnknown.
func DetectDisk(path string) DiskKind {
	return detectDisk(path)
}

// WorkersFor is how many files to probe at once on a kind of storage.
//
//   - SSD: one per CPU, 2 to 8. Reads are random-access and cheap.
//   - Hard drive: 2. More makes the drive head jump between files, which is
//     slower than reading them one after another.
//   - Network drive: 6. Each read waits on the network, so overlapping them
//     hides the latency.
//   - Unknown: 4, a safe middle.
func WorkersFor(kind DiskKind) int {
	switch kind {
	case DiskSSD:
		return min(max(runtime.NumCPU(), 2), 8)
	case DiskHDD:
		return 2
	case DiskNetwork:
		return 6
	}
	return 4
}
