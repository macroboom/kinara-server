//go:build windows

package main

import "os"

// exitRestart is the exit code that asks for a restart: the tray (sign-in
// mode) starts the server again at once, without saying it stopped.
const exitRestart = 75

// restartSelf ends the process with exitRestart. As the Windows service
// this isn't reached: serviceMain reports errRestart as a failure, and the
// service's recovery settings start it again.
func restartSelf() error {
	os.Exit(exitRestart)
	return nil
}
