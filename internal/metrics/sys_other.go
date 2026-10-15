//go:build !linux && !windows

package metrics

// Processor and memory readings aren't wired up on this system yet (macOS
// would need cgo or sysctl parsing); the page shows them as unavailable.

func readCPU() (cpuTimes, bool) { return cpuTimes{}, false }

func readMemory() (Memory, bool) { return Memory{}, false }
