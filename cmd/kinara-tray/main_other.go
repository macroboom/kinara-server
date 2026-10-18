//go:build !windows

// kinara-tray is the Windows notification-area icon; there is nothing to
// build elsewhere.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kinara-tray is for Windows")
	os.Exit(1)
}
