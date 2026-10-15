//go:build !windows

package metrics

import "os/exec"

func hideWindow(*exec.Cmd) {}
