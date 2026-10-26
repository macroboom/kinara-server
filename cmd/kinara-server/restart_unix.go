//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// restartSelf replaces this process with a fresh copy of the server, same
// arguments and environment. The process ID stays, so launchd, systemd and
// Docker see the same server carry on. Files and sockets close on exec.
func restartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	// A moment for the port to be free again.
	time.Sleep(300 * time.Millisecond)
	return syscall.Exec(exe, os.Args, os.Environ())
}
