//go:build windows

package metrics

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps nvidia-smi from flashing a console window every few
// seconds.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
