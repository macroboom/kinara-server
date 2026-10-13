//go:build !windows

package ffmpeg

import (
	"os/exec"
	"syscall"
)

// StartLow starts a background ffmpeg run (thumbnails, intro detection) at
// a lower priority, so it yields the CPU to playback and transcoding.
func StartLow(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 10)
	return nil
}

// HideWindow does nothing outside Windows.
func HideWindow(cmd *exec.Cmd) {}
