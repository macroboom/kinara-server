//go:build !windows

package update

import "os"

// lockDir makes the updates folder this account's alone.
func lockDir(dir string) error { return os.Chmod(dir, 0o700) }
