//go:build !windows

// Package fsacl makes folders private to the account the server runs as.
package fsacl

import "os"

// Private makes a folder this account's alone.
func Private(dir string) error { return os.Chmod(dir, 0o700) }
