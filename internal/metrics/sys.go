// Package metrics measures the server for the manager's metrics page: the
// computer (processor, memory, disks, NVIDIA card), the traffic it serves,
// how its HTTP routes perform, and recent warnings. It uses only the
// standard library and the operating system's own interfaces.
package metrics

// cpuTimes are cumulative processor ticks since boot.
type cpuTimes struct {
	idle, total uint64
}

// cpuShare is the busy share (0 to 1) between two readings.
func cpuShare(a, b cpuTimes) float64 {
	dt := float64(b.total - a.total)
	if b.total <= a.total || dt == 0 {
		return 0
	}
	busy := dt - float64(b.idle-a.idle)
	if busy < 0 {
		busy = 0
	}
	return busy / dt
}

// Memory is the computer's memory in bytes.
type Memory struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

// Disk is one volume's size in bytes.
type Disk struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}
