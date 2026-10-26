//go:build !windows && !darwin

package update

import (
	"context"
	"errors"
)

// Linux installs are updated by apt or docker, not by Kinara.
func canLaunch(kind string) bool { return false }

func verify(ctx context.Context, kind, path string) error { return nil }

func launch(kind, path, version, dir string, ended func(error)) error {
	return errors.New("this install is updated with " + how(kind))
}
