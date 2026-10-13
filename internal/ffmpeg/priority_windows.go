//go:build windows

package ffmpeg

import (
	"os/exec"
	"syscall"
)

const belowNormalPriorityClass = 0x00004000

// StartLow starts a background ffmpeg run (thumbnails, intro detection) at
// below-normal priority, so it yields the CPU to playback and transcoding.
func StartLow(cmd *exec.Cmd) error {
	HideWindow(cmd)
	cmd.SysProcAttr.CreationFlags |= belowNormalPriorityClass
	return cmd.Start()
}

// HideWindow stops a console window flashing up for each run.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
