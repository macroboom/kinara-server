//go:build !windows

package main

import "errors"

// runService is Windows only: elsewhere use systemd, launchd or Docker
// with "kinora-server serve".
func runService(args []string) error {
	return errors.New("the service command is for Windows; use: kinora-server serve")
}
